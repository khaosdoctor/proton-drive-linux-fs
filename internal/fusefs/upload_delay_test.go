package fusefs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
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

// newTreeDir makes d the root of an in-memory go-fuse inode tree (no kernel mount) with an empty,
// fresh listing, so handlers that find children by name run without a drive.
func newTreeDir(st *mountState) *dirNode {
	d, _ := newRawTree(st)
	return d
}

// newRawTree is newTreeDir plus go-fuse's raw filesystem for it, so a test can make the calls the
// kernel would (Create, Release, Forget, Lookup) against node id 1, the root.
func newRawTree(st *mountState) (*dirNode, fuse.RawFileSystem) {
	d := &dirNode{
		st:       st,
		node:     &drive.Node{},
		children: []*drive.Node{},
		expires:  time.Now().Add(time.Hour),
	}
	return d, fs.NewNodeFS(d, nil)
}

// addPending mounts a pending file with a dirty, still open write buffer under name in d.
func addPending(t *testing.T, d *dirNode, name string) (*fileNode, *fileHandle) {
	t.Helper()
	fn, h := newDelayedHandle(t, d.st, name)
	fn.parent = d
	d.AddChild(name, d.NewPersistentInode(context.Background(), fn, fs.StableAttr{Mode: fuse.S_IFREG}), true)
	return fn, h
}

func TestRenamePendingOverPendingDropsTheReplacedBuffer(t *testing.T) {
	st := &mountState{uploadDelay: time.Hour}
	d := newTreeDir(st)
	ctx := context.Background()

	// Two atomic saves within the upload delay: x.tmp1 -> x, then x.tmp2 -> x.
	first, h1 := addPending(t, d, "x")
	second, h2 := addPending(t, d, "x.tmp2")
	buf1 := h1.tmp.Name()
	_ = h1.Release(ctx)
	_ = h2.Release(ctx)
	defer h2.unpark(false)

	if errno := d.Rename(ctx, "x.tmp2", d, "x", 0); errno != 0 {
		t.Fatalf("Rename = %v, want 0", errno)
	}

	if first.parkedHandle() != nil || first.handle != nil {
		t.Error("replaced file still holds a buffer that would upload")
	}
	if _, err := os.Stat(buf1); !os.IsNotExist(err) {
		t.Errorf("replaced buffer still on disk: %v", err)
	}
	if len(st.delayed) != 1 || second.parkedHandle() != h2 || second.currentName() != "x" {
		t.Error("renamed file is not the only one waiting to upload, under its new name")
	}
	if second.replaces != first {
		t.Error("renamed file does not wait for an upload of the file it replaced")
	}
}

func TestUnlinkPendingWithOpenWriterDropsItOnClose(t *testing.T) {
	st := &mountState{uploadDelay: time.Hour}
	d := newTreeDir(st)
	ctx := context.Background()

	// create, unlink, write, close: the file never belongs on the drive.
	fn, h := addPending(t, d, "x")
	buf := h.tmp.Name()

	if errno := d.Unlink(ctx, "x"); errno != 0 {
		t.Fatalf("Unlink = %v, want 0", errno)
	}
	if errno := h.Release(ctx); errno != 0 {
		t.Fatalf("Release = %v, want 0", errno)
	}

	if fn.parkedHandle() != nil || fn.handle != nil || len(st.delayed) != 0 {
		t.Error("unlinked file still waiting to upload")
	}
	if _, err := os.Stat(buf); !os.IsNotExist(err) {
		t.Errorf("unlinked file's buffer still on disk: %v", err)
	}
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

	h.keepForRetry(h.sharedTmp(), "model.FCStd", errors.New("connection reset"))
	defer h.unpark(false)

	if fn.parkedHandle() != h || h.timer == nil || len(st.delayed) != 1 {
		t.Fatal("failed upload was not kept for a retry")
	}
	if _, err := os.Stat(h.sharedTmp().Name()); err != nil {
		t.Errorf("buffer of a failed upload removed: %v", err)
	}
}

func TestKeepForRetryBacksOffAndStopsOnPermanentErrors(t *testing.T) {
	st := &mountState{}
	fn, h := newDelayedHandle(t, st, "model.FCStd")
	transient := &proton.APIError{Status: http.StatusServiceUnavailable}

	// Each failure doubles the wait, up to the cap.
	for _, want := range []time.Duration{uploadRetryDelay, 2 * uploadRetryDelay, 4 * uploadRetryDelay} {
		h.keepForRetry(h.sharedTmp(), "model.FCStd", transient)
		if h.retryIn != want {
			t.Fatalf("retry in %v, want %v", h.retryIn, want)
		}
		h.unpark(false)
	}
	h.retryIn = uploadRetryMax
	h.keepForRetry(h.sharedTmp(), "model.FCStd", transient)
	h.unpark(false)
	if h.retryIn != uploadRetryMax {
		t.Errorf("retry in %v, want the cap %v", h.retryIn, uploadRetryMax)
	}

	// The name is taken: retrying cannot help, so the buffer stays parked with no timer.
	taken := fmt.Errorf("upload: %w", proton.APIError{Status: http.StatusUnprocessableEntity, Code: proton.AFileOrFolderNameExist})
	h.keepForRetry(h.sharedTmp(), "model.FCStd", taken)
	defer h.unpark(false)

	if fn.parkedHandle() != h || h.timer != nil || len(st.delayed) != 0 {
		t.Fatal("permanently failed upload still scheduled for a retry")
	}
	if _, err := os.Stat(h.sharedTmp().Name()); err != nil {
		t.Errorf("buffer of a permanently failed upload removed: %v", err)
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
