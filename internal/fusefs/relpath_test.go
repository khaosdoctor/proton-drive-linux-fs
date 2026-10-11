package fusefs

import (
	"context"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// TestRenamedDirPathFollowsTheTree checks a folder's path, and the path of every folder under
// it, follows a rename of that folder instead of keeping the name it was looked up under.
func TestRenamedDirPathFollowsTheTree(t *testing.T) {
	ctx := context.Background()
	root := &dirNode{node: node("root", "root")}
	fs.NewNodeFS(root, &fs.Options{}) // builds the inode tree without mounting it

	a := &dirNode{node: node("a", "a")}
	root.AddChild("a", root.NewPersistentInode(ctx, a, fs.StableAttr{Mode: fuse.S_IFDIR, Ino: 2}), true)
	b := &dirNode{node: node("b", "b")}
	a.AddChild("b", a.NewPersistentInode(ctx, b, fs.StableAttr{Mode: fuse.S_IFDIR, Ino: 3}), true)

	if got := b.relPath(); got != "a/b" {
		t.Fatalf("b.relPath() = %q before the rename, want a/b", got)
	}

	// What go-fuse does to the tree once a Rename handler returns success.
	root.MvChild("a", root.EmbeddedInode(), "z", true)

	if got := a.relPath(); got != "z" {
		t.Errorf("a.relPath() = %q, want z", got)
	}
	if got := b.relPath(); got != "z/b" {
		t.Errorf("b.relPath() = %q, want z/b", got)
	}
	if got := root.relPath(); got != "" {
		t.Errorf("root.relPath() = %q, want empty", got)
	}
}

// TestDetachedDirIsNotInTree checks a folder removed from the tree (Rmdir, or a rename over it)
// reports itself detached, so finishLoad does not key its thumbnails under the root's "" path.
func TestDetachedDirIsNotInTree(t *testing.T) {
	ctx := context.Background()
	root := &dirNode{node: node("root", "root")}
	fs.NewNodeFS(root, &fs.Options{})

	a := &dirNode{node: node("a", "a")}
	root.AddChild("a", root.NewPersistentInode(ctx, a, fs.StableAttr{Mode: fuse.S_IFDIR, Ino: 2}), true)
	if !root.inTree() || !a.inTree() {
		t.Fatal("root and a should both be in the tree")
	}

	// What go-fuse does to the tree once an Rmdir handler returns success.
	root.RmChild("a")
	if a.inTree() {
		t.Fatalf("a.inTree() = true after removal (relPath %q), want false", a.relPath())
	}
}
