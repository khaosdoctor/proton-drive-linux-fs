package fusefs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func newLocalState(t *testing.T) *mountState {
	t.Helper()
	dir := t.TempDir()
	var stat syscall.Stat_t
	if err := syscall.Stat(dir, &stat); err != nil {
		t.Fatal(err)
	}
	return &mountState{
		excludes: parseExcludes([]string{"node_modules", "*.tmp"}),
		local:    &fs.LoopbackRoot{Path: dir, Dev: stat.Dev},
	}
}

func listNames(t *testing.T, d *dirNode) map[string]uint32 {
	t.Helper()
	stream, errno := d.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("Readdir = %v", errno)
	}
	names := map[string]uint32{}
	for stream.HasNext() {
		e, _ := stream.Next()
		names[e.Name] = e.Mode
	}
	return names
}

func TestExcludedFolderIsLocalAndListed(t *testing.T) {
	st := newLocalState(t)
	d, raw := newRawTree(st)

	// npm install: mkdir -p node_modules/pkg, then write node_modules/pkg/index.js.
	var nm, pkg fuse.EntryOut
	if status := raw.Mkdir(nil, &fuse.MkdirIn{InHeader: fuse.InHeader{NodeId: 1}, Mode: 0o755}, "node_modules", &nm); !status.Ok() {
		t.Fatalf("Mkdir node_modules = %v", status)
	}
	if status := raw.Mkdir(nil, &fuse.MkdirIn{InHeader: fuse.InHeader{NodeId: nm.NodeId}, Mode: 0o755}, "pkg", &pkg); !status.Ok() {
		t.Fatalf("Mkdir pkg = %v", status)
	}
	var created fuse.CreateOut
	in := &fuse.CreateIn{InHeader: fuse.InHeader{NodeId: pkg.NodeId}, Flags: uint32(os.O_WRONLY), Mode: 0o644}
	if status := raw.Create(nil, in, "index.js", &created); !status.Ok() {
		t.Fatalf("Create = %v", status)
	}
	if _, status := raw.Write(nil, &fuse.WriteIn{InHeader: fuse.InHeader{NodeId: created.NodeId}, Fh: created.Fh}, []byte("test")); !status.Ok() {
		t.Fatalf("Write = %v", status)
	}
	raw.Release(nil, &fuse.ReleaseIn{InHeader: fuse.InHeader{NodeId: created.NodeId}, Fh: created.Fh})

	stored := filepath.Join(st.local.Path, "node_modules", "pkg", "index.js")
	if b, err := os.ReadFile(stored); err != nil || string(b) != "test" {
		t.Fatalf("local store has %q, %v; want the written file", b, err)
	}
	if mode, ok := listNames(t, d)["node_modules"]; !ok || mode&syscall.S_IFMT != syscall.S_IFDIR {
		t.Errorf("listing has node_modules = %v (mode %o), want a folder", ok, mode)
	}

	// A fresh mount over the same store finds it again.
	_, raw2 := newRawTree(st)
	var again fuse.EntryOut
	if status := raw2.Lookup(nil, &fuse.InHeader{NodeId: 1}, "node_modules", &again); !status.Ok() {
		t.Fatalf("Lookup after remount = %v", status)
	}

	// Renaming it to an uploadable name has to be a copy.
	if errno := d.Rename(context.Background(), "node_modules", d, "deps", 0); errno != syscall.EXDEV {
		t.Errorf("Rename to an uploadable name = %v, want EXDEV", errno)
	}
	if errno := d.Rmdir(context.Background(), "node_modules"); errno != syscall.ENOTEMPTY {
		t.Errorf("Rmdir of a non-empty local folder = %v, want ENOTEMPTY", errno)
	}
}

func TestUnmountKeepsExcludedFileLocally(t *testing.T) {
	st := newLocalState(t)
	d, raw := newRawTree(st)

	var created fuse.CreateOut
	in := &fuse.CreateIn{InHeader: fuse.InHeader{NodeId: 1}, Flags: uint32(os.O_WRONLY), Mode: 0o644}
	if status := raw.Create(nil, in, "x.tmp", &created); !status.Ok() {
		t.Fatalf("Create = %v", status)
	}
	raw.Release(nil, &fuse.ReleaseIn{InHeader: fuse.InHeader{NodeId: created.NodeId}, Fh: created.Fh})
	if _, ok := listNames(t, d)["x.tmp"]; !ok {
		t.Error("pending excluded file missing from the listing")
	}

	st.flushDelayed()
	if !exists(filepath.Join(st.local.Path, "x.tmp")) {
		t.Fatal("unmount did not keep the excluded file in the local store")
	}
	if _, ok := listNames(t, d)["x.tmp"]; !ok {
		t.Error("stored excluded file missing from the listing")
	}
}
