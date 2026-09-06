package drive

// The mount process publishes these so the tray and the local API, which run separately, can
// tell whether data is moving right now and show what it is.

import (
	"log/slog"
	"sync/atomic"
	"time"
)

// Transfer tracks the progress of one in-flight upload or download.
type Transfer struct {
	ID      uint64
	Path    string
	Action  string // "upload" or "download"
	Total   int64  // total bytes, when known; 0 if not
	Started time.Time

	bytes atomic.Int64
}

// Add records n more bytes moved by this transfer.
func (t *Transfer) Add(n int64) { t.bytes.Add(n) }

// Bytes reports how many bytes this transfer has moved so far.
func (t *Transfer) Bytes() int64 { return t.bytes.Load() }

var nextTransferID atomic.Uint64

// begin registers path as a new in-flight transfer.
func (c *Client) begin(path, action string, total int64) *Transfer {
	t := &Transfer{ID: nextTransferID.Add(1), Path: path, Action: action, Total: total, Started: time.Now()}
	c.transfers.Add(1)
	c.current.Store(t.ID, t)
	return t
}

// end unregisters a finished transfer, successful or not; err is only used for a debug log line,
// recording the outcome for the tray/API is the caller's own job.
func (c *Client) end(t *Transfer, err error) {
	c.current.Delete(t.ID)
	c.transfers.Add(-1)
	if err != nil {
		slog.Debug("transfer failed", "path", t.Path, "action", t.Action, "err", err)
	}
}

// Transfers reports how many uploads and downloads are in flight.
func (c *Client) Transfers() int64 { return c.transfers.Load() }

// CurrentTransfers returns a snapshot of every transfer in flight right now.
func (c *Client) CurrentTransfers() []*Transfer {
	var out []*Transfer
	c.current.Range(func(_, v any) bool {
		out = append(out, v.(*Transfer))
		return true
	})
	return out
}

// CacheDir returns the on-disk block cache directory, or "" when there is no cache, for the
// local API's /v1/cache endpoint.
func (c *Client) CacheDir() string {
	if c.cache == nil {
		return ""
	}
	return c.cache.dir
}

// ResetCacheSize zeroes the block cache's in-memory size counter, for when the API clears the
// cache directory out from under it (POST /v1/cache/clear).
func (c *Client) ResetCacheSize() {
	if c.cache == nil {
		return
	}
	c.cache.mu.Lock()
	c.cache.size = 0
	c.cache.mu.Unlock()
}
