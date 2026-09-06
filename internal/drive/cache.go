package drive

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// blocksSubdir and listingsSubdir are BlockCache's two on-disk trees, both under the same root
// and bounded by the same byte budget: blocksSubdir/<linkID>/<revID>/<idx> holds decrypted file
// blocks, listingsSubdir/<linkID>.json holds a persisted directory listing.
const (
	blocksSubdir   = "blocks"
	listingsSubdir = "listings"
)

// BlockCache stores decrypted file blocks and persisted directory listings on disk, bounded by
// one total byte budget shared across both, evicting the least recently used entries (by mtime)
// regardless of which kind they are. A cache with an empty dir is disabled: every method is a
// no-op.
type BlockCache struct {
	dir   string
	limit int64

	mu   sync.Mutex
	size int64
}

// OpenBlockCache opens (creating and migrating the layout if needed) an on-disk cache rooted at
// dir, bounded to limit total bytes shared by blocks and listings. limit <= 0 disables the cache:
// all methods become no-ops.
func OpenBlockCache(dir string, limit int64) (*BlockCache, error) {
	if limit <= 0 {
		return &BlockCache{}, nil
	}

	if err := migrateLayout(dir); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Join(dir, blocksSubdir), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, listingsSubdir), 0700); err != nil {
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

// migrateLayout moves an old-layout cache (blocks stored directly at <dir>/<linkID>/...) under
// <dir>/blocks, the layout every cache used before the listing cache introduced a second kind
// alongside it. It is a no-op once <dir>/blocks exists, or dir has no entries yet (a fresh cache).
func migrateLayout(dir string) error {
	blocksDir := filepath.Join(dir, blocksSubdir)
	if _, err := os.Stat(blocksDir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	// Entries move through a temp directory first: renaming dir/blocks itself into existence
	// while iterating dir's own entries would risk renaming it into one of the names being moved.
	tmp := filepath.Join(dir, blocksSubdir+".migrating")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(tmp, e.Name())); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, blocksDir); err != nil {
		return err
	}

	slog.Info("migrated block cache layout", "dir", dir)
	return nil
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

	dir := filepath.Join(c.dir, blocksSubdir, linkID, revID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}

	tmp, err := os.CreateTemp(dir, "block-*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return
	}

	path := filepath.Join(dir, strconv.Itoa(index))
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return
	}

	c.mu.Lock()
	c.size += int64(len(data))
	c.mu.Unlock()

	c.evict()
}

// ListingEntry is one child of a persisted directory listing: its decrypted name and the raw
// proton.Link JSON needed to rebuild the node without a network call.
//
// A disk-served listing carries no staleness marker of its own (an extra GetLink round-trip to
// check the parent's ModifyTime before serving would cancel out the whole point of skipping the
// network here); staleness is instead handled at the point it can actually be observed cheaply:
// dirNode.serveFromDiskCache always kicks off a real fetch in the background right after serving
// from disk, and a per-entry ghost (a file whose link or revision the API has since 404'd) self-
// heals in fusefs.openErrno, which expires the parent's cached listing on the spot.
type ListingEntry struct {
	Name string          `json:"name"`
	Link json.RawMessage `json:"link"`
}

// GetListing reads a persisted directory listing, bumping its mtime (recency) on a hit. The
// second result is false on a miss or when the cache is disabled.
func (c *BlockCache) GetListing(linkID string) ([]ListingEntry, bool) {
	if c.dir == "" {
		return nil, false
	}

	path := c.listingPath(linkID)
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Debug("listing cache miss", "link", linkID)
		return nil, false
	}

	var entries []ListingEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Debug("listing cache miss", "link", linkID, "err", err)
		return nil, false
	}

	now := time.Now()
	_ = os.Chtimes(path, now, now)

	slog.Debug("listing cache hit", "link", linkID, "count", len(entries))
	return entries, true
}

// PutListing persists a directory's listing as one JSON file, replacing whatever was cached for
// linkID before, then evicts the least recently used entries if the cache is now over its byte
// limit. A no-op when the cache is disabled.
func (c *BlockCache) PutListing(linkID string, entries []ListingEntry) {
	if c.dir == "" {
		return
	}

	dir := filepath.Join(c.dir, listingsSubdir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return
	}

	tmp, err := os.CreateTemp(dir, "listing-*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	// Listing files hold decrypted names; keep them readable only by the owner, same as the
	// session file.
	if err := os.Chmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return
	}

	path := c.listingPath(linkID)
	oldSize := int64(0)
	if info, err := os.Stat(path); err == nil {
		oldSize = info.Size()
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return
	}

	c.mu.Lock()
	c.size += int64(len(data)) - oldSize
	c.mu.Unlock()

	c.evict()
}

// evict removes least-recently-used entries (oldest mtime first), blocks and listings alike,
// until the cache is back within its byte limit.
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

// blockPath returns the on-disk path for a cached block, laid out as
// <dir>/blocks/<linkID>/<revID>/<index>.
func (c *BlockCache) blockPath(linkID, revID string, index int) string {
	return filepath.Join(c.dir, blocksSubdir, linkID, revID, strconv.Itoa(index))
}

// listingPath returns the on-disk path for a persisted directory listing, laid out as
// <dir>/listings/<linkID>.json.
func (c *BlockCache) listingPath(linkID string) string {
	return filepath.Join(c.dir, listingsSubdir, linkID+".json")
}
