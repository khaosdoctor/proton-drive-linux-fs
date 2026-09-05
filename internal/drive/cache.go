package drive

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// BlockCache stores decrypted blocks on disk, bounded by total bytes, evicting the least
// recently used entries. A cache with an empty dir is disabled: every method is a no-op.
type BlockCache struct {
	dir   string
	limit int64

	mu   sync.Mutex
	size int64
}

// OpenBlockCache opens (creating if needed) an on-disk block cache rooted at dir, bounded to
// limit total bytes. limit <= 0 disables the cache: all methods become no-ops.
func OpenBlockCache(dir string, limit int64) (*BlockCache, error) {
	if limit <= 0 {
		return &BlockCache{}, nil
	}

	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}

	var size int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &BlockCache{dir: dir, limit: limit, size: size}, nil
}

// Get reads a cached block, bumping its mtime (recency) on a hit.
func (c *BlockCache) Get(linkID, revID string, index int) ([]byte, bool) {
	if c.dir == "" {
		return nil, false
	}

	path := c.blockPath(linkID, revID, index)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}

	now := time.Now()
	_ = os.Chtimes(path, now, now)

	return data, true
}

// Put writes a decrypted block to disk, then evicts the least recently used entries if the
// cache is now over its byte limit.
func (c *BlockCache) Put(linkID, revID string, index int, data []byte) {
	if c.dir == "" {
		return
	}

	dir := filepath.Join(c.dir, linkID, revID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}

	tmp, err := os.CreateTemp(dir, "block-*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return
	}

	path := filepath.Join(dir, strconv.Itoa(index))
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return
	}

	c.mu.Lock()
	c.size += int64(len(data))
	c.mu.Unlock()

	c.evict()
}

// evict removes least-recently-used blocks (oldest mtime first) until the cache is back
// within its byte limit.
// ponytail: full scan per eviction; index if the cache dir gets huge
func (c *BlockCache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.size <= c.limit {
		return
	}

	type entry struct {
		path    string
		size    int64
		modTime time.Time
	}

	var entries []entry
	_ = filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		entries = append(entries, entry{path: path, size: info.Size(), modTime: info.ModTime()})
		return nil
	})

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime.Before(entries[j].modTime)
	})

	for _, e := range entries {
		if c.size <= c.limit {
			break
		}
		if err := os.Remove(e.path); err != nil {
			continue
		}
		c.size -= e.size
	}
}

// blockPath returns the on-disk path for a cached block, laid out as <dir>/<linkID>/<revID>/<index>.
func (c *BlockCache) blockPath(linkID, revID string, index int) string {
	return filepath.Join(c.dir, linkID, revID, strconv.Itoa(index))
}
