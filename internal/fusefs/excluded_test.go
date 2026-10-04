package fusefs

import (
	"context"
	"os"
	"testing"
)

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
