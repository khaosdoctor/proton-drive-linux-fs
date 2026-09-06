// Package api serves the mount daemon's local control API: status, transfer progress, cache
// stats, pause/resume, and a log stream. It listens on a unix socket instead of TCP, and relies
// on the socket's file permissions (mode 0600, owner-only) for access control the same way
// status.json and the pause marker already do; there is no separate authentication.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/state"
)

// shutdownGrace bounds how long Close waits for in-flight requests (an SSE log stream, say) to
// notice s.done and return on their own before the listener is force-closed under them.
const shutdownGrace = 2 * time.Second

// SocketName is the unix socket's filename under the runtime directory.
const SocketName = "api.sock"

// SocketPath returns where the local API listens: $XDG_RUNTIME_DIR/proton-drive-fs/api.sock,
// falling back to $XDG_STATE_HOME/proton-drive-fs (or ~/.local/state/proton-drive-fs) — the same
// directory state.StatusPath and state.PausePath use.
func SocketPath() (string, error) {
	dir, err := state.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, SocketName), nil
}

// Deps is what the API needs from the mount to answer requests. A nil ClearCache disables
// /v1/cache and POST /v1/cache/clear.
type Deps struct {
	// Status returns a fresh status snapshot, built live rather than read back from disk.
	Status func() state.Status

	// CacheDir is where downloaded blocks are cached on disk; "" means there is no cache.
	CacheDir string

	// ClearCache removes the cache directory's contents, keeping the directory itself, resets
	// whatever in-memory size counter tracks it, and returns the bytes freed.
	ClearCache func() (int64, error)
}

// Server is the local API listening on a unix socket.
type Server struct {
	listener net.Listener
	http     *http.Server
	sockPath string

	// done is closed by Close so a streaming handler (GET /v1/logs) can stop and let Shutdown
	// return instead of blocking on it forever.
	done chan struct{}
}

// Start opens the unix socket at SocketPath (mode 0600) and begins serving in the background.
// Call Close to stop and remove the socket.
func Start(deps Deps) (*Server, error) {
	sockPath, err := SocketPath()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, err
	}

	removeStaleSocket(sockPath)

	// net.Listen gives a unix socket file the mode umask leaves it, with no way to pass a mode
	// directly; narrow the umask for the call so nothing else can open it in between, then
	// restore it immediately and chmod belt-and-braces in case another process's umask raced us.
	oldUmask := syscall.Umask(0o077)
	l, err := net.Listen("unix", sockPath)
	syscall.Umask(oldUmask)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = l.Close()
		_ = os.Remove(sockPath)
		return nil, err
	}

	s := &Server{listener: l, sockPath: sockPath, done: make(chan struct{})}

	mux := http.NewServeMux()
	registerRoutes(mux, deps, s.done)
	s.http = &http.Server{Handler: mux}

	go func() {
		if err := s.http.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("local api server stopped", "err", err)
		}
	}()

	return s, nil
}

// removeStaleSocket removes sockPath if it exists but nothing answers on it (ECONNREFUSED),
// meaning it was left behind by a daemon that crashed without cleaning up. A socket that does
// answer is left alone; net.Listen then fails on it naturally, which is correct when another
// instance is actually running.
func removeStaleSocket(sockPath string) {
	conn, err := net.Dial("unix", sockPath)
	if err == nil {
		_ = conn.Close()
		return
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		_ = os.Remove(sockPath)
	}
}

// Close stops serving and removes the socket file. It signals done first so a streaming handler
// can return on its own, gives Shutdown a couple of seconds to let every handler finish
// gracefully, and force-closes whatever is left rather than blocking forever.
func (s *Server) Close() error {
	close(s.done)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	err := s.http.Shutdown(ctx)
	if err != nil {
		_ = s.http.Close()
	}

	_ = os.Remove(s.sockPath)
	return err
}
