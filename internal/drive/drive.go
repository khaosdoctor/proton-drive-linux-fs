package drive

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/auth"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/logx"
)

// Client is a thin wrapper around the Proton API client scoped to a single drive share.
type Client struct {
	api    *proton.Client
	addrKR *crypto.KeyRing

	// signKR and signEmail are the keyring and address used to sign writes (name/passphrase/
	// block/manifest signatures); addressID is that same address's ID, required by block upload
	// requests.
	signKR    *crypto.KeyRing
	signEmail string
	addressID string

	shareID  string
	volumeID string

	// tokenSource returns the current uid/access token for putJSON's direct requests, kept in
	// sync with library-triggered refreshes by auth.Session.
	tokenSource func() (uid, access string)

	// apiURL is putJSON's request base; auth.APIURL in production, a test server's URL in tests.
	apiURL string

	// cache is optional; nil means block downloads never hit disk.
	cache *BlockCache

	// largeFile is the file-size threshold above which blocks bypass the on-disk cache.
	// <= 0 means no threshold.
	largeFile int64

	// transfers counts uploads and downloads in flight; see activity.go.
	transfers atomic.Int64

	// current holds the in-flight transfers themselves (uint64 ID -> *Transfer), for the tray
	// and the local API to show what is moving right now; see activity.go.
	current sync.Map

	// downloads bounds concurrent block downloads; nil means unbounded.
	downloads chan struct{}
}

// SetMaxDownloads caps how many block downloads run at once. n <= 0 leaves them unbounded.
func (c *Client) SetMaxDownloads(n int) {
	if n <= 0 {
		c.downloads = nil
		return
	}
	c.downloads = make(chan struct{}, n)
}

