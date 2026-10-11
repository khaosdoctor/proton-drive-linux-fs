package fusefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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

// newWriteState is a mountState whose listing patches (upsertChild, removeChild) work without a
// drive, for tests that run an upload or a move through fakeDrive.
func newWriteState() *mountState {
	return &mountState{
		opTimeout:    time.Minute,
		metaTimeout:  time.Minute,
		dirs:         make(map[string]*dirNode),
		parentOf:     make(map[string]string),
		cacheListing: func(*drive.Node, []*drive.Node) {},
	}
}

// fakeDrive swaps the drive writes for fakes for the length of a test.
func fakeDrive(t *testing.T, upload func(name string) (*drive.Node, error), move func(n *drive.Node, newName string) (*drive.Node, error), trash func(n *drive.Node) error) {
	t.Helper()
	origUpload, origMove, origTrash := driveUpload, driveMove, driveTrash
	t.Cleanup(func() { driveUpload, driveMove, driveTrash = origUpload, origMove, origTrash })

	driveUpload = func(_ context.Context, _ *drive.Client, _ *drive.Node, name string, _ *drive.Node, _ io.Reader, _ int64, _ time.Time) (*drive.Node, error) {
		return upload(name)
	}
	driveMove = func(_ context.Context, _ *drive.Client, n, _, _ *drive.Node, newName string) (*drive.Node, error) {
		return move(n, newName)
	}
	driveTrash = func(_ context.Context, _ *drive.Client, _, n *drive.Node) error {
		return trash(n)
	}
}

func TestRenameDuringUploadMovesOnlyTheUploadedFile(t *testing.T) {
	st := newWriteState()
	d := newTreeDir(st)
	ctx := context.Background()
	p, hp := addPending(t, d, "a")

	// While "a" uploads, the user renames it to "b" and saves a new "a".
	var q *fileNode
	var movedLink string
	fakeDrive(t,
		func(string) (*drive.Node, error) {
			if errno := d.Rename(ctx, "a", d, "b", 0); errno != 0 {
				t.Fatalf("Rename = %v", errno)
			}
			d.MvChild("a", d.EmbeddedInode(), "b", true) // what go-fuse does once Rename returns
			q, _ = addPending(t, d, "a")
			return node("p", "a"), nil
		},
		func(n *drive.Node, newName string) (*drive.Node, error) {
			movedLink = n.Link.LinkID
			return node(n.Link.LinkID, newName), nil
		},
		func(*drive.Node) error { return nil },
	)

	if errno := hp.Release(ctx); errno != 0 {
		t.Fatalf("Release = %v, want 0", errno)
	}

	if movedLink != "p" || p.node == nil || p.node.Link.LinkID != "p" || p.currentName() != "b" {
		t.Errorf("uploaded file not moved to b by its own link (moved %q, node %v, name %q)", movedLink, p.node, p.currentName())
	}
	if !q.pending() || q.currentName() != "a" || q.handle == nil {
		t.Error("the new file saved under the old name was retargeted onto the uploaded file")
	}
}

func TestRenameDuringUploadWithoutReadBackMovesNothing(t *testing.T) {
	st := newWriteState()
	d := newTreeDir(st)
	ctx := context.Background()
	_, hp := addPending(t, d, "a")

	// A refetched listing already has another "a" (saved after the rename and uploaded).
	origFetch := fetchChildren
	t.Cleanup(func() { fetchChildren = origFetch })
	fetchChildren = func(context.Context, *drive.Client, *drive.Node) ([]*drive.Node, error) {
		return []*drive.Node{node("other", "a")}, nil
	}

	var moved []string
	fakeDrive(t,
		func(string) (*drive.Node, error) {
			if errno := d.Rename(ctx, "a", d, "b", 0); errno != 0 {
				t.Fatalf("Rename = %v", errno)
			}
			d.MvChild("a", d.EmbeddedInode(), "b", true)
			return nil, nil // uploaded, but the link could not be read back
		},
		func(n *drive.Node, newName string) (*drive.Node, error) {
			moved = append(moved, n.Link.LinkID)
			return node(n.Link.LinkID, newName), nil
		},
		func(*drive.Node) error { return nil },
	)

	if errno := hp.Release(ctx); errno != 0 {
		t.Fatalf("Release = %v, want 0", errno)
	}
	if len(moved) != 0 {
		t.Errorf("moved %v by name after a failed read-back, want nothing moved", moved)
	}
}

func TestUnmountRetriesReparkedUploadsAndSavesTheRest(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	st := newWriteState()
	st.uploadDelay = time.Hour
	d := newTreeDir(st)
	ctx := context.Background()

	flaky, hf := addPending(t, d, "flaky")
	_, hd := addPending(t, d, "down")
	if _, err := hd.tmp.WriteString("save"); err != nil {
		t.Fatal(err)
	}
	downBuf := hd.tmp.Name()
	_ = hf.Release(ctx)
	_ = hd.Release(ctx)

	// "flaky" fails its first upload, during the unmount flush; "down" never gets through.
	var mu sync.Mutex
	tries := map[string]int{}
	fakeDrive(t,
		func(name string) (*drive.Node, error) {
			mu.Lock()
			defer mu.Unlock()
			tries[name]++
			if name == "flaky" && tries[name] > 1 {
				return node("flaky", "flaky"), nil
			}
			return nil, errors.New("connection reset")
		},
		func(n *drive.Node, newName string) (*drive.Node, error) { return n, nil },
		func(*drive.Node) error { return nil },
	)

	st.flushDelayed()

	if tries["flaky"] != 2 || flaky.pending() {
		t.Errorf("upload that failed during the flush tried %d times, want a second try that works", tries["flaky"])
	}
	if exists(downBuf) {
		t.Error("save that never uploaded left in $TMPDIR at unmount")
	}
	saved, _ := filepath.Glob(filepath.Join(stateHome, "proton-drive-fs", "unsaved", "*-down"))
	if len(saved) != 1 {
		t.Fatalf("unsaved dir has %v, want the one save that never uploaded", saved)
	}
	if got, _ := os.ReadFile(saved[0]); string(got) != "save" {
		t.Errorf("saved buffer holds %q, want %q", got, "save")
	}
}

