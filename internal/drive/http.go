package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/auth"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/logx"
)

// httpClient carries every request this package sends around go-proton-api: putJSON's PUTs and
// getRaw's GETs. Its Timeout covers the whole exchange, body included, so a stalled connection
// can't hang a caller whose ctx has no deadline (the thumbnail worker's has none); two minutes
// leaves a 4 MiB block room on a slow link.
var httpClient = &http.Client{Timeout: 2 * time.Minute}

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
	resp, err := httpClient.Do(req)
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

// rateLimitAttempts is how many times getRawRetrying tries a GET that keeps answering 429/503,
// the same count go-proton-api's resty client used, so a short burst never reaches the reader.
const rateLimitAttempts = 3

// maxRetryAfterWait caps one wait between those attempts: a reader is blocked on it, so a long
// Retry-After is left to the shared backoff window instead.
const maxRetryAfterWait = 10 * time.Second

// rateLimitRetryBase is the first wait between attempts when the server sends no Retry-After,
// doubling after each; a var so tests can shrink it.
var rateLimitRetryBase = 500 * time.Millisecond

// getRawRetrying is getRawOnce with up to rateLimitAttempts tries on 429/503, waiting out the
// server's Retry-After (capped) or a doubling backoff between them. Only when the last attempt
// still fails does it open the shared backoff window and return ErrRateLimited, so a burst the
// retries absorb never blocks the other call sites.
func (c *Client) getRawRetrying(ctx context.Context, url, token string) ([]byte, int, error) {
	backoff := rateLimitRetryBase
	for attempt := 1; ; attempt++ {
		body, status, retryAfter, err := c.getRawOnce(ctx, url, token)
		if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
			return body, status, err
		}

		apiErr, _ := asAPIError(err)
		if attempt == rateLimitAttempts {
			c.noteRateLimitStatus(status, apiErr)
			return nil, status, fmt.Errorf("%w: %w", ErrRateLimited, *apiErr)
		}

		wait := backoff
		if retryAfter > 0 {
			wait = min(retryAfter, maxRetryAfterWait)
		}
		backoff *= 2

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

	resp, err := httpClient.Do(req)
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
