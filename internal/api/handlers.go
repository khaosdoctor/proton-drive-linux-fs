package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"slices"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/state"
)

// registerRoutes wires every endpoint onto mux. Go 1.22+'s ServeMux understands a method prefix
// in the pattern ("GET /v1/status"), so there is no need for a router dependency. done is closed
// when the server shuts down, so the streaming /v1/logs handler can stop its journalctl child
// and return instead of blocking Shutdown forever.
func registerRoutes(mux *http.ServeMux, deps Deps, done <-chan struct{}) {
	mux.HandleFunc("GET /v1/status", handleStatus(deps))
	mux.HandleFunc("GET /v1/transfers", handleTransfers(deps))
	mux.HandleFunc("GET /v1/cache", handleCacheStats(deps))
	mux.HandleFunc("POST /v1/cache/clear", handleCacheClear(deps))
	mux.HandleFunc("POST /v1/pause", handlePause(true))
	mux.HandleFunc("POST /v1/resume", handlePause(false))
	mux.HandleFunc("GET /v1/logs", handleLogs(done))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleStatus answers the same snapshot the tray polls from status.json, built live instead of
// read back from disk, so it is never stale.
func handleStatus(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, deps.Status())
	}
}

// handleTransfers answers just the current/recent slices, for a client that only cares about
// transfer progress and would rather not parse the whole status snapshot.
func handleTransfers(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		st := deps.Status()
		writeJSON(w, struct {
			Current []state.CurrentTransfer `json:"current"`
			Recent  []state.RecentTransfer  `json:"recent"`
		}{st.Current, st.Recent})
	}
}

// CacheStats is the response body for GET /v1/cache.
type CacheStats struct {
	Dir     string `json:"dir"`
	Entries int    `json:"entries"`
	Bytes   int64  `json:"bytes"`
}

func handleCacheStats(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if deps.CacheDir == "" {
			writeJSON(w, CacheStats{})
			return
		}

		entries, bytes := dirStats(deps.CacheDir)
		writeJSON(w, CacheStats{Dir: deps.CacheDir, Entries: entries, Bytes: bytes})
	}
}

// CacheClearResult is the response body for POST /v1/cache/clear.
type CacheClearResult struct {
	Freed int64 `json:"freed"`
}

func handleCacheClear(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if deps.ClearCache == nil {
			http.Error(w, "no cache configured", http.StatusNotFound)
			return
		}

		freed, err := deps.ClearCache()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		writeJSON(w, CacheClearResult{Freed: freed})
	}
}

// PauseResult is the response body for POST /v1/pause and /v1/resume.
type PauseResult struct {
	Paused bool `json:"paused"`
}

func handlePause(pause bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if err := state.SetPaused(pause); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, PauseResult{Paused: pause})
	}
}

// journalPriorities are the journalctl -p values accepted for the level query parameter: the
// named syslog priorities and their numeric equivalents (0-7).
var journalPriorities = []string{
	"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug",
	"0", "1", "2", "3", "4", "5", "6", "7",
}

func validLevel(level string) bool {
	return slices.Contains(journalPriorities, level)
}

// handleLogs streams the daemon's journal entries as Server-Sent Events, one journalctl JSON
// line per "data:" event. follow=1 tails live; level filters by priority (e.g. "debug"), 400 if
// it isn't one journalctl understands. Answers 501 when journalctl isn't installed, since there
// is no other log source to fall back to here.
func handleLogs(done <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		level := r.URL.Query().Get("level")
		if level != "" && !validLevel(level) {
			http.Error(w, "invalid level", http.StatusBadRequest)
			return
		}

		journalctl, err := exec.LookPath("journalctl")
		if err != nil {
			http.Error(w, "journalctl not available", http.StatusNotImplemented)
			return
		}

		args := []string{"--user", "-t", "proton-drive-fs", "-o", "json", "-n", "200"}
		if r.URL.Query().Get("follow") == "1" {
			args = append(args, "-f")
		}
		if level != "" {
			args = append(args, "-p", level)
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		// A plain r.Context() lasts as long as the client stays connected, which for a follow
		// request is indefinitely; http.Server.Shutdown never cancels it, so it would otherwise
		// block shutdown forever. Cancel on server shutdown (done) too, which kills the
		// journalctl child and unblocks the scan loop below.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func() {
			select {
			case <-done:
				cancel()
			case <-ctx.Done():
			}
		}()

		cmd := exec.CommandContext(ctx, journalctl, args...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := cmd.Start(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = cmd.Wait() }()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			if _, err := w.Write([]byte("data: " + scanner.Text() + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
