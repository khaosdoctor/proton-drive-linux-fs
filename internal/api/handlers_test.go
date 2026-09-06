package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/state"
)

func newTestServer(deps Deps) *httptest.Server {
	mux := http.NewServeMux()
	registerRoutes(mux, deps, make(chan struct{}))
	return httptest.NewServer(mux)
}

func TestHandleStatus(t *testing.T) {
	want := state.Status{Mountpoint: "/home/u/ProtonDrive", Version: "1.2.3", PID: 42, Transfers: 1}
	srv := newTestServer(Deps{Status: func() state.Status { return want }})
	defer srv.Close()

	res, err := http.Get(srv.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	var got state.Status
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Mountpoint != want.Mountpoint || got.Version != want.Version || got.PID != want.PID {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestHandleTransfers(t *testing.T) {
	want := state.Status{
		Current: []state.CurrentTransfer{{Path: "a.txt", Action: "upload", Bytes: 5, Total: 10}},
		Recent:  []state.RecentTransfer{{Path: "b.txt", Action: "download", Status: "done", Bytes: 20}},
	}
	srv := newTestServer(Deps{Status: func() state.Status { return want }})
	defer srv.Close()

	res, err := http.Get(srv.URL + "/v1/transfers")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	var got struct {
		Current []state.CurrentTransfer `json:"current"`
		Recent  []state.RecentTransfer  `json:"recent"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Current) != 1 || got.Current[0].Path != "a.txt" {
		t.Fatalf("current = %+v", got.Current)
	}
	if len(got.Recent) != 1 || got.Recent[0].Status != "done" {
		t.Fatalf("recent = %+v", got.Recent)
	}
}

func TestHandleCacheStats(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "block1"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "block2"), []byte("worldly"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(Deps{Status: func() state.Status { return state.Status{} }, CacheDir: dir})
	defer srv.Close()

	res, err := http.Get(srv.URL + "/v1/cache")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	var got CacheStats
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Entries != 2 || got.Bytes != int64(len("hello")+len("worldly")) {
		t.Fatalf("got %+v", got)
	}
}

func TestHandleCacheClear(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "block1"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	var resetCalled bool
	deps := Deps{
		Status:   func() state.Status { return state.Status{} },
		CacheDir: dir,
		ClearCache: func() (int64, error) {
			freed, err := ClearCacheDir(dir)
			resetCalled = true
			return freed, err
		},
	}
	srv := newTestServer(deps)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/cache/clear", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	var got CacheClearResult
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Freed != int64(len("hello")) {
		t.Fatalf("freed = %d, want %d", got.Freed, len("hello"))
	}
	if !resetCalled {
		t.Fatal("ClearCache was not called")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dir still has %d entries", len(entries))
	}
}

func TestHandleCacheClearDisabled(t *testing.T) {
	srv := newTestServer(Deps{Status: func() state.Status { return state.Status{} }})
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/cache/clear", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

func TestHandlePauseResume(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	srv := newTestServer(Deps{Status: func() state.Status { return state.Status{} }})
	defer srv.Close()

	if _, err := http.Post(srv.URL+"/v1/pause", "", nil); err != nil {
		t.Fatal(err)
	}
	if !state.Paused() {
		t.Fatal("expected paused after POST /v1/pause")
	}

	if _, err := http.Post(srv.URL+"/v1/resume", "", nil); err != nil {
		t.Fatal(err)
	}
	if state.Paused() {
		t.Fatal("expected not paused after POST /v1/resume")
	}
}

func TestHandleLogsMissingJournalctl(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // a PATH with no journalctl on it

	srv := newTestServer(Deps{Status: func() state.Status { return state.Status{} }})
	defer srv.Close()

	res, err := http.Get(srv.URL + "/v1/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", res.StatusCode)
	}
}

func TestHandleLogsInvalidLevel(t *testing.T) {
	srv := newTestServer(Deps{Status: func() state.Status { return state.Status{} }})
	defer srv.Close()

	res, err := http.Get(srv.URL + "/v1/logs?level=bogus")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestValidLevel(t *testing.T) {
	for _, level := range []string{"emerg", "debug", "0", "7"} {
		if !validLevel(level) {
			t.Errorf("validLevel(%q) = false, want true", level)
		}
	}
	for _, level := range []string{"", "bogus", "8", "DEBUG"} {
		if validLevel(level) {
			t.Errorf("validLevel(%q) = true, want false", level)
		}
	}
}

// TestServerCloseStopsStreamingLogs guards against Close hanging forever behind an SSE client
// that never disconnects: a follow=1 request must stop, and Close must return, once Close is
// called, whether or not journalctl itself produced anything in this environment.
func TestServerCloseStopsStreamingLogs(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	srv, err := Start(Deps{Status: func() state.Status { return state.Status{} }})
	if err != nil {
		t.Fatal(err)
	}

	client, err := NewClient()
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, "http://unix/v1/logs?follow=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	bodyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, res.Body)
		close(bodyDone)
	}()

	closeErr := make(chan error, 1)
	go func() { closeErr <- srv.Close() }()

	select {
	case err := <-closeErr:
		if err != nil {
			t.Fatalf("Close() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return within 5s; a streaming client blocked shutdown")
	}

	select {
	case <-bodyDone:
	case <-time.After(5 * time.Second):
		t.Fatal("streaming response body never closed")
	}
}

func TestDirStats(t *testing.T) {
	dir := t.TempDir()
	if entries, bytes := dirStats(dir); entries != 0 || bytes != 0 {
		t.Fatalf("empty dir: entries=%d bytes=%d", entries, bytes)
	}

	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "g"), []byte("56"), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, bytes := dirStats(dir)
	if entries != 2 || bytes != 6 {
		t.Fatalf("entries=%d bytes=%d, want 2 6", entries, bytes)
	}
}

func TestClearCacheDirEmptyPath(t *testing.T) {
	freed, err := ClearCacheDir("")
	if err != nil || freed != 0 {
		t.Fatalf("freed=%d err=%v, want 0 nil", freed, err)
	}
}