func TestUnmountDeadlineKeepsUploadsStillRunning(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	st := newWriteState()
	st.uploadDelay = time.Hour
	d := newTreeDir(st)

	origTimeout, origUpload := unmountFlushTimeout, driveUpload
	t.Cleanup(func() { unmountFlushTimeout, driveUpload = origTimeout, origUpload })
	unmountFlushTimeout = 50 * time.Millisecond
	// A big file: its upload outlasts the unmount deadline and stops only when cancelled.
	driveUpload = func(ctx context.Context, _ *drive.Client, _ *drive.Node, _ string, _ *drive.Node, _ io.Reader, _ int64, _ time.Time) (*drive.Node, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	_, h := addPending(t, d, "big")
	buf := h.tmp.Name()
	_ = h.Release(context.Background())

	done := make(chan struct{})
	go func() {
		st.flushDelayed()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("unmount flush did not stop at its deadline")
	}

	saved, _ := filepath.Glob(filepath.Join(stateHome, "proton-drive-fs", "unsaved", "*-big"))
	if exists(buf) || len(saved) != 1 {
		t.Errorf("buffer of an upload cut off by the unmount deadline not moved for recovery (saved: %v)", saved)
	}
}

func TestFailedMoveKeepsTheTrashedDestinationsEdit(t *testing.T) {
	st := newWriteState()
	st.uploadDelay = time.Hour
	d := newTreeDir(st)
	ctx := context.Background()

	// "b" is on the drive with an edit still waiting to upload; "a" is renamed over it.
	listedB := node("b", "b")
	d.children = []*drive.Node{node("a", "a"), listedB}
	b, hb := addPending(t, d, "b")
	b.node = listedB
	buf := hb.tmp.Name()
	_ = hb.Release(ctx)
	defer hb.unpark(false)

	fakeDrive(t,
		func(string) (*drive.Node, error) { return nil, errors.New("unexpected upload") },
		func(*drive.Node, string) (*drive.Node, error) { return nil, errors.New("move refused") },
		func(*drive.Node) error { return nil },
	)

	if errno := d.Rename(ctx, "a", d, "b", 0); errno != syscall.EIO {
		t.Fatalf("Rename = %v, want EIO", errno)
	}
	if !b.pending() || b.parkedHandle() != hb || !exists(buf) {
		t.Error("edit of the trashed destination would upload into the trash instead of as a new file")
	}
}

func TestRenameOverARefusedSaveKeepsIt(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	st := &mountState{uploadDelay: time.Hour}
	d := newTreeDir(st)
	ctx := context.Background()

	// Proton refused "x" for good; the tray says its buffer is kept. Then "y" is renamed over it.
	_, hx := addPending(t, d, "x")
	if _, err := hx.tmp.WriteString("refused"); err != nil {
		t.Fatal(err)
	}
	buf := hx.tmp.Name()
	hx.keepForRetry(hx.sharedTmp(), "x", proton.APIError{Status: http.StatusUnprocessableEntity, Code: proton.AFileOrFolderNameExist})
	_, hy := addPending(t, d, "y")
	_ = hy.Release(ctx)
	defer hy.unpark(false)

	if errno := d.Rename(ctx, "y", d, "x", 0); errno != 0 {
		t.Fatalf("Rename = %v, want 0", errno)
	}

	saved, _ := filepath.Glob(filepath.Join(stateHome, "proton-drive-fs", "unsaved", "*-x"))
	if exists(buf) || len(saved) != 1 {
		t.Fatalf("refused save renamed over was not kept (unsaved dir: %v)", saved)
	}
	if got, _ := os.ReadFile(saved[0]); string(got) != "refused" {
		t.Errorf("kept buffer holds %q, want %q", got, "refused")
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

func TestPermanentUploadErrorOnlyForNamedCodes(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"name taken":       {&proton.APIError{Status: http.StatusUnprocessableEntity, Code: proton.AFileOrFolderNameExist}, true},
		"wrapped value":    {fmt.Errorf("upload: %w", proton.APIError{Status: http.StatusUnprocessableEntity, Code: proton.AFileOrFolderNameExist}), true},
		"paid plan":        {&proton.APIError{Status: http.StatusUnprocessableEntity, Code: proton.PaidPlanRequired}, true},
		"stale draft":      {&proton.APIError{Status: http.StatusConflict, Code: proton.ADraftExist}, false},
		"other conflict":   {&proton.APIError{Status: http.StatusConflict, Code: 2000}, false},
		"other validation": {&proton.APIError{Status: http.StatusUnprocessableEntity, Code: proton.InvalidValue}, false},
		"not found":        {&proton.APIError{Status: http.StatusNotFound}, false},
		"network":          {errors.New("connection reset"), false},
	}
	for name, c := range cases {
		if got := permanentUploadError(c.err); got != c.want {
			t.Errorf("%s: permanentUploadError = %v, want %v", name, got, c.want)
		}
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
	h.settle(context.Background())

	if fn.parkedHandle() != h {
		t.Fatal("excluded name was not parked after the delay ran out")
	}
	if h.timer != nil || len(st.delayed) != 0 {
		t.Error("excluded buffer still has an upload timer")
	}
}
