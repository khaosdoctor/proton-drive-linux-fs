package drive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/logx"
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

	// path is this file's display path, used only for the Transfer this file's downloads are
	// reported under (see ensureTransfer).
	path string

	mu       sync.Mutex
	cache    map[int][]byte
	cacheLRU []int

	// inflight serializes concurrent misses on the same block index (int -> *blockFetch): only
	// the caller that wins the LoadOrStore actually downloads it, everyone else waits on its
	// result, so a block is never downloaded, decrypted or counted against the transfer's
	// progress more than once. Mirrors dirNode's loading/finishLoad pattern in fusefs.go.
	inflight sync.Map

	// transferMu guards transfer, lazily created on this file's first real network fetch so a
	// file served entirely from cache never shows up as "downloading".
	transferMu sync.Mutex
	transfer   *Transfer
	failed     atomic.Bool
}

// blockFetch is the in-flight record one claimBlock winner publishes for a block index; every
// other caller waits on done and then reads err (valid only once done is closed).
type blockFetch struct {
	done chan struct{}
	err  error
}

// OpenFile fetches the session key and full block list for n's active revision. path is where
// this file's downloads are reported under (see Transfer); an empty path falls back to n.Name.
// ponytail: fetches the whole block list at open; paginate with GetRevision if huge files are slow.
func (c *Client) OpenFile(ctx context.Context, n *Node, path string) (*File, error) {
	if n.Link.Type != proton.LinkTypeFile || n.Link.FileProperties == nil {
		return nil, errors.New("node is not a file")
	}
	if path == "" {
		path = n.Name
	}

	kr, err := n.Keyring()
	if err != nil {
		return nil, err
	}

	sessionKey, err := n.Link.GetSessionKey(kr)
	if err != nil {
		return nil, err
	}

	// An open reads real bytes, so the plaintext size has to be the real one, not the encrypted
	// Link size a fresh listing starts with.
	n.ResolveAttrs()

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
		size:       n.Size(),
		linkID:     n.Link.LinkID,
		revID:      revID,
		path:       path,
		cache:      make(map[int][]byte),
	}, nil
}

// ensureTransfer lazily starts this file's tracked download transfer on its first real network
// fetch, so a file served entirely from cache never shows up as "downloading".
func (f *File) ensureTransfer() *Transfer {
	f.transferMu.Lock()
	defer f.transferMu.Unlock()
	if f.transfer == nil {
		f.transfer = f.client.begin(f.path, "download", f.size)
	}
	return f.transfer
}

// BytesTransferred reports how many bytes this file's tracked download has moved so far, or 0 if
// it never touched the network (served entirely from cache).
func (f *File) BytesTransferred() int64 {
	f.transferMu.Lock()
	defer f.transferMu.Unlock()
	if f.transfer == nil {
		return 0
	}
	return f.transfer.Bytes()
}

