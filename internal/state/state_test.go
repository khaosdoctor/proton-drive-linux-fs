package state

import (
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
