package drive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMoveLinkReqJSON asserts every field Proton's move endpoint requires -- exactly WebClients'
// MoveLink shape (Name, Hash, ParentLinkID, NodePassphrase, NameSignatureEmail), no more and no
// less -- is present and non-empty in the marshaled request body. go-proton-api's own
// MoveLinkReq also carries OriginalHash/SignatureAddress; neither belongs in this payload, and an
// empty OriginalHash was exactly what the live "This value should not be blank" (Code=2000)
// error was about.
func TestMoveLinkReqJSON(t *testing.T) {
	req := moveLinkReq{
		Name:               "enc-name",
		Hash:               "name-hash",
		ParentLinkID:       "parent-link-id",
		NodePassphrase:     "enc-passphrase",
		NameSignatureEmail: "user@proton.me",
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	required := []string{"Name", "Hash", "ParentLinkID", "NodePassphrase", "NameSignatureEmail"}
	for _, key := range required {
		if got[key] == "" {
			t.Errorf("moveLinkReq JSON missing or empty required key %q: %s", key, data)
		}
	}

	for _, stale := range []string{"OriginalHash", "SignatureAddress", "NodePassphraseSignature", "SignatureEmail"} {
		if _, ok := got[stale]; ok {
			t.Errorf("moveLinkReq JSON carries %q, which isn't part of WebClients' MoveLink payload: %s", stale, data)
		}
	}

	if len(got) != len(required) {
		t.Errorf("moveLinkReq JSON has %d keys, want exactly %v: %s", len(got), required, data)
	}
}

// TestPayloadFieldLengths checks the error-decoration helper: it names every top-level JSON key
// with its string length (never the value), in alphabetical order, on one line -- so a blank
// field is nameable straight from the journal without ever logging real payload content.
func TestPayloadFieldLengths(t *testing.T) {
	req := moveLinkReq{
		Name:               "enc-name",
		Hash:               "name-hash",
		ParentLinkID:       "",
		NodePassphrase:     "enc-passphrase",
		NameSignatureEmail: "user@proton.me",
	}

	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := payloadFieldLengths(payload)
	want := "Hash=9 Name=8 NameSignatureEmail=14 NodePassphrase=14 ParentLinkID=0"
	if got != want {
		t.Errorf("payloadFieldLengths = %q, want %q", got, want)
	}
	if strings.Contains(got, "enc-name") || strings.Contains(got, "user@proton.me") {
		t.Errorf("payloadFieldLengths leaked a value: %q", got)
	}
}

// TestPutJSON checks the headers putJSON sends and that an API error body ({"Code":...,
// "Error":...}) surfaces as an error carrying both the code and the message.
func TestPutJSON(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"Code":2000,"Error":"This value should not be blank."}`))
	}))
	defer server.Close()

	c := &Client{
		apiURL:      server.URL,
		tokenSource: func() (string, string) { return "test-uid", "test-access" },
	}

	err := c.putJSON(context.Background(), "/drive/shares/s1/links/l1/move", moveLinkReq{Name: "x"})
	if err == nil {
		t.Fatal("putJSON: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "This value should not be blank.") {
		t.Errorf("error %q does not contain the API message", err)
	}
	if !strings.Contains(err.Error(), "2000") {
		t.Errorf("error %q does not contain the API code", err)
	}
	if !strings.Contains(err.Error(), "payload keys: ") || !strings.Contains(err.Error(), "Name=1") {
		t.Errorf("error %q does not name the payload field lengths", err)
	}

	if got := gotHeaders.Get("x-pm-uid"); got != "test-uid" {
		t.Errorf("x-pm-uid = %q, want %q", got, "test-uid")
	}
	if got := gotHeaders.Get("Authorization"); got != "Bearer test-access" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-access")
	}
	if got := gotHeaders.Get("x-pm-appversion"); got == "" {
		t.Error("x-pm-appversion header not set")
	}
	if got := gotHeaders.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// TestGetRawStatus checks that a storage GET hands back the body only on a 2xx: a 404 is a
// not-found error, a short 429 burst is retried away, and 429s on every attempt open the backoff
// window, never a body to decrypt as ciphertext.
func TestGetRawStatus(t *testing.T) {
	fastRetries(t)

	var burstHits, busyHits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(r.Header.Get("pm-storage-token")))
		case "/missing":
			http.Error(w, "<html>not here</html>", http.StatusNotFound)
		case "/burst":
			if burstHits.Add(1) <= getRawAttempts-1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = w.Write([]byte("after burst"))
		case "/busy":
			busyHits.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"Code":2028,"Error":"Too many recent API requests"}`))
		}
	}))
	defer server.Close()

	c := &Client{tokenSource: func() (string, string) { return "uid", "access" }}
	ctx := context.Background()

	body, err := c.getRaw(ctx, server.URL+"/ok", "tok")
	if err != nil || string(body) != "tok" {
		t.Fatalf("ok: body %q err %v, want the storage token echoed back", body, err)
	}

	if body, err := c.getRaw(ctx, server.URL+"/missing", "tok"); body != nil || !IsNotFound(err) {
		t.Errorf("missing: body %q err %v, want nil body and a not-found error", body, err)
	}

	body, err = c.getRaw(ctx, server.URL+"/burst", "tok")
	if err != nil || string(body) != "after burst" {
		t.Fatalf("burst: body %q err %v, want the body after %d 429s", body, err, getRawAttempts-1)
	}
	if _, limited := c.RateLimited(); limited {
		t.Error("burst: backoff window opened for a burst the retries absorbed")
	}

	body, err = c.getRaw(ctx, server.URL+"/busy", "tok")
	if body != nil || !errors.Is(err, ErrRateLimited) {
		t.Errorf("busy: body %q err %v, want nil body and ErrRateLimited", body, err)
	}
	if got := busyHits.Load(); got != getRawAttempts {
		t.Errorf("busy: %d attempts, want %d", got, getRawAttempts)
	}
	if _, limited := c.RateLimited(); !limited {
		t.Error("busy: backoff window not opened")
	}
}

