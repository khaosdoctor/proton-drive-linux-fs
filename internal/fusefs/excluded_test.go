package fusefs

import (
	"context"
	"net/http"
	"os"
	"testing"

	proton "github.com/henrybear327/go-proton-api"
)

func TestUnmountAndForgetLetGoOfKeptBuffers(t *testing.T) {
	st := &mountState{excludes: parseExcludes([]string{"*.tmp"})}
	exists := func(name string) bool {
		_, err := os.Stat(name)
		return err == nil
	}
	parkExcluded := func(name string) (*fileNode, string) {
		fn, h := newDelayedHandle(t, st, name)
		buf := h.tmp.Name()
		if errno := h.Release(context.Background()); errno != 0 {
			t.Fatalf("Release = %v, want 0", errno)
		}
		return fn, buf
	}

	forgotten, forgottenBuf := parkExcluded("a.3mf.tmp")
	_, unmountedBuf := parkExcluded("b.3mf.tmp")
	_, failed := newDelayedHandle(t, st, "c.3mf")
	failedBuf := failed.tmp.Name()
	failed.keepForRetry(failed.sharedTmp(), "c.3mf", proton.APIError{Status: http.StatusNotFound})

	forgotten.OnForget()
	if exists(forgottenBuf) {
		t.Error("excluded buffer of a forgotten file still in $TMPDIR")
	}

	st.flushDelayed()
	if exists(unmountedBuf) {
		t.Error("excluded buffer still in $TMPDIR after unmount")
	}
	if !exists(failedBuf) {
		t.Error("unmount removed the only copy of a save whose upload failed for good")
	}
	if len(st.kept) != 0 {
		t.Errorf("%d buffers still kept after unmount", len(st.kept))
	}
}

func TestReleaseParksExcludedNameUntilDropped(t *testing.T) {
	st := &mountState{excludes: parseExcludes([]string{"*.tmp"})}
	fn := &fileNode{parent: &dirNode{st: st}, name: "model.3mf.tmp"}
	tmp, err := os.CreateTemp(t.TempDir(), "buf")
	if err != nil {
		t.Fatal(err)
	}
	h := &fileHandle{node: fn, tmp: tmp, dirty: true, owns: true}
	fn.handle = h

	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("Release = %v, want 0", errno)
	}
	if fn.parkedHandle() != h {
		t.Fatal("excluded name was not parked")
	}
	if _, err := os.Stat(tmp.Name()); err != nil {
		t.Fatalf("parked buffer removed: %v", err)
	}

	h.unpark(false)
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("second Release = %v, want 0", errno)
	}
	if fn.handle != nil {
		t.Error("dropped handle still attached to the node")
	}
	if _, err := os.Stat(tmp.Name()); !os.IsNotExist(err) {
		t.Errorf("dropped buffer still on disk: %v", err)
	}
}
