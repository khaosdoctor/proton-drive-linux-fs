package fusefs

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestDelayForFirstRuleWins(t *testing.T) {
	st := &mountState{
		uploadDelay: 5 * time.Second,
		delayRules:  parseDelayRules([]string{"*.FCBak=1m", `re:\.bak$=0s`, "*.FCBak=2m", "broken"}),
	}

	cases := map[string]time.Duration{
		"x.20261005-151648.FCBak": time.Minute,
		"notes.bak":               0,
		"x.FCStd":                 5 * time.Second,
	}
	for name, want := range cases {
		if got := st.delayFor(name); got != want {
			t.Errorf("delayFor(%q) = %v, want %v", name, got, want)
		}
	}
}

func newDelayedHandle(t *testing.T, st *mountState, name string) (*fileNode, *fileHandle) {
	t.Helper()
	fn := &fileNode{parent: &dirNode{st: st}, name: name}
	tmp, err := os.CreateTemp(t.TempDir(), "buf")
	if err != nil {
		t.Fatal(err)
	}
	h := &fileHandle{node: fn, tmp: tmp, dirty: true, owns: true}
	fn.handle = h
	return fn, h
}

func TestReleaseDelaysUploadAndReopenTakesItBack(t *testing.T) {
	st := &mountState{uploadDelay: time.Hour}
	fn, h := newDelayedHandle(t, st, "model.FCStd")

	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("Release = %v, want 0", errno)
	}
	if fn.parkedHandle() != h || len(st.delayed) != 1 {
		t.Fatal("released file was not held for its upload delay")
	}

	got, _, errno := fn.Open(context.Background(), syscall.O_WRONLY)
	if errno != 0 || got != h {
		t.Fatalf("Open = %v, %v; want the parked handle back", got, errno)
	}
	if fn.parkedHandle() != nil || len(st.delayed) != 0 {
		t.Error("reopened handle still parked or still waiting to upload")
	}
	if !h.hasTmp() {
		t.Error("reopened handle lost its buffer")
	}
}

func TestSettleUnderExcludedNameParksAgain(t *testing.T) {
	st := &mountState{uploadDelay: time.Hour, excludes: parseExcludes([]string{"*.tmp"})}
	fn, h := newDelayedHandle(t, st, "model.FCStd")

	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("Release = %v, want 0", errno)
	}

	// Renamed to an excluded name while waiting: the settle must not upload it.
	fn.name = "model.tmp"
	h.settle()

	if fn.parkedHandle() != h {
		t.Fatal("excluded name was not parked after the delay ran out")
	}
	if h.timer != nil || len(st.delayed) != 0 {
		t.Error("excluded buffer still has an upload timer")
	}
}
