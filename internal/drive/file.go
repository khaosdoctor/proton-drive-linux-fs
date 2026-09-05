package drive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
	proton "github.com/henrybear327/go-proton-api"
)

const blockSize = 4 << 20 // 4 MiB, per Proton Drive's block layout

// maxCachedBlocks bounds the per-file decrypted block cache.
// ponytail: FIFO eviction over 4 slots; an LRU only matters if read patterns stop being sequential.
const maxCachedBlocks = 4

// File is an open, readable handle to a file's active revision.
type File struct {
	client     *Client
	sessionKey *crypto.SessionKey
	blocks     map[int]proton.Block
	size       int64
	linkID     string
	revID      string

	mu       sync.Mutex
	cache    map[int][]byte
	cacheLRU []int
}

// OpenFile fetches the session key and full block list for n's active revision.
// ponytail: fetches the whole block list at open; paginate with GetRevision if huge files are slow.
func (c *Client) OpenFile(ctx context.Context, n *Node) (*File, error) {
	if n.Link.Type != proton.LinkTypeFile || n.Link.FileProperties == nil {
		return nil, errors.New("node is not a file")
	}

	sessionKey, err := n.Link.GetSessionKey(n.KR)
	if err != nil {
		return nil, err
	}

	revID := n.Link.FileProperties.ActiveRevision.ID

	rev, err := c.api.GetRevisionAllBlocks(ctx, c.shareID, n.Link.LinkID, revID)
	if err != nil {
		return nil, err
	}

	blocks := make(map[int]proton.Block, len(rev.Blocks))
	for _, b := range rev.Blocks {
		blocks[b.Index] = b
	}

	return &File{
		client:     c,
		sessionKey: sessionKey,
		blocks:     blocks,
		size:       n.Size,
		linkID:     n.Link.LinkID,
		revID:      revID,
		cache:      make(map[int][]byte),
	}, nil
}

// ReadAt fills p with data starting at off, downloading and decrypting blocks on demand.
func (f *File) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= f.size {
		return 0, io.EOF
	}

	n := 0
	for n < len(p) {
		curOff := off + int64(n)
		if curOff >= f.size {
			break
		}

		idx := blockIndexForOffset(curOff)
		data, err := f.getBlock(ctx, idx)
		if err != nil {
			if n > 0 {
				break
			}
			return 0, err
		}

		blockStart := blockByteOffset(idx)
		inBlockOff := curOff - blockStart
		if inBlockOff >= int64(len(data)) {
			break
		}

		copied := copy(p[n:], data[inBlockOff:])
		n += copied
	}

	return n, nil
}

func (f *File) getBlock(ctx context.Context, idx int) ([]byte, error) {
	f.mu.Lock()
	if data, ok := f.cache[idx]; ok {
		f.mu.Unlock()
		return data, nil
	}
	f.mu.Unlock()

	// ponytail: big files skip the disk cache so one video does not evict everything else; blocks are still fetched lazily
	useDisk := f.client.cache != nil && cacheOnDisk(f.size, f.client.largeFile)

	if useDisk {
		if data, ok := f.client.cache.Get(f.linkID, f.revID, idx); ok {
			f.mu.Lock()
			f.cachePut(idx, data)
			f.mu.Unlock()
			return data, nil
		}
	}

	blk, ok := f.blocks[idx]
	if !ok {
		return nil, errors.New("block not found")
	}

	ciphertext, err := f.client.getBlockBytes(ctx, blk.BareURL, blk.Token)
	if err != nil {
		return nil, err
	}

	plain, err := f.sessionKey.Decrypt(ciphertext)
	if err != nil {
		return nil, err
	}

	data := plain.GetBinary()

	if useDisk {
		f.client.cache.Put(f.linkID, f.revID, idx, data)
	}

	f.mu.Lock()
	f.cachePut(idx, data)
	f.mu.Unlock()

	return data, nil
}

// cachePut stores a decrypted block, evicting the oldest entry once the cache is full.
// Caller must hold f.mu.
func (f *File) cachePut(idx int, data []byte) {
	if _, exists := f.cache[idx]; exists {
		return
	}

	if len(f.cacheLRU) >= maxCachedBlocks {
		oldest := f.cacheLRU[0]
		f.cacheLRU = f.cacheLRU[1:]
		delete(f.cache, oldest)
	}

	f.cache[idx] = data
	f.cacheLRU = append(f.cacheLRU, idx)
}

