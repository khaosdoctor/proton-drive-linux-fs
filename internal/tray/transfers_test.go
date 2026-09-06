package tray

import (
	"testing"
	"time"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/state"
)

func TestTransferLine(t *testing.T) {
	tests := []struct {
		name string
		t    state.CurrentTransfer
		want string
	}{
		{"upload with percent", state.CurrentTransfer{Path: "Files/report.pdf", Action: "upload", Bytes: 40, Total: 100}, "Uploading Files/report.pdf 40%"},
		{"download with percent", state.CurrentTransfer{Path: "movie.mp4", Action: "download", Bytes: 25, Total: 100}, "Downloading movie.mp4 25%"},
		{"unknown total", state.CurrentTransfer{Path: "a.txt", Action: "upload", Bytes: 5, Total: 0}, "Uploading a.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := transferLine(tt.t); got != tt.want {
				t.Errorf("transferLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTooltipFor(t *testing.T) {
	noTransfers := snapshot{loggedIn: true, mounted: true}
	if got, want := tooltipFor(noTransfers, "/home/u/ProtonDrive"), "Mounted at /home/u/ProtonDrive"; got != want {
		t.Errorf("tooltipFor() = %q, want %q", got, want)
	}

	one := snapshot{current: []state.CurrentTransfer{{Path: "report.pdf", Action: "upload", Bytes: 40, Total: 100}}}
	if got, want := tooltipFor(one, ""), "Uploading report.pdf 40%"; got != want {
		t.Errorf("tooltipFor() = %q, want %q", got, want)
	}

	several := snapshot{current: []state.CurrentTransfer{
		{Path: "report.pdf", Action: "upload", Bytes: 40, Total: 100},
		{Path: "b.txt", Action: "upload"},
		{Path: "c.txt", Action: "download"},
	}}
	if got, want := tooltipFor(several, ""), "Uploading report.pdf 40% and 2 more"; got != want {
		t.Errorf("tooltipFor() = %q, want %q", got, want)
	}
}

func TestRecentLine(t *testing.T) {
	done := state.RecentTransfer{Path: "a.txt", Status: "done", Bytes: 1024}
	if got, want := recentLine(done), "✓ a.txt (1.0 KiB)"; got != want {
		t.Errorf("recentLine() = %q, want %q", got, want)
	}

	failed := state.RecentTransfer{Path: "b.txt", Status: "failed", Err: "timeout"}
	if got, want := recentLine(failed), "✗ b.txt: timeout"; got != want {
		t.Errorf("recentLine() = %q, want %q", got, want)
	}
}

func TestRefreshInterval(t *testing.T) {
	if got := refreshInterval(snapshot{}); got != 2*time.Second {
		t.Errorf("refreshInterval() = %v, want 2s", got)
	}
	active := snapshot{current: []state.CurrentTransfer{{Path: "a"}}}
	if got := refreshInterval(active); got != time.Second {
		t.Errorf("refreshInterval() = %v, want 1s", got)
	}
}

// TestReadStatusFallsBackWithNoSocket checks readStatus falls back to status.json when nothing
// is listening on the local API socket, e.g. an older daemon build without the API.
func TestReadStatusFallsBackWithNoSocket(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	want := state.Status{Mountpoint: "/home/u/ProtonDrive", Version: "1.2.3", PID: 99, Updated: time.Now().Unix()}
	if err := state.WriteStatus(want); err != nil {
		t.Fatal(err)
	}

	st, fresh := readStatus()
	if !fresh {
		t.Fatal("expected fresh=true from the status.json fallback")
	}
	if st.PID != want.PID || st.Version != want.Version {
		t.Fatalf("got %+v, want %+v", st, want)
	}
}

func TestReadStatusNoSocketNoFile(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	if _, fresh := readStatus(); fresh {
		t.Fatal("expected fresh=false with neither a socket nor a status file")
	}
}

// TestSetPausedViaAPIOrFileFallsBackWithNoSocket checks the pause/resume path falls back to the
// pause file directly when no daemon answers on the socket.
func TestSetPausedViaAPIOrFileFallsBackWithNoSocket(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	if err := setPausedViaAPIOrFile(true); err != nil {
		t.Fatal(err)
	}
	if !state.Paused() {
		t.Fatal("expected the pause file to be set")
	}

	if err := setPausedViaAPIOrFile(false); err != nil {
		t.Fatal(err)
	}
	if state.Paused() {
		t.Fatal("expected the pause file to be cleared")
	}
}
