// Package state holds the two small runtime files the mount process and the tray use to
// talk to each other: a pause marker the mount checks before polling, and a status
// snapshot the mount writes and the tray reads.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// StaleAfter is how long a status snapshot stays meaningful. A snapshot older than this
// means the mount process is gone or wedged.
const StaleAfter = 10 * time.Second

// Status is the snapshot the mount process publishes once per second.
type Status struct {
	Mountpoint string `json:"mountpoint"`
	Version    string `json:"version"`
	PID        int    `json:"pid"`
	Transfers  int64  `json:"transfers"`
	Paused     bool   `json:"paused"`
	Updated    int64  `json:"updated"`
}

// Fresh reports whether the snapshot is recent enough to trust.
func (s Status) Fresh() bool {
	return s.Updated > 0 && time.Since(time.Unix(s.Updated, 0)) < StaleAfter
}

// resolveDir picks the directory for the runtime files: the session runtime directory when
// there is one, then the state home, then ~/.local/state.
func resolveDir(runtimeDir, stateHome, home string) string {
	base := runtimeDir
	if base == "" {
		base = stateHome
	}
	if base == "" {
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "proton-drive-fs")
}

func dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil && os.Getenv("XDG_RUNTIME_DIR") == "" && os.Getenv("XDG_STATE_HOME") == "" {
		return "", err
	}
	return resolveDir(os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("XDG_STATE_HOME"), home), nil
}

// PausePath returns the path of the pause marker.
func PausePath() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "paused"), nil
}

// StatusPath returns the path of the status snapshot.
func StatusPath() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "status.json"), nil
}

// Paused reports whether the pause marker exists. It never fails: an unreadable path counts
// as not paused so polling keeps working.
func Paused() bool {
	path, err := PausePath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// SetPaused creates or removes the pause marker.
func SetPaused(paused bool) error {
	path, err := PausePath()
	if err != nil {
		return err
	}

	if !paused {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// ReadStatus returns the published snapshot. ok is false when there is none to read.
func ReadStatus() (Status, bool) {
	path, err := StatusPath()
	if err != nil {
		return Status{}, false
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Status{}, false
	}

	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return Status{}, false
	}
	return s, true
}

// WriteStatus publishes s, replacing any previous snapshot in one rename so a reader never
// sees a half-written file.
func WriteStatus(s Status) error {
	path, err := StatusPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	data, err := json.Marshal(s)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "status-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
}

// RemoveStatus drops the snapshot, so readers stop believing a mount is running.
func RemoveStatus() {
	path, err := StatusPath()
	if err != nil {
		return
	}
	_ = os.Remove(path)
}
