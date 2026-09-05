package drive

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/auth"
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

	// cache is optional; nil means block downloads never hit disk.
	cache *BlockCache
}

// SetBlockCache attaches an on-disk block cache that OpenFile's Files consult before
// downloading a block over the network.
func (c *Client) SetBlockCache(bc *BlockCache) {
	c.cache = bc
}

// Node is a decrypted file or folder in the drive tree.
type Node struct {
	Link    proton.Link
	Name    string
	KR      *crypto.KeyRing
	Size    int64
	ModTime time.Time
}

// IsDir reports whether the node is a folder.
func (n *Node) IsDir() bool {
	return n.Link.Type == proton.LinkTypeFolder
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
		api:       api,
		addrKR:    keys.Merged,
		signKR:    signKR,
		signEmail: signEmail,
		addressID: share.AddressID,
		shareID:   shareID,
		volumeID:  volumeID,
	}
	size, modTime := c.resolveFileAttrs(rootLink, rootKR)

	root := &Node{Link: rootLink, Name: "/", KR: rootKR, Size: size, ModTime: modTime}

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

// Children lists the active children of parent, decrypting their names and keyrings.
// Entries that fail to decrypt are skipped rather than failing the whole listing.
func (c *Client) Children(ctx context.Context, parent *Node) ([]*Node, error) {
	links, err := c.api.ListChildren(ctx, c.shareID, parent.Link.LinkID, false)
	if err != nil {
		return nil, err
	}

	nodes := make([]*Node, 0, len(links))
	for _, link := range links {
		if link.State != proton.LinkStateActive {
			continue
		}

		name, err := link.GetName(parent.KR, c.addrKR)
		if err != nil {
			continue
		}

		kr, err := link.GetKeyRing(parent.KR, c.addrKR)
		if err != nil {
			continue
		}

		size, modTime := c.resolveFileAttrs(link, kr)
		nodes = append(nodes, &Node{Link: link, Name: name, KR: kr, Size: size, ModTime: modTime})
	}

	return nodes, nil
}

// resolveFileAttrs returns the best-known size and modification time for link, preferring
// the values in its decrypted XAttr (real plaintext size) over the encrypted-size Link.Size.
func (c *Client) resolveFileAttrs(link proton.Link, kr *crypto.KeyRing) (int64, time.Time) {
	size := link.Size
	modTime := time.Unix(link.ModifyTime, 0)

	if link.Type != proton.LinkTypeFile || link.FileProperties == nil {
		return size, modTime
	}

	common, err := decryptXAttr(c.addrKR, kr, link.FileProperties.ActiveRevision.XAttr)
	if err != nil {
		return size, modTime
	}

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
func decryptXAttr(addrKR, nodeKR *crypto.KeyRing, armored string) (proton.RevisionXAttrCommon, error) {
	if armored == "" {
		return proton.RevisionXAttrCommon{}, nil
	}

	msg, err := crypto.NewPGPMessageFromArmored(armored)
	if err != nil {
		return proton.RevisionXAttrCommon{}, err
	}

	dec, err := nodeKR.Decrypt(msg, addrKR, crypto.GetUnixTime())
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
