package drive

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	proton "github.com/henrybear327/go-proton-api"
)

// defaultRateLimitBackoff is how long a call site backs off after a 429/503 that carries no
// usable Retry-After hint (see retryAfterHint); also the fallback wait waitOutRateLimit uses when
// SetMetaTimeout was never called (a bare test Client).
const defaultRateLimitBackoff = 30 * time.Second

// maxRateLimitBackoff caps how far a single 429/503 can push the shared backoff window out, so a
// pathological Retry-After value never parks every call site for an unreasonable stretch.
const maxRateLimitBackoff = 5 * time.Minute

// rateLimit is the shared "stop calling for a while" window every API call site funnels its
// errors through (see noteAPIError) and checks before issuing a new request (see RateLimited):
// a burst of "Too many recent API requests" seen on one path (a directory listing, say) backs off
// every other path (events, thumbnails, block downloads) too, instead of each hammering the API
// on its own schedule.
type rateLimit struct {
	mu    sync.Mutex
	until time.Time
}

// ErrRateLimited is returned by waitOutRateLimit when a caller's patience (bounded by
// SetMetaTimeout) ran out before Proton's own rate-limit window did; fusefs maps it to ETIMEDOUT
// the same as a deadline, since that is exactly what happened from a FUSE caller's perspective.
var ErrRateLimited = errors.New("proton api rate limited, backing off")

// SetMetaTimeout tells the client how long a caller is willing to wait out a rate-limit window
// (see waitOutRateLimit) before giving up rather than blocking. Called once from fusefs.Mount
// with Options.MetaTimeout. Must be called once, before any goroutine that reads it; Mount does
// this, and this is why no lock is needed.
func (c *Client) SetMetaTimeout(d time.Duration) {
	c.metaTimeout = d
}

// noteAPIError funnels err through the shared rate-limit gate when it is a Proton 429 or 503; any
// other error, or a nil err, is a no-op. Every network call site that can be throttled calls this
// on failure.
//
//nolint:unused // part of the backoff API, not yet wired in all call sites
func (c *Client) noteAPIError(err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		return
	}
	c.noteRateLimitStatus(apiErr.Status, err)
}

// noteRateLimitStatus opens or extends the shared backoff window for a 429/503 response. It is
// split out from noteAPIError so putJSON, which already has the raw HTTP status from its
// hand-rolled request, can call it directly without needing err to unwrap to an *APIError (see
// asAPIError's doc for why that shape can't be relied on there).
func (c *Client) noteRateLimitStatus(status int, err error) {
	if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
		return
	}

	wait := defaultRateLimitBackoff
	if apiErr, ok := asAPIError(err); ok {
		if hint, ok := retryAfterHint(apiErr); ok {
			wait = hint
		}
	}
	if wait > maxRateLimitBackoff {
		wait = maxRateLimitBackoff
	}

	until := time.Now().Add(wait)

	c.rateLimit.mu.Lock()
	wasExpired := time.Now().After(c.rateLimit.until)
	c.rateLimit.until = until
	c.rateLimit.mu.Unlock()

	if wasExpired {
		slog.Warn("rate limited by Proton, backing off", "until", until.Format(time.RFC3339))
	}
}

// RateLimited reports how long is left in the current backoff window, if any.
func (c *Client) RateLimited() (time.Duration, bool) {
	if c == nil {
		return 0, false
	}

	c.rateLimit.mu.Lock()
	defer c.rateLimit.mu.Unlock()

	remaining := time.Until(c.rateLimit.until)
	if remaining <= 0 {
		return 0, false
	}
	return remaining, true
}

// waitOutRateLimit blocks until the current rate-limit window passes, ctx is done, or the
// client's meta timeout elapses, whichever comes first, so a single FUSE read never sleeps out a
// multi-minute backoff. It returns immediately when no window is active.
//
//nolint:unused // part of the backoff API, not yet wired in all call sites
func (c *Client) waitOutRateLimit(ctx context.Context) error {
	wait, limited := c.RateLimited()
	if !limited {
		return nil
	}

	patience := c.metaTimeout
	if patience <= 0 {
		patience = defaultRateLimitBackoff
	}

	giveUp := wait > patience
	if giveUp {
		wait = patience
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-timer.C:
		if giveUp {
			return ErrRateLimited
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryAfterHint reads a Retry-After hint from apiErr.Details, on the chance a response ever puts
// one there. go-proton-api's own resty retry policy already consumes the HTTP header (and sleeps
// through it) before this error ever reaches application code, and does not forward the value, so
// in practice this rarely finds one and noteRateLimitStatus falls back to defaultRateLimitBackoff.
func retryAfterHint(apiErr *proton.APIError) (time.Duration, bool) {
	details, ok := apiErr.Details.(map[string]any)
	if !ok {
		return 0, false
	}

	for _, key := range []string{"RetryAfter", "retryAfter", "Retry-After"} {
		v, ok := details[key]
		if !ok {
			continue
		}
		if secs, ok := v.(float64); ok && secs > 0 {
			return time.Duration(secs) * time.Second, true
		}
	}

	return 0, false
}

// IsUnauthorized reports whether err is a 401 Unauthorized response from the Proton API,
// which means the session has expired and the user needs to login again.
func IsUnauthorized(err error) bool {
	apiErr, ok := asAPIError(err)
	if !ok {
		return false
	}
	return apiErr.Status == http.StatusUnauthorized
}

// asAPIError extracts a *proton.APIError from err's chain, matching either the pointer shape
// go-proton-api wraps its own errors in, or the plain value shape drive.putJSON wraps its
// hand-rolled requests' errors in (proton.APIError's Error method has a value receiver, so
// fmt.Errorf("%w", apiErr) there wraps a value, not a pointer).
func asAPIError(err error) (*proton.APIError, bool) {
	var p *proton.APIError
	if errors.As(err, &p) {
		return p, true
	}

	var v proton.APIError
	if errors.As(err, &v) {
		return &v, true
	}

	return nil, false
}
