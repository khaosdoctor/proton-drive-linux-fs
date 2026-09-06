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
	"strings"
	"time"

	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/auth"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/logx"
)

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

	resp, err := http.DefaultClient.Do(req)
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
