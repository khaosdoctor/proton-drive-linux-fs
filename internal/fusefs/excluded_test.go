package fusefs

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hanwen/go-fuse/v2/fuse"
	proton "github.com/henrybear327/go-proton-api"
)

func exists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

func TestForgottenPendingFileKeepsItsBuffer(t *testing.T) {
	st := &mountState{excludes: parseExcludes([]string{"*.tmp"})}
	d, raw := newRawTree(st)

	// Orca writes "x.3mf.tmp" (excluded, so parked with no timer), and the kernel forgets the
	// inode before Orca renames it.
	var created fuse.CreateOut
	in := &fuse.CreateIn{InHeader: fuse.InHeader{NodeId: 1}, Flags: uint32(os.O_WRONLY), Mode: 0o644}
	if status := raw.Create(nil, in, "x.3mf.tmp", &created); !status.Ok() {
		t.Fatalf("Create = %v", status)
	}
	fn := d.fileChild("x.3mf.tmp")
	buf := fn.handle.tmp.Name()
	raw.Release(nil, &fuse.ReleaseIn{InHeader: fuse.InHeader{NodeId: created.NodeId}, Fh: created.Fh})
	raw.Forget(created.NodeId, 1)

	var entry fuse.EntryOut
	if status := raw.Lookup(nil, &fuse.InHeader{NodeId: 1}, "x.3mf.tmp", &entry); !status.Ok() {
		t.Fatalf("Lookup after forget = %v, want the pending file still there", status)
	}
	if d.pendingChild("x.3mf.tmp") != fn || !exists(buf) {
		t.Fatal("forgotten pending file lost its buffer")
	}

	st.flushDelayed()
	if exists(buf) {
		t.Error("excluded buffer still in $TMPDIR after unmount")
	}
}

func TestUnmountLetsGoOfKeptBuffers(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	st := &mountState{excludes: parseExcludes([]string{"*.tmp"})}
	_, h := newDelayedHandle(t, st, "b.3mf.tmp")
	unmountedBuf := h.tmp.Name()
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("Release = %v, want 0", errno)
	}
	_, failed := newDelayedHandle(t, st, "c.3mf")
	failedBuf := failed.tmp.Name()
	failed.keepForRetry(failed.sharedTmp(), "c.3mf", proton.APIError{Status: http.StatusUnprocessableEntity, Code: proton.AFileOrFolderNameExist})

	st.flushDelayed()
	if exists(unmountedBuf) {
		t.Error("excluded buffer still in $TMPDIR after unmount")
	}
	saved, _ := filepath.Glob(filepath.Join(stateHome, "proton-drive-fs", "unsaved", "*-c.3mf"))
	if exists(failedBuf) || len(saved) != 1 {
		t.Error("unmount did not move the only copy of a save whose upload failed for good out of $TMPDIR")
	}
	if len(st.kept) != 0 {
		t.Errorf("%d buffers still kept after unmount", len(st.kept))
	}
}

func TestSaveUnsavedFitsAMaximalName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	tmp, err := os.CreateTemp(t.TempDir(), "buf")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tmp.Close() }()

	name := strings.Repeat("é", 127) + "x" // 255 bytes, the longest name a file can have
	saved, err := saveUnsaved(tmp, name)
	if err != nil {
		t.Fatalf("saveUnsaved with a %d-byte name = %v, want the buffer moved", len(name), err)
	}
	if base := filepath.Base(saved); !utf8.ValidString(base) || len(base) > 255 {
		t.Errorf("saved as %q, want a valid name of at most 255 bytes", base)
	}
	if exists(tmp.Name()) {
		t.Error("buffer still in the temp dir")
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
