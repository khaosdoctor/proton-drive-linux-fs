package fusefs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
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

func TestTrashingLocalFolderStaysLocal(t *testing.T) {
	st := newLocalState(t)
	d, raw := newRawTree(st)
	ctx := context.Background()

	var nm fuse.EntryOut
	if status := raw.Mkdir(nil, &fuse.MkdirIn{InHeader: fuse.InHeader{NodeId: 1}, Mode: 0o755}, "node_modules", &nm); !status.Ok() {
		t.Fatalf("Mkdir node_modules = %v", status)
	}
	if err := os.WriteFile(filepath.Join(st.local.Path, "node_modules", "index.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// trash-put renames into .Trash-1000/files, a drive folder, under a free name.
	trash := &dirNode{st: st, node: node("t", ".Trash-1000"), children: []*drive.Node{}, expires: time.Now().Add(time.Hour)}
	d.AddChild(".Trash-1000", d.NewPersistentInode(ctx, trash, fs.StableAttr{Mode: fuse.S_IFDIR}), true)
	if errno := d.Rename(ctx, "node_modules", trash, "node_modules_1", 0); errno != 0 {
		t.Fatalf("Rename into the trash = %v, want it moved locally", errno)
	}
	if !exists(filepath.Join(st.local.Path, ".Trash-1000", "node_modules_1", "index.js")) {
		t.Fatal("trashed folder not in the local store's trash")
	}
	if _, ok := listNames(t, trash)["node_modules_1"]; !ok {
		t.Error("trashed folder missing from the trash listing")
	}
	var out fuse.EntryOut
	if _, errno := trash.Lookup(ctx, "node_modules_1", &out); errno != 0 {
		t.Errorf("Lookup in the trash = %v", errno)
	}

	// Restoring moves it back; emptying the trash removes it.
	if errno := trash.Rename(ctx, "node_modules_1", d, "node_modules", 0); errno != 0 {
		t.Fatalf("restore = %v", errno)
	}
	if errno := d.Rename(ctx, "node_modules", trash, "node_modules_1", 0); errno != 0 {
		t.Fatalf("second trash = %v", errno)
	}
	if err := os.Remove(filepath.Join(st.local.Path, ".Trash-1000", "node_modules_1", "index.js")); err != nil {
		t.Fatal(err)
	}
	if errno := trash.Rmdir(ctx, "node_modules_1"); errno != 0 {
		t.Errorf("emptying the trash = %v", errno)
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