// cacheOnDisk reports whether a file of the given size should have its blocks stored in
// the on-disk cache. largeFile <= 0 means no threshold, so every size is cached.
func cacheOnDisk(size, largeFile int64) bool {
	return largeFile <= 0 || size <= largeFile
}

// thumbnailPreview is Type=1 in Proton's thumbnail API, the small preview every thumbnailed
// revision has (Type=2 is an HD preview only photos get).
// Source: WebClients packages/drive-store/store/_uploads/media/interface.ts, enum ThumbnailType.
const thumbnailPreview = 1

// HasThumbnail reports whether this node's active revision has a preview image stored with it.
func (n *Node) HasThumbnail() bool {
	if n.Link.Type != proton.LinkTypeFile || n.Link.FileProperties == nil {
		return false
	}

	return bool(n.Link.FileProperties.ActiveRevision.Thumbnail)
}

// Thumbnail downloads and decrypts the preview image Proton stores next to a file's active
// revision. It never touches the file's own content blocks, so a preview costs one small
// download no matter how big the file is.
func (c *Client) Thumbnail(ctx context.Context, n *Node) ([]byte, error) {
	if !n.HasThumbnail() {
		return nil, errors.New("revision has no thumbnail")
	}

	sessionKey, err := n.Link.GetSessionKey(n.KR)
	if err != nil {
		return nil, err
	}

	bareURL, token, err := c.thumbnailURL(ctx, n.Link.LinkID, n.Link.FileProperties.ActiveRevision.ID)
	if err != nil {
		return nil, err
	}

	ciphertext, err := c.getBlockBytes(ctx, bareURL, token)
	if err != nil {
		return nil, err
	}

	// The thumbnail is encrypted to the file's content session key, the same one the content
	// blocks use, and its signature is not verified.
	// Source: WebClients packages/drive-store/store/_downloads/download/downloadThumbnailPure.ts
	// (decryptThumbnail) and @protontech/drive-sdk src/internal/download/cryptoService.ts
	// (decryptThumbnail, "We ignore verification for thumbnails").
	plain, err := sessionKey.Decrypt(ciphertext)
	if err != nil {
		return nil, err
	}

	return plain.GetBinary(), nil
}

// thumbnailURL asks Proton where a revision's thumbnail data packet lives.
//
// Source: WebClients packages/shared/lib/api/drive/files.ts, queryFileRevisionThumbnail:
// GET drive/shares/{shareID}/files/{linkID}/revisions/{revisionID}/thumbnail?Type=N, answered
// with DriveFileRevisionThumbnailResult{ThumbnailLink, ThumbnailBareURL, ThumbnailToken}
// (packages/shared/lib/interfaces/drive/file.ts).
//
// go-proton-api exposes no way to build an arbitrary request, but GetBlock issues an
// authenticated GET (with the automatic token refresh in Client.doRes) against any path and
// hands back the raw body, so the JSON is decoded here. The empty storage token it sends along
// is an unused header on an API path.
func (c *Client) thumbnailURL(ctx context.Context, linkID, revID string) (bareURL, token string, err error) {
	path := fmt.Sprintf("/drive/shares/%s/files/%s/revisions/%s/thumbnail?Type=%d", c.shareID, linkID, revID, thumbnailPreview)

	body, err := c.getBlockBytes(ctx, path, "")
	if err != nil {
		return "", "", err
	}

	var res struct {
		ThumbnailBareURL string
		ThumbnailToken   string
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", "", fmt.Errorf("thumbnail response for revision %s: %w", revID, err)
	}
	if res.ThumbnailBareURL == "" {
		return "", "", fmt.Errorf("no thumbnail url for revision %s", revID)
	}

	return res.ThumbnailBareURL, res.ThumbnailToken, nil
}

// getBlockBytes reads a storage GET to completion.
func (c *Client) getBlockBytes(ctx context.Context, bareURL, token string) ([]byte, error) {
	rc, err := c.api.GetBlock(ctx, bareURL, token)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	return io.ReadAll(rc)
}

// blockIndexForOffset returns the 1-based Proton block index covering byte offset off.
func blockIndexForOffset(off int64) int {
	return int(off/blockSize) + 1
}

// blockByteOffset returns the starting byte offset (within the file) of the given 1-based block index.
func blockByteOffset(idx int) int64 {
	return int64(idx-1) * blockSize
}
