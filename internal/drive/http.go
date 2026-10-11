package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/auth"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/logx"
)

// putClient carries putJSON's small JSON PUTs; its Timeout covers the whole exchange.
var putClient = &http.Client{Timeout: 2 * time.Minute}

// storageClient carries getRaw's GETs. It has no overall Timeout: a 4 MiB block on a slow link
// can take minutes, and the caller's ctx already bounds that. ResponseHeaderTimeout still cuts
// off a server that accepts the request and never answers, and getRawRetrying retries that.
var storageClient = &http.Client{Transport: storageTransport()}

func storageTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 60 * time.Second
	return t
}

// putJSON sends body as a JSON PUT to path (relative to auth.APIURL), authenticated with the
// client's token source, and retries once after forcing a token refresh on a 401.
//
// go-proton-api's Client only exposes request helpers (do/doRes/exec) for its own typed methods;
// they're unexported, so there's no library way to PUT an arbitrary body. This talks to the API
// directly, sending the same headers the library sets in client.go (exec) and
// manager_builder.go (build): x-pm-uid, an Authorization bearer token, and x-pm-appversion.
func (c *Client) putJSON(ctx context.Context, path string, body any) error {
	status, err := c.putJSONOnce(ctx, path, body)
	if status != http.StatusUnauthorized {
		return err
	}

	// GetUser goes through the shared *proton.Client, so a 401 there makes go-proton-api refresh
	// the token itself and run the AuthHandler that updates our token source.
	if _, refreshErr := c.api.GetUser(ctx); refreshErr != nil {
		return err
	}

	_, err = c.putJSONOnce(ctx, path, body)
	return err
}

// putJSONOnce issues a single PUT attempt, returning the HTTP status code (0 on a transport
// error) alongside any error so putJSON can tell a 401 apart from other failures.
func (c *Client) putJSONOnce(ctx context.Context, path string, body any) (int, error) {
	start := time.Now()
	defer func() { slog.Debug("api call", "call", "PUT "+path, logx.Elapsed(start)) }()

	payload, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.apiURL+path, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}

	uid, access := c.tokenSource()
	req.Header.Set("x-pm-uid", uid)
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("x-pm-appversion", auth.AppVersion)
	req.Header.Set("Accept", "application/vnd.protonmail.v1+json")
	req.Header.Set("Content-Type", "application/json")

	slog.Debug("api request", "method", "PUT", "path", path)
	resp, err := putClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}

	var apiErr proton.APIError
	if decErr := json.NewDecoder(resp.Body).Decode(&apiErr); decErr != nil {
		return resp.StatusCode, fmt.Errorf("PUT %s: unexpected status %d (payload keys: %s)", path, resp.StatusCode, payloadFieldLengths(payload))
	}
	apiErr.Status = resp.StatusCode

	// Proton's "This value should not be blank" (Code=2000) never names the field -- record each
	// key's string length (never its value; names/passphrases are encrypted but still sensitive)
	// so the next failure names the empty one straight from the journal.
	return resp.StatusCode, fmt.Errorf("%w (payload keys: %s)", apiErr, payloadFieldLengths(payload))
}

// getRaw reads a GET to completion: a storage URL (absolute, with its pm-storage-token), or an API
// path (leading "/", relative to apiURL, token empty) for thumbnailURL's JSON. Like putJSON, it
// retries once after forcing a token refresh on a 401.
//
// It replaces go-proton-api's GetBlock, whose SetDoNotParseResponse(true) breaks three ways:
// resty's retry loop on 429/503 overwrites each response without closing its body, Client.doRes
// re-runs the request after a 401 refresh without closing the first one, and resty skips the
// afterResponse middleware that turns a non-2xx status into an APIError, so a final 429 or 404
// body came back with a nil error and was decrypted as ciphertext. The headers are the ones
// GetBlock sends (exec, plus manager_builder.go's OnBeforeRequest).
func (c *Client) getRaw(ctx context.Context, url, token string) ([]byte, error) {
	body, status, err := c.getRawRetrying(ctx, url, token)
	if status != http.StatusUnauthorized {
		return body, err
	}

	if _, refreshErr := c.api.GetUser(ctx); refreshErr != nil {
		return nil, err
	}

	body, _, err = c.getRawRetrying(ctx, url, token)
	return body, err
}

// The retry budget below copies what go-proton-api's resty client gave GetBlock, so a burst main
// absorbed is absorbed here too: SetRetryCount(3) is 4 attempts (manager_builder.go), and
// catchRetryAfter (response.go) waits the Retry-After seconds, or 10s without one, plus up to 10s
// of jitter, capped by SetRetryMaxWaitTime(time.Minute).
const (
	getRawAttempts       = 4
	maxRateLimitWait     = time.Minute
	rateLimitJitterRange = 10 * time.Second
)

