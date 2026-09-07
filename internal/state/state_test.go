package state

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestResolveDir(t *testing.T) {
	tests := []struct {
		name       string
		runtimeDir string
		stateHome  string
		home       string
		want       string
	}{
		{"runtime dir wins", "/run/user/1000", "/home/u/.local/state", "/home/u", "/run/user/1000/proton-drive-fs"},
		{"state home next", "", "/home/u/.local/state", "/home/u", "/home/u/.local/state/proton-drive-fs"},
		{"home last", "", "", "/home/u", "/home/u/.local/state/proton-drive-fs"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveDir(tt.runtimeDir, tt.stateHome, tt.home); got != tt.want {
				t.Errorf("resolveDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStatusFresh(t *testing.T) {
	if (Status{}).Fresh() {
		t.Error("zero status should not be fresh")
	}
	if !(Status{Updated: time.Now().Unix()}).Fresh() {
		t.Error("just-written status should be fresh")
	}
	if (Status{Updated: time.Now().Add(-2 * StaleAfter).Unix()}).Fresh() {
		t.Error("old status should not be fresh")
	}
}

func TestPausedRoundTrip(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	if Paused() {
		t.Fatal("should start unpaused")
	}
	if err := SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if !Paused() {
		t.Fatal("should be paused after SetPaused(true)")
	}
	if err := SetPaused(false); err != nil {
		t.Fatal(err)
	}
	if Paused() {
		t.Fatal("should be unpaused after SetPaused(false)")
	}
	if err := SetPaused(false); err != nil {
		t.Fatal("removing an absent marker should succeed:", err)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	if _, ok := ReadStatus(); ok {
		t.Fatal("no status should be readable yet")
	}

	want := Status{
		Mountpoint: "/home/u/ProtonDrive", Version: "1.2.3", PID: 4242, Transfers: 2, Paused: true, Updated: time.Now().Unix(),
		Current: []CurrentTransfer{{Path: "a.txt", Action: "upload", Bytes: 10, Total: 20, Started: 1}},
		Recent:  []RecentTransfer{{Path: "b.txt", Action: "download", Status: "done", Bytes: 5, Finished: 2}},
	}
	if err := WriteStatus(want); err != nil {
		t.Fatal(err)
	}

	got, ok := ReadStatus()
	if !ok {
		t.Fatal("status should be readable after WriteStatus")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadStatus() = %+v, want %+v", got, want)
	}

	RemoveStatus()
	if _, ok := ReadStatus(); ok {
		t.Fatal("status should be gone after RemoveStatus")
	}
}

func TestAcquireLock(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	f1, err := AcquireLock()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := AcquireLock(); !errors.Is(err, ErrLocked) {
		t.Fatalf("second AcquireLock() = %v, want ErrLocked", err)
	}

	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}

	f2, err := AcquireLock()
	if err != nil {
		t.Fatalf("AcquireLock() after releasing the first lock: %v", err)
	}
	_ = f2.Close()
}

func TestFindRunningDaemon(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	// FindRunningDaemon checks a live PID's /proc/<pid>/exe against DaemonExeName; point that at
	// this test binary's own exe so "our own PID" reads as alive below.
	exe, err := os.Readlink("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	orig := DaemonExeName
	DaemonExeName = filepath.Base(exe)
	t.Cleanup(func() { DaemonExeName = orig })

	if _, _, alive := FindRunningDaemon(); alive {
		t.Fatal("no status published yet, should not report a running daemon")
	}

	// Our own PID is alive, and its exe matches DaemonExeName as set above.
	want := Status{PID: os.Getpid(), Mountpoint: "/home/u/ProtonDrive", Updated: time.Now().Unix()}
	if err := WriteStatus(want); err != nil {
		t.Fatal(err)
	}
	pid, mountpoint, alive := FindRunningDaemon()
	if !alive || pid != want.PID || mountpoint != want.Mountpoint {
		t.Fatalf("FindRunningDaemon() = (%d, %q, %v), want (%d, %q, true)", pid, mountpoint, alive, want.PID, want.Mountpoint)
	}

	// A PID that (almost certainly) doesn't exist should report as not alive, even though the
	// snapshot is otherwise fresh: status.json can be stale.
	if err := WriteStatus(Status{PID: 999999, Mountpoint: "/home/u/ProtonDrive", Updated: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if _, _, alive := FindRunningDaemon(); alive {
		t.Fatal("PID 999999 should not be alive")
	}
}

// TestFindRunningDaemonRejectsPIDReuse covers the case the exe check exists for: the recorded PID
// died without cleaning up status.json (SIGKILL, OOM) and the kernel later reused that PID for an
// unrelated process. Signal 0 alone can't tell that apart from the real daemon still running.
func TestFindRunningDaemonRejectsPIDReuse(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	// DaemonExeName is left at its default ("proton-drive-fs"), which the test binary running
	// this process certainly isn't, so this PID stands in for an unrelated process reusing an
	// old daemon's PID.
	if err := WriteStatus(Status{PID: os.Getpid(), Mountpoint: "/home/u/ProtonDrive", Updated: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if _, _, alive := FindRunningDaemon(); alive {
		t.Fatal("a live PID whose exe isn't proton-drive-fs should not be reported as alive")
	}
}
