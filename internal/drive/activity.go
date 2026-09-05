package drive

// The mount process publishes this counter so the tray, which runs separately, can tell
// whether data is moving right now.

// beginTransfer marks the start of one block download or upload.
func (c *Client) beginTransfer() { c.transfers.Add(1) }

// endTransfer marks its end, successful or not.
func (c *Client) endTransfer() { c.transfers.Add(-1) }

// Transfers reports how many block downloads and uploads are in flight.
func (c *Client) Transfers() int64 { return c.transfers.Load() }