// rateLimitDefaultWait is the wait before the jitter when a 429/503 carries no Retry-After; a var,
// with rateLimitJitter, so tests can shrink them.
var rateLimitDefaultWait = 10 * time.Second

// rateLimitJitter returns the random extra wait added to every 429/503 wait.
var rateLimitJitter = func() time.Duration {
	return time.Duration(rand.Int64N(int64(rateLimitJitterRange)))
}

// transportRetryBase is the first wait after a transport error, doubling per attempt: resty's
// jitterBackoff from its 100ms RetryWaitTime default, without the jitter. A var so tests can
// shrink it.
var transportRetryBase = 100 * time.Millisecond

// getRawRetrying is getRawOnce with up to getRawAttempts tries on 429/503 and on transport
// errors, which go-proton-api also retried (catchDialError, catchDropError). A 429/503 waits out
// the server's Retry-After or rateLimitDefaultWait, plus jitter; a transport error waits
// transportRetryBase, doubling. getRawOnce reads the whole body, so a connection reset halfway
// through a 4 MiB block is retried too. Only when the last attempt still answers 429/503 does it
// open the shared backoff window and return ErrRateLimited, so a burst the retries absorb never
// blocks the other call sites.
func (c *Client) getRawRetrying(ctx context.Context, url, token string) ([]byte, int, error) {
	for attempt := 1; ; attempt++ {
		body, status, retryAfter, err := c.getRawOnce(ctx, url, token)
		if err != nil && ctx.Err() != nil {
			return nil, status, err
		}

		apiErr, isAPIErr := asAPIError(err)
		transportErr := err != nil && !isAPIErr
		rateLimited := status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
		if !transportErr && !rateLimited {
			return body, status, err
		}

		if attempt == getRawAttempts {
			if transportErr {
				return nil, status, err
			}
			c.noteRateLimitStatus(status, apiErr)
			return nil, status, fmt.Errorf("%w: %w", ErrRateLimited, *apiErr)
		}

		wait := transportRetryBase << (attempt - 1)
		if rateLimited {
			wait = rateLimitDefaultWait
			if retryAfter > 0 {
				wait = retryAfter
			}
			wait = min(wait+rateLimitJitter(), maxRateLimitWait)
		}

		select {
		case <-ctx.Done():
			c.noteRateLimitStatus(status, apiErr)
			return nil, status, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// retryAfter parses a Retry-After header, either delay-seconds or an HTTP date; 0 when absent or
// unusable.
func retryAfter(v string) time.Duration {
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// getRawOnce issues a single GET attempt, returning the HTTP status code (0 on a transport error)
// so its callers can tell a 401 or a 429/503 apart, and the response's Retry-After. A non-2xx
// status comes back as a *proton.APIError, so IsNotFound and the rest of the package's error
// checks see it the same way as a go-proton-api error.
func (c *Client) getRawOnce(ctx context.Context, url, token string) ([]byte, int, time.Duration, error) {
	if strings.HasPrefix(url, "/") {
		url = c.apiURL + url
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, 0, err
	}

	uid, access := c.tokenSource()
	req.Header.Set("x-pm-uid", uid)
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("x-pm-appversion", auth.AppVersion)
	if token != "" {
		req.Header.Set("pm-storage-token", token)
	}

	resp, err := storageClient.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	// Draining what the error decode left (bounded) lets the connection go back to the pool
	// for the retry instead of being torn down.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, err := io.ReadAll(resp.Body)
		return body, resp.StatusCode, 0, err
	}

	// Storage hosts don't always answer with Proton's JSON error shape, so a body that doesn't
	// decode still leaves the status and its standard text.
	apiErr := &proton.APIError{}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(apiErr)
	apiErr.Status = resp.StatusCode
	if apiErr.Message == "" {
		apiErr.Message = http.StatusText(resp.StatusCode)
	}

	return nil, resp.StatusCode, retryAfter(resp.Header.Get("Retry-After")), apiErr
}

// payloadFieldLengths summarizes a marshaled JSON object's top-level fields as "Key=len Key2=len2
// ...", in alphabetical key order for a deterministic one-line message. Non-string values are
// sized by their raw JSON length. Returns "" if payload isn't a JSON object.
func payloadFieldLengths(payload []byte) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return ""
	}

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		length := len(fields[k])
		var s string
		if err := json.Unmarshal(fields[k], &s); err == nil {
			length = len(s)
		}
		parts = append(parts, fmt.Sprintf("%s=%d", k, length))
	}

	return strings.Join(parts, " ")
}