// acquireDownload waits for a download slot and returns the func that gives it back.
func (c *Client) acquireDownload(ctx context.Context) (func(), error) {
	if c.downloads == nil {
		return func() {}, nil
	}

	select {
	case c.downloads <- struct{}{}:
		return func() { <-c.downloads }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SetBlockCache attaches an on-disk block cache that OpenFile's Files consult before
// downloading a block over the network. Files larger than largeFile skip the cache
// entirely; largeFile <= 0 means no threshold.
func (c *Client) SetBlockCache(bc *BlockCache, largeFile int64) {
	c.cache = bc
	c.largeFile = largeFile
}

// LargeFileLimit returns the file-size threshold above which a file counts as large, the same
// one that keeps big files out of the on-disk block cache. <= 0 means no threshold.
func (c *Client) LargeFileLimit() int64 {
	return c.largeFile
}

// CachedChildren rebuilds parent's children from the persisted listing cache, without any network
// call: it unlocks parent's own keyring (already resolved, or a fast in-memory unlock) but leaves
// each child's keyring lazy, same as a normal listing. The second result is false when nothing is
// cached, the cache is disabled, or parent's keyring can't be unlocked.
func (c *Client) CachedChildren(parent *Node) ([]*Node, bool) {
	if c == nil || c.cache == nil {
		return nil, false
	}

	entries, ok := c.cache.GetListing(parent.Link.LinkID)
	if !ok {
		return nil, false
	}

	parentKR, err := parent.Keyring()
	if err != nil {
		return nil, false
	}

	nodes := make([]*Node, 0, len(entries))
	for _, e := range entries {
		var link proton.Link
		if err := json.Unmarshal(e.Link, &link); err != nil {
			continue
		}
		nodes = append(nodes, c.newNode(link, e.Name, parentKR))
	}

	return nodes, true
}

// CacheListing persists parent's current children so a future cold start can serve them without a
// network round-trip (see CachedChildren). A no-op when the cache is disabled.
func (c *Client) CacheListing(parent *Node, children []*Node) {
	if c == nil || c.cache == nil {
		return
	}

	entries := make([]ListingEntry, 0, len(children))
	for _, n := range children {
		raw, err := json.Marshal(n.Link)
		if err != nil {
			continue
		}
		entries = append(entries, ListingEntry{Name: n.Name, Link: raw})
	}

	c.cache.PutListing(parent.Link.LinkID, entries)
}

// Node is a decrypted file or folder in the drive tree. Its name is decrypted when the node is
// built; the node keyring and the attributes that need it are resolved on first use.
type Node struct {
	Link proton.Link
	Name string

	client   *Client
	parentKR *crypto.KeyRing

	krMu sync.Mutex
	kr   *crypto.KeyRing

	attrMu     sync.Mutex
	attrsKnown bool
	size       int64
	modTime    time.Time
}

// IsDir reports whether the node is a folder.
func (n *Node) IsDir() bool {
	return n.Link.Type == proton.LinkTypeFolder
}

// getKeyRing unlocks a link's own keyring; overridden in tests to simulate a transient failure
// without a real PGP key.
var getKeyRing = func(link proton.Link, parentKR, addrKR *crypto.KeyRing) (*crypto.KeyRing, error) {
	return link.GetKeyRing(parentKR, addrKR)
}

// Keyring unlocks this node's own keyring and caches it on success. Unlocking is deferred because
// a listing of 10k entries would otherwise spend tens of seconds of CPU unlocking a key for every
// child, and almost none of those children are opened. A failure is never cached: it retries on
// the next call instead of poisoning the node until its listing expires.
func (n *Node) Keyring() (*crypto.KeyRing, error) {
	n.krMu.Lock()
	defer n.krMu.Unlock()

	if n.kr != nil {
		return n.kr, nil
	}

	start := time.Now()
	kr, err := getKeyRing(n.Link, n.parentKR, n.client.addrKR)
	if err != nil {
		return nil, err
	}
	slog.Debug("keyring unlocked", "link", n.Link.LinkID, logx.Elapsed(start))

	n.kr = kr
	return n.kr, nil
}

// Size is the file's plaintext size when it is known, and the encrypted Link size until the node
// has been resolved; see ResolveAttrs.
func (n *Node) Size() int64 {
	n.attrMu.Lock()
	defer n.attrMu.Unlock()
	return n.size
}

// ModTime is the modification time, refined by ResolveAttrs the same way Size is.
func (n *Node) ModTime() time.Time {
	n.attrMu.Lock()
	defer n.attrMu.Unlock()
	return n.modTime
}

// inheritAttrs carries src's already-resolved size and mtime over to n, so a move does not throw
// away attributes that cost an XAttr decrypt to learn.
func (n *Node) inheritAttrs(src *Node) {
	src.attrMu.Lock()
	size, modTime, known := src.size, src.modTime, src.attrsKnown
	src.attrMu.Unlock()

	n.attrMu.Lock()
	n.size, n.modTime, n.attrsKnown = size, modTime, known
	n.attrMu.Unlock()
}

// ResolveAttrs replaces the Link's encrypted size and server mtime with the real values from the
// node's XAttr, unlocking its keyring to do so. Callers do this for a single node being looked
// up, never for a whole listing.
func (n *Node) ResolveAttrs() {
	n.attrMu.Lock()
	known := n.attrsKnown
	n.attrMu.Unlock()

	if known || n.Link.Type != proton.LinkTypeFile || n.Link.FileProperties == nil {
		return
	}

	kr, err := n.Keyring()
	if err != nil {
		slog.Warn("resolving keyring failed, keeping encrypted size", "link", n.Link.LinkID, "size", n.Link.Size, "err", err)
		return
	}

	size, modTime := n.client.resolveFileAttrs(n.Link, kr)

	n.attrMu.Lock()
	n.size = size
	n.modTime = modTime
	n.attrsKnown = true
	n.attrMu.Unlock()
}

// Open resolves the primary share and its root node, returning a Client ready to list children.
func Open(ctx context.Context, api *proton.Client, keys *auth.Keys) (*Client, *Node, error) {
	shares, err := api.ListShares(ctx, false)
	if err != nil {
		return nil, nil, err
	}

	var shareID, volumeID string
	for _, s := range shares {
		if s.Flags == proton.PrimaryShare {
			shareID = s.ShareID
			volumeID = s.VolumeID
			break
		}
	}
	if shareID == "" {
		return nil, nil, errors.New("no primary drive share found")
	}

	share, err := api.GetShare(ctx, shareID)
	if err != nil {
		return nil, nil, err
	}

	signEmail, signKR, err := resolveSigner(keys, share.AddressID)
	if err != nil {
		return nil, nil, err
	}

	shareKR, err := share.GetKeyRing(keys.Merged)
	if err != nil {
		return nil, nil, err
	}

	rootLink, err := api.GetLink(ctx, shareID, share.LinkID)
	if err != nil {
		return nil, nil, err
	}

	rootKR, err := rootLink.GetKeyRing(shareKR, keys.Merged)
	if err != nil {
		return nil, nil, err
	}

	c := &Client{
		api:         api,
		addrKR:      keys.Merged,
		signKR:      signKR,
		signEmail:   signEmail,
		addressID:   share.AddressID,
		shareID:     shareID,
		volumeID:    volumeID,
		tokenSource: keys.TokenSource,
		apiURL:      auth.APIURL,
	}
	size, modTime := c.resolveFileAttrs(rootLink, rootKR)

	root := &Node{Link: rootLink, Name: "/", client: c, kr: rootKR, size: size, modTime: modTime, attrsKnown: true}

	return c, root, nil
}

// resolveSigner finds the address and its unlocked keyring matching a share's AddressID. Writes
// are signed as this address, as the Proton clients do.
func resolveSigner(keys *auth.Keys, addressID string) (string, *crypto.KeyRing, error) {
	for _, a := range keys.Addresses {
		if a.ID != addressID {
			continue
		}

		kr, ok := keys.ByAddressID[addressID]
		if !ok {
			return "", nil, errors.New("no unlocked keyring for share's address")
		}

		return a.Email, kr, nil
	}

	return "", nil, errors.New("no address found for share's address id")
}

// Children lists the active children of parent, decrypting their names. Keyrings, and the
// attributes that need them, stay locked until something asks for that one child.
// Entries whose name fails to decrypt are skipped rather than failing the whole listing.
func (c *Client) Children(ctx context.Context, parent *Node) ([]*Node, error) {
	start := time.Now()

	parentKR, err := parent.Keyring()
	if err != nil {
		return nil, err
	}

	links, err := c.api.ListChildren(ctx, c.shareID, parent.Link.LinkID, false)
	if err != nil {
		return nil, err
	}

	active := make([]proton.Link, 0, len(links))
	for _, link := range links {
		if link.State != proton.LinkStateActive {
			continue
		}
		active = append(active, link)
	}

	nodes := c.decryptNames(active, parentKR)
	slog.Debug("listed children", "parent", parent.Link.LinkID, "count", len(nodes), logx.Elapsed(start))

	return nodes, nil
}

// decryptNames builds one node per link, decrypting the names on up to NumCPU goroutines: name
// decryption is the whole per-child CPU cost once keyrings are lazy, and a 10k-entry folder
// spends it all in one listing. The result keeps the API's order.
func (c *Client) decryptNames(links []proton.Link, parentKR *crypto.KeyRing) []*Node {
	nodes := make([]*Node, len(links))

	workers := runtime.NumCPU()
	if workers > len(links) {
		workers = len(links)
	}

	var wg sync.WaitGroup
	indices := make(chan int)

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range indices {
				name, err := links[i].GetName(parentKR, c.addrKR)
				if err != nil {
					continue
				}
				nodes[i] = c.newNode(links[i], name, parentKR)
			}
		}()
	}

	for i := range links {
		indices <- i
	}
	close(indices)
	wg.Wait()

	out := make([]*Node, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		out = append(out, n)
	}

	return out
}