// Close ends this file's tracked download transfer, if one was ever started, and reports whether
// any block fetch failed. Safe to call once per open File; a second call is a no-op.
func (f *File) Close() error {
	f.transferMu.Lock()
	t := f.transfer
	f.transfer = nil
	f.transferMu.Unlock()

	if t == nil {
		return nil
	}

	var err error
	if f.failed.Load() {
		err = errors.New("a block download failed")
	}
	f.client.end(t, err)
	return err
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

// getBlock returns block idx's decrypted content, from the in-memory cache, the on-disk cache, or
// the network in that order. Concurrent callers asking for the same block never both hit the
// network: only the first (the "winner", see claimBlock) fetches it, and the rest wait for that
// result instead of duplicating the download and its progress.
func (f *File) getBlock(ctx context.Context, idx int) ([]byte, error) {
	debug := logx.DebugEnabled(ctx)

	if data, ok := f.cachedBlock(idx); ok {
		if debug {
			slog.Debug("block cache hit", "link", f.linkID, "block", idx, "cache", "memory")
		}
		return data, nil
	}

	fetch, winner := f.claimBlock(idx)
	if !winner {
		select {
		case <-fetch.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if fetch.err != nil {
			return nil, fetch.err
		}
		if data, ok := f.cachedBlock(idx); ok {
			return data, nil
		}
		return nil, errors.New("block fetch result missing from cache")
	}

	data, err := f.fetchAndCacheBlock(ctx, idx, debug)
	f.releaseBlock(idx, fetch, err)
	return data, err
}

// cachedBlock reads block idx from the in-memory cache.
func (f *File) cachedBlock(idx int) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.cache[idx]
	return data, ok
}

// claimBlock reports whether this call is the one that fetches block idx: true with the record
// to fill in and close when done, or false with the record to wait on for whoever already claimed
// it.
func (f *File) claimBlock(idx int) (fetch *blockFetch, winner bool) {
	bf := &blockFetch{done: make(chan struct{})}
	actual, loaded := f.inflight.LoadOrStore(idx, bf)
	if !loaded {
		return bf, true
	}
	return actual.(*blockFetch), false
}

// releaseBlock publishes the winner's result to every waiter and un-claims the block so a later
// retry (e.g. after a failure) can claim it again.
func (f *File) releaseBlock(idx int, fetch *blockFetch, err error) {
	f.inflight.Delete(idx)
	fetch.err = err
	close(fetch.done)
}

// fetchAndCacheBlock does the actual disk-cache lookup or network fetch for block idx and caches
// the result. Only claimBlock's winner calls this, so it never runs twice at once for the same
// block.
func (f *File) fetchAndCacheBlock(ctx context.Context, idx int, debug bool) ([]byte, error) {
	// ponytail: big files skip the disk cache so one video does not evict everything else; blocks are still fetched lazily
	useDisk := f.client.cache != nil && cacheOnDisk(f.size, f.client.largeFile)

	if useDisk {
		if data, ok := f.client.cache.Get(f.linkID, f.revID, idx); ok {
			f.mu.Lock()
			f.cachePut(idx, data)
			f.mu.Unlock()
			if debug {
				slog.Debug("block cache hit", "link", f.linkID, "block", idx, "cache", "disk", "size", len(data))
			}
			return data, nil
		}
	}

	blk, ok := f.blocks[idx]
	if !ok {
		return nil, errors.New("block not found")
	}

	releaseSlot, err := f.client.acquireDownload(ctx)
	if err != nil {
		return nil, err
	}

	transfer := f.ensureTransfer()

	dlStart := time.Now()
	ciphertext, err := fetchBlock(ctx, f.client, blk.BareURL, blk.Token)
	releaseSlot()
	if err != nil {
		f.failed.Store(true)
		return nil, err
	}

	decStart := time.Now()
	plain, err := f.sessionKey.Decrypt(ciphertext)
	if err != nil {
		f.failed.Store(true)
		return nil, err
	}

	data := plain.GetBinary()
	transfer.Add(int64(len(data)))
	if debug {
		slog.Debug("block cache miss, downloaded", "link", f.linkID, "block", idx, "size", len(data),
			"download_elapsed", decStart.Sub(dlStart), "decrypt_elapsed", time.Since(decStart))
	}

	if useDisk {
		f.client.cache.Put(f.linkID, f.revID, idx, data)
	}

	f.mu.Lock()
	f.cachePut(idx, data)
	f.mu.Unlock()

	return data, nil
}

// fetchBlock downloads one encrypted block; overridden in tests to fake or count downloads
// without a real Proton API client.
var fetchBlock = func(ctx context.Context, c *Client, bareURL, token string) ([]byte, error) {
	return c.getBlockBytes(ctx, bareURL, token)
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

	kr, err := n.Keyring()
	if err != nil {
		return nil, err
	}

	sessionKey, err := n.Link.GetSessionKey(kr)
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