// TestGetRawRetriesTransportError checks that a connection dropped halfway through the body is
// retried instead of failing the read.
func TestGetRawRetriesTransportError(t *testing.T) {
	fastRetries(t)

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) > 1 {
			_, _ = w.Write([]byte("whole block"))
			return
		}
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()

	c := &Client{tokenSource: func() (string, string) { return "uid", "access" }}
	body, err := c.getRaw(context.Background(), server.URL+"/block", "tok")
	if err != nil || string(body) != "whole block" {
		t.Fatalf("body %q err %v, want the body from the retry", body, err)
	}
}

// TestGetRawCancelDuringWaitKeepsWindowClosed checks that a caller whose ctx ends while waiting
// out a 429 gets its ctx error back without opening the shared backoff window.
func TestGetRawCancelDuringWaitKeepsWindowClosed(t *testing.T) {
	origWait, origJitter := rateLimitDefaultWait, rateLimitJitter
	rateLimitDefaultWait = time.Minute
	rateLimitJitter = func() time.Duration { return 0 }
	t.Cleanup(func() { rateLimitDefaultWait, rateLimitJitter = origWait, origJitter })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	c := &Client{tokenSource: func() (string, string) { return "uid", "access" }}
	if _, err := c.getRaw(ctx, server.URL+"/block", "tok"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want context.DeadlineExceeded", err)
	}
	if _, limited := c.RateLimited(); limited {
		t.Error("backoff window opened by a caller's own deadline")
	}
}

// fastRetries shrinks getRawRetrying's waits to a millisecond for the rest of the test.
func fastRetries(t *testing.T) {
	origWait, origJitter, origTransport := rateLimitDefaultWait, rateLimitJitter, transportRetryBase
	rateLimitDefaultWait = time.Millisecond
	rateLimitJitter = func() time.Duration { return 0 }
	transportRetryBase = time.Millisecond
	t.Cleanup(func() {
		rateLimitDefaultWait, rateLimitJitter, transportRetryBase = origWait, origJitter, origTransport
	})
}