// newNode wraps one link whose name is already decrypted. Size and mtime start from the link
// itself (Link.Size is the encrypted size, a little larger than the real one) and are refined
// by ResolveAttrs when this single node is looked up.
func (c *Client) newNode(link proton.Link, name string, parentKR *crypto.KeyRing) *Node {
	return &Node{
		Link:     link,
		Name:     name,
		client:   c,
		parentKR: parentKR,
		size:     link.Size,
		modTime:  time.Unix(link.ModifyTime, 0),
	}
}

// nodeFromLink decrypts link's name under parent and returns it as a node, for a link the API
// just handed back from a write.
func (c *Client) nodeFromLink(link proton.Link, parent *Node) (*Node, error) {
	parentKR, err := parent.Keyring()
	if err != nil {
		return nil, err
	}

	name, err := link.GetName(parentKR, c.addrKR)
	if err != nil {
		return nil, err
	}

	return c.newNode(link, name, parentKR), nil
}

// fetchNode reads one link back from the API and decrypts it under parent, so a caller that just
// created or replaced it gets a node without re-listing the whole parent folder.
func (c *Client) fetchNode(ctx context.Context, linkID string, parent *Node) (*Node, error) {
	link, err := c.api.GetLink(ctx, c.shareID, linkID)
	if err != nil {
		return nil, err
	}

	return c.nodeFromLink(link, parent)
}

// resolveFileAttrs returns the best-known size and modification time for link, preferring
// the values in its decrypted XAttr (real plaintext size) over the encrypted-size Link.Size.
func (c *Client) resolveFileAttrs(link proton.Link, kr *crypto.KeyRing) (int64, time.Time) {
	size := link.Size
	modTime := time.Unix(link.ModifyTime, 0)

	if link.Type != proton.LinkTypeFile || link.FileProperties == nil {
		return size, modTime
	}

	start := time.Now()
	common, err := decryptXAttr(kr, link.FileProperties.ActiveRevision.XAttr)
	if err != nil {
		slog.Warn("resolving xattr failed, using encrypted size", "link", link.LinkID, "size", link.Size, "err", err)
		return size, modTime
	}
	slog.Debug("xattr resolved", "link", link.LinkID, logx.Elapsed(start))

	if common.Size > 0 {
		size = common.Size
	}

	if t, err := time.Parse(time.RFC3339, common.ModificationTime); err == nil {
		modTime = t
	}

	return size, modTime
}

// decryptXAttr decrypts a Link's armored XAttr blob (encrypted to the node key, signed by the
// address key) and parses its JSON payload.
func decryptXAttr(nodeKR *crypto.KeyRing, armored string) (proton.RevisionXAttrCommon, error) {
	if armored == "" {
		return proton.RevisionXAttrCommon{}, nil
	}

	msg, err := crypto.NewPGPMessageFromArmored(armored)
	if err != nil {
		return proton.RevisionXAttrCommon{}, err
	}

	// ponytail: XAttr is metadata (size, mtime); skip signature verification so a foreign signer does not hide the real size
	dec, err := nodeKR.Decrypt(msg, nil, 0)
	if err != nil {
		return proton.RevisionXAttrCommon{}, err
	}

	return parseXAttr(dec.GetBinary())
}

// parseXAttr unmarshals a decrypted XAttr JSON payload. Split out from decryptXAttr so it can
// be unit-tested without real PGP keys.
func parseXAttr(plaintext []byte) (proton.RevisionXAttrCommon, error) {
	var doc proton.RevisionXAttr
	if err := json.Unmarshal(plaintext, &doc); err != nil {
		return proton.RevisionXAttrCommon{}, err
	}

	return doc.Common, nil
}
