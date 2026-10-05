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

func TestReaderOutlivesTheDroppedBuffer(t *testing.T) {
	fn, h := newDelayedHandle(t, &mountState{}, "model.FCStd")
	if _, err := h.tmp.WriteString("saved"); err != nil {
		t.Fatal(err)
	}

	r := readerOf(fn, h)
	if r == nil {
		t.Fatal("no reader for a buffered file")
	}

	h.drop(h.sharedTmp())
	if fn.handle != nil {
		t.Error("dropped handle still attached to the node")
	}

	buf := make([]byte, 5)
	res, errno := r.Read(context.Background(), buf, 0)
	if errno != 0 {
		t.Fatalf("read after drop = %v, want the buffered bytes", errno)
	}
	if got, _ := res.Bytes(buf); string(got) != "saved" {
		t.Errorf("read %q, want %q", got, "saved")
	}
	if errno := r.Release(context.Background()); errno != 0 {
		t.Errorf("reader Release = %v", errno)
	}
}

func TestKeepForRetryParksWithATimer(t *testing.T) {
	st := &mountState{}
	fn, h := newDelayedHandle(t, st, "model.FCStd")

	h.keepForRetry(h.sharedTmp(), "model.FCStd")
	defer h.unpark(false)

	if fn.parkedHandle() != h || h.timer == nil || len(st.delayed) != 1 {
		t.Fatal("failed upload was not kept for a retry")
	}
	if _, err := os.Stat(h.sharedTmp().Name()); err != nil {
		t.Errorf("buffer of a failed upload removed: %v", err)
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
