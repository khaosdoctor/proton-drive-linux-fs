package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/state"
)

// Client talks to a running daemon's local API over its unix socket.
type Client struct {
	http *http.Client
}

// NewClient returns a Client dialing the daemon's unix socket. It never checks that a daemon is
// actually listening; a request against a dead or absent socket simply fails, which callers
// treat the same way they already treat a missing status.json: fall back to something else.
func NewClient() (*Client, error) {
	sockPath, err := SocketPath()
	if err != nil {
		return nil, err
	}

	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			},
			Timeout: 5 * time.Second,
		},
	}, nil
}

// Status fetches GET /v1/status.
func (c *Client) Status(ctx context.Context) (state.Status, error) {
	var st state.Status
	err := c.getJSON(ctx, "/v1/status", &st)
	return st, err
}

// Cache fetches GET /v1/cache.
func (c *Client) Cache(ctx context.Context) (CacheStats, error) {
	var stats CacheStats
	err := c.getJSON(ctx, "/v1/cache", &stats)
	return stats, err
}

// ClearCache calls POST /v1/cache/clear and returns the bytes freed.
func (c *Client) ClearCache(ctx context.Context) (int64, error) {
	var res CacheClearResult
	err := c.postJSON(ctx, "/v1/cache/clear", &res)
	return res.Freed, err
}

// SetPaused calls POST /v1/pause or /v1/resume.
func (c *Client) SetPaused(ctx context.Context, paused bool) error {
	path := "/v1/resume"
	if paused {
		path = "/v1/pause"
	}
	var res PauseResult
	return c.postJSON(ctx, path, &res)
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, out)
}

func (c *Client) postJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodPost, path, out)
}

// doJSON issues a request against the unix socket; the host in the URL is ignored by the custom
// DialContext above and only there to satisfy net/http's URL parser.
func (c *Client) doJSON(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, nil)
	if err != nil {
		return err
	}

	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("api %s: status %d", path, res.StatusCode)
	}

	return json.NewDecoder(res.Body).Decode(out)
}
