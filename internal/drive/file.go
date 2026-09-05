package drive

import (
	"context"
	"errors"
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

	blk, ok := f.blocks[idx]
	if !ok {
		return nil, errors.New("block not found")
	}

	rc, err := f.client.api.GetBlock(ctx, blk.BareURL, blk.Token)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	ciphertext, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}

	plain, err := f.sessionKey.Decrypt(ciphertext)
	if err != nil {
		return nil, err
	}

	data := plain.GetBinary()

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

// blockIndexForOffset returns the 1-based Proton block index covering byte offset off.
func blockIndexForOffset(off int64) int {
	return int(off/blockSize) + 1
}

// blockByteOffset returns the starting byte offset (within the file) of the given 1-based block index.
func blockByteOffset(idx int) int64 {
	return int64(idx-1) * blockSize
}
