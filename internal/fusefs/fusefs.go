package fusefs

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/logx"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/state"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/thumbs"
)

// defaultOpTimeout bounds a handler's network calls when Options.OpTimeout is unset.
const defaultOpTimeout = 60 * time.Second

// Options configures the mount.
type Options struct {
	// Version is this binary's version, logged at startup and published in the status
	// snapshot so a stale daemon left running after a rebuild is easy to spot.
	Version string

	Debug        bool
	TTL          time.Duration
	PollInterval time.Duration

	// OpTimeout bounds every network call a handler makes; a handler that misses it returns
	// ETIMEDOUT instead of hanging the caller. <=0 uses the default.
	OpTimeout time.Duration

	// Thumbnails, when set, receives the previews Proton stores for listed files. nil disables
	// preview caching.
	Thumbnails *thumbs.Store

	// DenyReaders are process names refused a read of a file above the client's large-file
	// threshold. Empty allows every reader.
	DenyReaders []string

	// MaxUploads caps how many files upload at once. <=0 uses the default.
	MaxUploads int
}

// defaultMaxUploads matches what Proton's own clients keep in flight.
const defaultMaxUploads = 5

// semWaitLogThreshold is how long a handler has to wait for a semaphore slot (currently just the
// upload one) before that wait is worth a debug log line.
const semWaitLogThreshold = 100 * time.Millisecond

// Mount publishes c's drive tree, rooted at root, at mountpoint and blocks until it is unmounted
// or ctx is cancelled.
func Mount(ctx context.Context, mountpoint string, c *drive.Client, root *drive.Node, opts Options) error {
	if opts.TTL <= 0 {
		opts.TTL = 30 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 10 * time.Second
	}
	if opts.OpTimeout <= 0 {
		opts.OpTimeout = defaultOpTimeout
	}
	if opts.MaxUploads <= 0 {
		opts.MaxUploads = defaultMaxUploads
	}

	// FUSE's kernel-side permission check compares these against the caller; 0:0 made every write ACCESS fail.
	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	st := newMountState(ctx, c, uid, gid, opts)

	// The root directory's path relative to the mountpoint is the empty string.
	rootNode := &dirNode{client: c, node: root, ttl: opts.TTL, st: st}
	st.register(rootNode)

	ttl := opts.TTL
	negTTL := time.Second

	server, err := fs.Mount(mountpoint, rootNode, &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:        "proton-drive-fs",
			Name:          "proton-drive-fs",
			DisableXAttrs: true,
			Debug:         opts.Debug,
		},
		EntryTimeout:    &ttl,
		AttrTimeout:     &ttl,
		NegativeTimeout: &negTTL,
	})
	if err != nil {
		return err
	}

	slog.Info("mounted", "path", mountpoint, "version", opts.Version, "pid", os.Getpid())

	go c.Events(ctx, opts.PollInterval, st.handle, state.Paused)
	go st.publishStatus(ctx, mountpoint, opts.Version)
	go st.watchdog(ctx)

	if st.thumbs != nil {
		go st.runThumbWorker(ctx)
	}

	// A plain Unmount() fails while the mountpoint is busy (e.g. a shell still cd'd into it),
	// and server.Wait() would then block forever waiting for a detach that never happens; fall
	// back to a lazy unmount so Ctrl-C always returns, and let the kernel finish detaching once
	// the last reference drops.
	unmountDone := make(chan struct{})
	go func() {
		defer close(unmountDone)

		<-ctx.Done()

		slog.Info("unmounting", "path", mountpoint)

		if err := server.Unmount(); err != nil {
			slog.Warn("unmount failed, retrying lazily", "path", mountpoint, "err", err)

			bin := fusermountBinary()
			out, lazyErr := exec.Command(bin, "-u", "-z", mountpoint).CombinedOutput()
			if lazyErr != nil {
				slog.Error("lazy unmount failed", "path", mountpoint, "err", lazyErr, "output", string(out))
				slog.Error("could not unmount; run: proton-drive-fs unmount -force", "path", mountpoint)
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		server.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-unmountDone:
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}

	return nil
}

// publishStatus writes a status snapshot for the tray once a second, and only when something
// changed, so a mount nobody watches costs one comparison per second. The snapshot is removed
// on shutdown; a reader that finds a stale one treats the mount as gone.
func (st *mountState) publishStatus(ctx context.Context, mountpoint string, version string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer state.RemoveStatus()

	var last state.Status
	pid := os.Getpid()

	for {
		queued, done, failed := st.uploadCounters(time.Now())
		current := state.Status{
			Mountpoint:    mountpoint,
			Version:       version,
			PID:           pid,
			Transfers:     st.client.Transfers(),
			Paused:        state.Paused(),
			UploadsQueued: queued,
			UploadsDone:   done,
			UploadsFailed: failed,
		}
		if current != last {
			last = current
			current.Updated = time.Now().Unix()
			if err := state.WriteStatus(current); err != nil {
				slog.Warn("writing status failed", "err", err)
			} else {
				slog.Debug("status published", "transfers", current.Transfers, "uploads_queued", current.UploadsQueued, "uploads_done", current.UploadsDone, "uploads_failed", current.UploadsFailed)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// fusermountBinary returns the fusermount helper to use for a lazy unmount: fusermount3 when
// it's on PATH (what libfuse3, and go-fuse, expect), else the older fusermount name.
func fusermountBinary() string {
	if _, err := exec.LookPath("fusermount3"); err == nil {
		return "fusermount3"
	}
	return "fusermount"
}

// mountState tracks every registered directory node and each node's last-known parent, so a
// remote event naming a LinkID can find which cached listings to drop.
type mountState struct {
	client    *drive.Client
	uid       uint32
	gid       uint32
	opTimeout time.Duration

	// ctx is the mount's own lifetime context, cancelled on unmount. A directory listing fetch
	// parents its own deadline on this instead of on whichever request's context happened to
	// trigger it, so one caller cancelling its request never fails every other goroutine waiting
	// on the same in-flight fetch (see dirNode.finishLoad). nil in tests that build a mountState
	// directly; finishLoad falls back to context.Background() then.
	ctx context.Context

	thumbs      *thumbs.Store
	thumbJobs   chan thumbJob
	denyReaders []string

	// uploads bounds how many files upload at once, so a bulk copy does not open one connection
	// per file it was handed.
	uploads sem

	// Upload progress the tray shows as "syncing done/total". drainedAt is when the queue first
	// looked finished, so the counters can go back to zero once nothing is left.
	uploadsQueued atomic.Int64
	uploadsDone   atomic.Int64
	uploadsFailed atomic.Int64
	drainedAt     atomic.Int64

	inflight sync.Map // *int -> inflightOp; the watchdog reports whatever is still here well past its deadline

	mu            sync.Mutex
	dirs          map[string]*dirNode // folder LinkID -> its inode
	parentOf      map[string]string   // child LinkID -> parent LinkID
	thumbInflight map[string]bool     // revision key -> queued, being fetched, or given up on
}

func newMountState(ctx context.Context, c *drive.Client, uid, gid uint32, opts Options) *mountState {
	st := &mountState{
		client:        c,
		ctx:           ctx,
		uid:           uid,
		gid:           gid,
		opTimeout:     opts.OpTimeout,
		thumbs:        opts.Thumbnails,
		denyReaders:   opts.DenyReaders,
		uploads:       newSem(opts.MaxUploads),
		dirs:          make(map[string]*dirNode),
		parentOf:      make(map[string]string),
		thumbInflight: make(map[string]bool),
	}

	if st.thumbs != nil {
		st.thumbJobs = make(chan thumbJob, thumbQueueSize)
	}

	return st
}

// sem is a counting semaphore. A buffered channel is the whole implementation; a nil sem lets
// everything through.
type sem chan struct{}

func newSem(n int) sem {
	if n <= 0 {
		return nil
	}
	return make(sem, n)
}

// acquire takes a slot, or reports false when ctx is done first.
func (s sem) acquire(ctx context.Context) bool {
	if s == nil {
		return true
	}

	select {
	case s <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s sem) release() {
	if s == nil {
		return
	}
	<-s
}

// counterResetIdle is how long the upload queue has to stay drained before the progress counters
// go back to zero, so a finished bulk copy stops reading "10000/10000" forever.
const counterResetIdle = 30 * time.Second

// uploadCounters returns the progress to publish, zeroing the counters once the queue has been
// drained for counterResetIdle.
func (st *mountState) uploadCounters(now time.Time) (queued, done, failed int64) {
	queued = st.uploadsQueued.Load()
	done = st.uploadsDone.Load()
	failed = st.uploadsFailed.Load()

	if queued == 0 || queued > done+failed {
		st.drainedAt.Store(0)
		return queued, done, failed
	}

	since := st.drainedAt.Load()
	if since == 0 {
		st.drainedAt.Store(now.UnixNano())
		return queued, done, failed
	}
	if now.UnixNano()-since < int64(counterResetIdle) {
		return queued, done, failed
	}

	st.uploadsQueued.Add(-queued)
	st.uploadsDone.Add(-done)
	st.uploadsFailed.Add(-failed)
	st.drainedAt.Store(0)

	return 0, 0, 0
}

func (st *mountState) register(d *dirNode) {
	st.mu.Lock()
	st.dirs[d.node.Link.LinkID] = d
	st.mu.Unlock()
}

func (st *mountState) setParent(childID, parentID string) {
	st.mu.Lock()
	st.parentOf[childID] = parentID
	st.mu.Unlock()
}

func (st *mountState) oldParent(childID string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.parentOf[childID]
}

func (st *mountState) forgetParent(childID string) {
	st.mu.Lock()
	delete(st.parentOf, childID)
	st.mu.Unlock()
}

func (st *mountState) invalidateDir(linkID string) {
	st.mu.Lock()
	d, ok := st.dirs[linkID]
	st.mu.Unlock()
	if !ok {
		return
	}
	d.invalidate()
	slog.Info("remote change applied", "path", displayPath(d.path))
}

func (st *mountState) invalidateAll() {
	st.mu.Lock()
	dirs := make([]*dirNode, 0, len(st.dirs))
	for _, d := range st.dirs {
		dirs = append(dirs, d)
	}
	st.mu.Unlock()

	for _, d := range dirs {
		d.invalidate()
	}
	slog.Info("remote change applied", "path", "/", "scope", "full refresh")
}

// displayPath returns p, or "/" for the mount root, whose own path is "".
func displayPath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// invalidateFileContent drops the kernel page cache for a file whose active revision changed,
// so the next read fetches fresh blocks instead of serving stale FOPEN_KEEP_CACHE content.
func (st *mountState) invalidateFileContent(ev drive.Event) {
	st.mu.Lock()
	parent, ok := st.dirs[ev.ParentID]
	st.mu.Unlock()
	if !ok {
		return
	}

	parent.mu.Lock()
	var name string
	found := false
	for _, ch := range parent.children {
		if ch.Link.LinkID == ev.LinkID {
			name = ch.Name
			found = true
			break
		}
	}
	parent.mu.Unlock()
	if !found {
		return
	}

	child := parent.GetChild(name)
	if child == nil {
		return
	}

	// Runs off the event-loop goroutine for the same reason invalidate's NotifyEntry does: a
	// kernel reply blocked behind an in-flight handler for this file must not stall event delivery.
	go func() { _ = child.NotifyContent(0, -1) }()
}

// handle reacts to one remote volume event by invalidating the cached listings and, for file
// content updates, the kernel page cache it affects.
func (st *mountState) handle(ev drive.Event) {
	if ev.Refresh {
		st.invalidateAll()
		return
	}

	old := st.oldParent(ev.LinkID)
	for _, p := range parentsToInvalidate(ev.ParentID, old) {
		st.invalidateDir(p)
	}

	if ev.Type == proton.LinkEventUpdate && !ev.IsDir {
		st.invalidateFileContent(ev)
	}

	if ev.Type == proton.LinkEventDelete {
		st.forgetParent(ev.LinkID)
	}
}

// parentsToInvalidate returns the distinct, non-empty directory LinkIDs whose listing an event
// affects: the link's current parent, plus its previous parent when known and different.
func parentsToInvalidate(newParent, oldParent string) []string {
	var out []string
	if newParent != "" {
		out = append(out, newParent)
	}
	if oldParent != "" && oldParent != newParent {
		out = append(out, oldParent)
	}
	return out
}

// inflightOp describes one handler currently running a network call, for the watchdog to report
// if it runs long.
type inflightOp struct {
	op      string
	path    string
	started time.Time
}

// track registers a handler starting op on path and returns a func to unregister it; call it with
// defer at the top of every handler that makes a network call, e.g. defer st.track("mkdir", name)().
func (st *mountState) track(op, path string) func() {
	key := new(int) // a fresh pointer is a cheap, unique, comparable map key
	st.inflight.Store(key, inflightOp{op: op, path: path, started: time.Now()})
	return func() { st.inflight.Delete(key) }
}

// watchdogInterval is how often watchdog checks for a stuck handler.
const watchdogInterval = 30 * time.Second

// watchdog periodically logs any handler that has been in flight well past its own deadline,
// so a hang like the self-deadlock this mount used to hit (see expire's doc) shows up in the
// journal instead of just freezing the file manager.
func (st *mountState) watchdog(ctx context.Context) {
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st.logStuckOps()
		}
	}
}

func (st *mountState) logStuckOps() {
	stale := 2 * st.opTimeout
	now := time.Now()

	count := 0
	st.inflight.Range(func(_, v any) bool {
		count++
		op := v.(inflightOp)
		if age := now.Sub(op.started); age > stale {
			slog.Warn("operation stuck", "op", op.op, "path", op.path, "in_flight", age.Round(time.Second))
		}
		return true
	})
	slog.Debug("watchdog check", "in_flight", count)
}

// timedOut reports whether ctx's own per-request deadline, not some other failure, is why a
// network call just returned an error.
func timedOut(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.DeadlineExceeded)
}

func inodeNum(linkID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(linkID))
	return h.Sum64()
}

// dirNode is a folder. Its children are fetched lazily and cached for ttl.
type dirNode struct {
	fs.Inode

	client *drive.Client
	ttl    time.Duration
	st     *mountState

	// path is this folder's path relative to the mountpoint, "" for the root. The thumbnail
	// cache is keyed by the absolute path of each file, so children need it to build theirs.
	path string

	// notifying is set while a background NotifyEntry pass for this dir is in flight, so a burst
	// of remote events dedupes to one pass instead of stacking up goroutines.
	notifying atomic.Bool

	mu       sync.Mutex
	node     *drive.Node
	children []*drive.Node
	expires  time.Time

	// gen counts every upsertChild/removeChild patch to children, so a fetch that was already in
	// flight when a patch landed can tell its result is stale (see publish).
	gen uint64

	// loading is non-nil while one goroutine fetches this listing, and closed when it is done;
	// everybody else waits on it instead of issuing the same ListChildren again.
	loading chan struct{}

	// loadErrno is the errno the most recently finished fetch failed with, valid to read once the
	// `loading` channel that fetch published to has closed. 0 after a successful fetch.
	loadErrno syscall.Errno
}

// setNode replaces the node backing this directory, e.g. after a remote listing refresh.
func (d *dirNode) setNode(n *drive.Node) {
	d.mu.Lock()
	d.node = n
	d.mu.Unlock()
}

// expire drops the cached listing so the next Lookup/Readdir refetches it, without touching the
// kernel. A request handler for this directory must call this, never invalidate: the kernel holds
// the directory locked for the duration of a Mkdir/Create/Unlink/Rmdir/Rename/Release and waits
// for our reply before releasing that lock, while NotifyEntry blocks writing to /dev/fuse until
// the same lock is free — calling it from inside a handler for its own directory deadlocks the
// handler against itself (goroutine dump: Mkdir -> re-list -> invalidate -> NotifyEntry, stuck
// behind the event loop's own invalidate of the same dir). The handler's own reply already tells
// the kernel about the entry it just created/removed/renamed, so no notification is needed here.
func (d *dirNode) expire() {
	d.mu.Lock()
	d.expires = time.Time{}
	d.mu.Unlock()
}

// invalidate expires the cached listing and notifies the kernel, in the background, to drop the
// dentry for every previously listed child, forcing a re-Lookup (and re-Getattr) on next access.
// Call this only from the event loop (mountState.handle and its helpers), never from a request
// handler; see expire's doc. The notification runs off this goroutine so a kernel reply that is
// slow, or blocked behind an in-flight handler for this same dir, never stalls the event loop.
func (d *dirNode) invalidate() {
	d.expire()

	if !d.notifying.CompareAndSwap(false, true) {
		return // a notify pass for this dir is already in flight
	}

	d.mu.Lock()
	names := make([]string, 0, len(d.children))
	for _, ch := range d.children {
		names = append(names, ch.Name)
	}
	d.mu.Unlock()

	go d.notifyEntries(names)
}

// notifyEntries pushes NotifyEntry for each name to the kernel. Runs only on the goroutine
// invalidate spawns for it.
func (d *dirNode) notifyEntries(names []string) {
	defer d.notifying.Store(false)

	for _, name := range names {
		_ = d.NotifyEntry(name)
	}
}

var _ = (fs.NodeGetattrer)((*dirNode)(nil))
var _ = (fs.NodeStatfser)((*dirNode)(nil))
var _ = (fs.NodeLookuper)((*dirNode)(nil))
var _ = (fs.NodeReaddirer)((*dirNode)(nil))
var _ = (fs.NodeMkdirer)((*dirNode)(nil))
var _ = (fs.NodeUnlinker)((*dirNode)(nil))
var _ = (fs.NodeRmdirer)((*dirNode)(nil))
var _ = (fs.NodeRenamer)((*dirNode)(nil))
var _ = (fs.NodeCreater)((*dirNode)(nil))

func (d *dirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = d.Mode() | 0o755
	out.Ino = d.StableAttr().Ino
	out.Uid = d.st.uid
	out.Gid = d.st.gid
	return 0
}

func (d *dirNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	// ponytail: Proton exposes no quota through this API path; report a large fake filesystem so tools do not treat it as full
	out.Bsize = 4096
	out.Frsize = 4096
	out.NameLen = 255
	out.Blocks = 1 << 40
	out.Bfree = 1 << 39
	out.Bavail = 1 << 39
	out.Files = 1 << 30
	out.Ffree = 1 << 29
	return 0
}

// load returns the cached children, refetching them once ttl has elapsed. The fetch itself runs
// detached from any single caller (see finishLoad): every caller here, whether it triggers the
// fetch or finds one already running, only ever waits for it to finish and respects nothing but
// its own ctx while doing so, so one caller's cancellation or timeout never fails another's.
func (d *dirNode) load(ctx context.Context) ([]*drive.Node, syscall.Errno) {
	for {
		children, done, wait := d.beginLoad()
		if children != nil {
			return children, 0
		}

		if done != nil {
			go d.finishLoad(done)
			wait = done
		} else if logx.DebugEnabled(ctx) {
			slog.Debug("singleflight wait", "op", "readdir", "path", displayPath(d.path))
		}

		select {
		case <-wait:
			return d.loadResult()
		case <-ctx.Done():
			return nil, syscall.EINTR
		}
	}
}

// loadResult reads the outcome of the fetch that just finished: the freshly published listing, or
// the errno finishLoad recorded when the fetch failed.
func (d *dirNode) loadResult() ([]*drive.Node, syscall.Errno) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.children != nil && time.Now().Before(d.expires) {
		return d.children, 0
	}
	return nil, d.loadErrno
}

// beginLoad says how this caller gets the listing: children when the cache is still fresh, done
// when this caller owns the fetch, or wait when another goroutine is already fetching.
func (d *dirNode) beginLoad() (children []*drive.Node, done, wait chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.children != nil && time.Now().Before(d.expires) {
		return d.children, nil, nil
	}
	if d.loading != nil {
		return nil, nil, d.loading
	}

	d.loading = make(chan struct{})
	return nil, d.loading, nil
}

// shouldPublish reports whether a freshly fetched listing should replace the cache. hadChildren
// is whether the directory already had a cached listing for a handler to patch; startGen and
// curGen are dirNode.gen at the moment the fetch started and right now. A handler's local patch
// (upsertChild/removeChild) bumps gen, so startGen != curGen means the fetch's result predates a
// patch that already landed and must not overwrite it.
func shouldPublish(hadChildren bool, startGen, curGen uint64) bool {
	return !hadChildren || startGen == curGen
}

// publish swaps a freshly fetched listing in and wakes everyone waiting on the fetch. children
// nil means the fetch failed and the cache stays as it was, and loadResult reports the errno
// finishLoad stored instead. When a handler patched the cache while this fetch was in flight, the
// patch wins over the listing that predates it: the fetched slice is dropped, but the cache still
// counts as fresh (expires is refreshed) so the next TTL refetch or event-driven expire is what
// brings the remote view back in sync, not an immediate re-fetch.
func (d *dirNode) publish(children []*drive.Node, done chan struct{}, startGen uint64) {
	d.mu.Lock()
	d.loading = nil

	if children != nil {
		if shouldPublish(d.children != nil, startGen, d.gen) {
			d.children = children
		}
		d.expires = time.Now().Add(d.ttl)
	}

	d.mu.Unlock()

	close(done)
}

// finishLoad fetches the listing whoever called beginLoad and got done took ownership of. It runs
// on its own goroutine, detached from any single caller's context: the fetch is parented on the
// mount's own lifetime context (or context.Background() in a test that never set one), not on the
// context of whichever request happened to trigger it, so that request cancelling or timing out
// never fails every other goroutine waiting on this same fetch (see load). Each of those waiters
// reads the result for itself via loadResult once `done` closes.
func (d *dirNode) finishLoad(done chan struct{}) {
	d.mu.Lock()
	node := d.node
	startGen := d.gen
	d.mu.Unlock()

	defer d.st.track("readdir", node.Name)()

	parent := context.Background()
	if d.st.ctx != nil {
		parent = d.st.ctx
	}
	opCtx, cancel := context.WithTimeout(parent, d.st.opTimeout)
	defer cancel()

	children, err := fetchChildren(opCtx, d.client, node)
	if err != nil {
		errno := syscall.EIO
		if timedOut(opCtx) {
			slog.Error("readdir timed out", "path", displayPath(d.path), "timeout", d.st.opTimeout)
			errno = syscall.ETIMEDOUT
		} else {
			slog.Error("readdir failed", "path", displayPath(d.path), "err", err)
		}

		d.mu.Lock()
		d.loadErrno = errno
		d.mu.Unlock()

		d.publish(nil, done, startGen)
		return
	}

	d.mu.Lock()
	d.loadErrno = 0
	d.mu.Unlock()

	d.publish(children, done, startGen)

	parentID := node.Link.LinkID
	for _, ch := range children {
		d.st.setParent(ch.Link.LinkID, parentID)
	}

	if d.st.thumbs != nil {
		go d.st.queueThumbs(d.path, children)
	}
}

// fetchChildren lists node's children over the network; overridden in tests to fake a slow or
// failing fetch without a real drive.Client.
var fetchChildren = func(ctx context.Context, c *drive.Client, node *drive.Node) ([]*drive.Node, error) {
	return c.Children(ctx, node)
}

// upsertChild patches one entry into the cached listing, replacing an entry with the same link or
// the same name. Handlers do this instead of re-listing the parent after a write: a re-list per
// written file makes a bulk copy quadratic (10k files into one folder is ~340k listing requests
// at 150 links per page).
func (d *dirNode) upsertChild(n *drive.Node) {
	d.mu.Lock()
	d.children = upsertNode(d.children, n)
	d.gen++
	parentID := d.node.Link.LinkID
	d.mu.Unlock()

	d.st.setParent(n.Link.LinkID, parentID)
}

// removeChild drops the entry named name from the cached listing.
func (d *dirNode) removeChild(name string) {
	d.mu.Lock()
	removed, children := removeNode(d.children, name)
	d.children = children
	d.gen++
	d.mu.Unlock()

	if removed != nil {
		d.st.forgetParent(removed.Link.LinkID)
	}
}

// upsertNode returns the listing with the entry n stands for replaced, matched by link id or by
// name, and appended when the listing has neither. It copies rather than writing in place: load
// hands the slice to Readdir and Lookup with the lock released, so a cached listing has to stay
// immutable once it is published.
func upsertNode(children []*drive.Node, n *drive.Node) []*drive.Node {
	out := make([]*drive.Node, len(children), len(children)+1)
	copy(out, children)

	for i, ch := range out {
		if ch.Link.LinkID != n.Link.LinkID && ch.Name != n.Name {
			continue
		}

		out[i] = n
		return out
	}

	return append(out, n)
}

// removeNode drops the entry named name, returning it and a copy of the listing without it.
func removeNode(children []*drive.Node, name string) (*drive.Node, []*drive.Node) {
	for i, ch := range children {
		if ch.Name != name {
			continue
		}

		out := make([]*drive.Node, 0, len(children)-1)
		out = append(out, children[:i]...)
		out = append(out, children[i+1:]...)
		return ch, out
	}

	return nil, children
}

func (d *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	children, errno := d.load(ctx)
	if errno != 0 {
		return nil, errno
	}

	for _, child := range children {
		if child.Name != name {
			continue
		}

		// A listing only knows the encrypted size; resolve the real one for this single child,
		// because what a Lookup reports is what applications go on.
		child.ResolveAttrs()

		if existing := d.GetChild(name); existing != nil {
			refreshNode(existing, child)
			d.fillEntryOut(out, child)
			return existing, 0
		}

		return d.makeChild(ctx, child, out), 0
	}

	return nil, syscall.ENOENT
}

// findChild looks up name in the cached listing, refetching once if it's not there in case the
// cache is merely stale.
func (d *dirNode) findChild(ctx context.Context, name string) (*drive.Node, syscall.Errno) {
	children, errno := d.load(ctx)
	if errno != 0 {
		return nil, errno
	}

	for _, child := range children {
		if child.Name == name {
			return child, 0
		}
	}

	return nil, syscall.ENOENT
}

// Mkdir creates a folder on the drive and mounts its inode from what the API returned, without
// re-listing the parent.
func (d *dirNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	target := path.Join(d.path, name)
	slog.Info("creating folder", "path", target)

	defer d.st.track("mkdir", target)()
	opCtx, cancel := context.WithTimeout(ctx, d.st.opTimeout)
	defer cancel()

	created, err := d.client.CreateDir(opCtx, d.node, name)
	if err != nil {
		if timedOut(opCtx) {
			slog.Error("creating folder timed out", "path", target, "timeout", d.st.opTimeout)
			return nil, syscall.ETIMEDOUT
		}
		slog.Error("creating folder failed", "path", target, "err", err)
		return nil, syscall.EIO
	}

	d.upsertChild(created)

	return d.makeChild(ctx, created, out), 0
}

// Create makes a temp file to buffer the new file's content and mounts a pending fileNode
// (node == nil) for it; the file isn't created on the drive until Release uploads it.
// ponytail: whole-file temp buffer; block-level partial writes later if needed
func (d *dirNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	tmp, err := os.CreateTemp("", "proton-drive-fs-*")
	if err != nil {
		slog.Error("creating temp file failed", "path", path.Join(d.path, name), "err", err)
		return nil, nil, 0, syscall.EIO
	}

	fn := &fileNode{client: d.client, parent: d, name: name}
	handle := &fileHandle{node: fn, tmp: tmp, dirty: true, owns: true}
	fn.handle = handle

	// ponytail: inode number derived from parent+name until the real LinkID exists after upload
	attr := fs.StableAttr{Mode: fuse.S_IFREG, Ino: inodeNum(d.node.Link.LinkID + "/" + name)}
	inode := d.NewInode(ctx, fn, attr)

	out.Ino = attr.Ino
	out.Mode = fuse.S_IFREG | 0o644
	out.Uid = d.st.uid
	out.Gid = d.st.gid

	return inode, handle, 0, 0
}

// Unlink trashes a file.
func (d *dirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return d.remove(ctx, name)
}

// Rmdir trashes a folder. Proton trashes non-empty folders recursively, so this doesn't need to
// check for children first.
func (d *dirNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return d.remove(ctx, name)
}

func (d *dirNode) remove(ctx context.Context, name string) syscall.Errno {
	target, errno := d.findChild(ctx, name)
	if errno != 0 {
		return errno
	}

	targetPath := path.Join(d.path, name)
	slog.Info("deleting", "path", targetPath)

	defer d.st.track("remove", targetPath)()
	opCtx, cancel := context.WithTimeout(ctx, d.st.opTimeout)
	defer cancel()

	if err := d.client.Trash(opCtx, d.node, target); err != nil {
		if timedOut(opCtx) {
			slog.Error("deleting timed out", "path", targetPath, "timeout", d.st.opTimeout)
			return syscall.ETIMEDOUT
		}
		slog.Error("deleting failed", "path", targetPath, "err", err)
		return syscall.EIO
	}

	d.removeChild(name)
	return 0
}

// renameNoReplace is Linux's RENAME_NOREPLACE renameat2() flag; go-fuse only exports
// RENAME_EXCHANGE.
const renameNoReplace = 0x1

func (d *dirNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if flags&(fs.RENAME_EXCHANGE|renameNoReplace) != 0 {
		return syscall.EINVAL
	}

	newDir, ok := newParent.(*dirNode)
	if !ok {
		return syscall.EINVAL
	}

	target, errno := d.findChild(ctx, name)
	if errno != 0 {
		return errno
	}

	from := path.Join(d.path, name)
	to := path.Join(newDir.path, newName)

	action := "moving"
	if d == newDir {
		action = "renaming"
	}
	slog.Info(action, "from", from, "to", to)

	defer d.st.track("rename", from+" -> "+to)()
	opCtx, cancel := context.WithTimeout(ctx, d.st.opTimeout)
	defer cancel()

	moved, err := d.client.Move(opCtx, target, d.node, newDir.node, newName)
	if err != nil {
		if timedOut(opCtx) {
			slog.Error(action+" timed out", "from", from, "to", to, "timeout", d.st.opTimeout)
			return syscall.ETIMEDOUT
		}
		slog.Error(action+" failed", "from", from, "to", to, "err", err)
		return syscall.EIO
	}

	if child := d.GetChild(name); child != nil {
		refreshNode(child, moved)
		if fn, ok := child.Operations().(*fileNode); ok {
			fn.mu.Lock()
			fn.name = newName
			fn.parent = newDir
			fn.mu.Unlock()
		}
	}

	d.removeChild(name)
	newDir.upsertChild(moved)

	return 0
}

func (d *dirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	children, errno := d.load(ctx)
	if errno != 0 {
		return nil, errno
	}

	entries := make([]fuse.DirEntry, 0, len(children))
	for _, child := range children {
		mode := uint32(fuse.S_IFREG)
		if child.IsDir() {
			mode = fuse.S_IFDIR
		}

		entries = append(entries, fuse.DirEntry{
			Name: child.Name,
			Ino:  inodeNum(child.Link.LinkID),
			Mode: mode,
		})
	}

	return fs.NewListDirStream(entries), 0
}

func (d *dirNode) makeChild(ctx context.Context, child *drive.Node, out *fuse.EntryOut) *fs.Inode {
	d.fillEntryOut(out, child)
	attr := fs.StableAttr{Ino: out.Ino}

	if child.IsDir() {
		attr.Mode = fuse.S_IFDIR
		dn := &dirNode{client: d.client, node: child, ttl: d.ttl, st: d.st, path: path.Join(d.path, child.Name)}
		inode := d.NewInode(ctx, dn, attr)
		d.st.register(dn)
		return inode
	}

	attr.Mode = fuse.S_IFREG
	return d.NewInode(ctx, &fileNode{client: d.client, parent: d, name: child.Name, node: child}, attr)
}

// fillEntryOut populates a Lookup/makeChild EntryOut from child's current attributes.
func (d *dirNode) fillEntryOut(out *fuse.EntryOut, child *drive.Node) {
	out.Ino = inodeNum(child.Link.LinkID)
	out.Uid = d.st.uid
	out.Gid = d.st.gid

	if child.IsDir() {
		out.Mode = fuse.S_IFDIR | 0o755
		return
	}

	out.Mode = fuse.S_IFREG | 0o644
	out.Size = uint64(child.Size())
	out.Mtime = uint64(child.ModTime().Unix())
}

// refreshNode updates an already-mounted inode's embedded drive.Node so its Getattr/Open see
// the latest listing (size, mtime, revision) without recreating the inode.
func refreshNode(inode *fs.Inode, node *drive.Node) {
	switch op := inode.Operations().(type) {
	case *fileNode:
		op.setNode(node)
	case *dirNode:
		op.setNode(node)
	}
}

// fileNode is a regular file. Content is fetched lazily through drive.File on Open/Read; writes
// buffer to a whole-file temp file and upload on Release.
type fileNode struct {
	fs.Inode

	client *drive.Client

	mu     sync.Mutex
	node   *drive.Node // nil until the file's first revision is uploaded
	parent *dirNode    // current parent, kept in sync by Rename
	name   string      // current name, kept in sync by Rename
	handle *fileHandle // most recently opened write handle with a temp buffer, if any
}

var _ = (fs.NodeGetattrer)((*fileNode)(nil))
var _ = (fs.NodeSetattrer)((*fileNode)(nil))
var _ = (fs.NodeOpener)((*fileNode)(nil))

// setNode replaces the node backing this file, e.g. after a remote listing refresh.
func (f *fileNode) setNode(n *drive.Node) {
	f.mu.Lock()
	f.node = n
	f.name = n.Name
	f.mu.Unlock()
}

// currentName returns the file's current name, used for logging and as the Upload target name.
func (f *fileNode) currentName() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.name
}

// mountState returns the mountState of this file's current parent, for handlers that need
// opTimeout or the in-flight registry.
func (f *fileNode) mountState() *mountState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.parent.st
}

func (f *fileNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	f.mu.Lock()
	node := f.node
	handle := f.handle
	parent := f.parent
	f.mu.Unlock()

	out.Mode = f.Mode() | 0o644
	out.Ino = f.StableAttr().Ino
	if parent != nil {
		out.Uid = parent.st.uid
		out.Gid = parent.st.gid
	}

	// A dirty write handle's temp buffer is the current size, even for an existing file whose
	// node still points at the last-uploaded revision.
	if handle != nil {
		if size, ok := handle.tmpSize(); ok {
			out.Size = uint64(size)
			if node != nil {
				out.Mtime = uint64(node.ModTime().Unix())
			}
			return 0
		}
	}

	if node == nil {
		return 0
	}

	// A stat is what applications size their reads by, so it must report the plaintext size even
	// when this node came straight out of a listing.
	node.ResolveAttrs()

	out.Size = uint64(node.Size())
	out.Mtime = uint64(node.ModTime().Unix())
	return 0
}

// Setattr only handles size (truncate); every other attribute is accepted as a no-op.
func (f *fileNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	size, ok := in.GetSize()
	if !ok {
		return f.Getattr(ctx, fh, out)
	}

	// Prefer the handle the kernel passed in fh; fall back to this node's write handle for a
	// bare truncate(2) or an fh that isn't the write handle (e.g. a read-only fh).
	handle, ok := fh.(*fileHandle)
	if !ok || !handle.hasTmp() {
		f.mu.Lock()
		handle = f.handle
		f.mu.Unlock()
	}

	// ponytail: truncate needs a write handle already open on the file; a bare truncate(2) with
	// no prior open isn't supported.
	if handle == nil {
		slog.Warn("truncate without an open write handle", "path", f.currentName())
		return syscall.EIO
	}

	if err := handle.truncate(int64(size)); err != nil {
		slog.Error("truncating failed", "path", f.currentName(), "err", err)
		return syscall.EIO
	}

	return f.Getattr(ctx, fh, out)
}

func (f *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	f.mu.Lock()
	node := f.node
	parent := f.parent
	f.mu.Unlock()

	if node == nil {
		// A pending (not yet uploaded) file only has the handle Create already returned.
		return nil, 0, syscall.EIO
	}

	slog.Info("opening file", "path", f.currentName())

	// Every open mode below can read, so the denylist applies to all of them.
	if parent != nil {
		if procName, pid, denied := parent.st.deniedReader(ctx, node.Size()); denied {
			slog.Warn("denied reader", "path", f.currentName(), "size", node.Size(), "process", procName, "pid", pid)
			return nil, 0, syscall.EACCES
		}
	}

	if flags&syscall.O_ACCMODE == syscall.O_RDONLY {
		// A dirty write handle's buffered bytes are the current content; read from the same
		// temp file (not closed or removed here) instead of serving the stale committed
		// revision. Direct I/O keeps the kernel from caching pre-write bytes alongside them.
		f.mu.Lock()
		writer := f.handle
		f.mu.Unlock()

		if writer != nil {
			if tmp := writer.sharedTmp(); tmp != nil {
				return &fileHandle{node: f, tmp: tmp}, fuse.FOPEN_DIRECT_IO, 0
			}
		}

		defer parent.st.track("open", f.currentName())()
		opCtx, cancel := context.WithTimeout(ctx, parent.st.opTimeout)
		defer cancel()

		file, err := f.client.OpenFile(opCtx, node)
		if err != nil {
			if timedOut(opCtx) {
				slog.Error("opening file timed out", "path", f.currentName(), "timeout", parent.st.opTimeout)
				return nil, 0, syscall.ETIMEDOUT
			}
			slog.Error("opening file failed", "path", f.currentName(), "err", err)
			return nil, 0, syscall.EIO
		}
		return &fileHandle{node: f, file: file}, fuse.FOPEN_KEEP_CACHE, 0
	}

	// ponytail: single writer per file; per-handle set if a real workload needs concurrent writers
	f.mu.Lock()
	if f.handle != nil {
		f.mu.Unlock()
		return nil, 0, syscall.EBUSY
	}
	f.mu.Unlock()

	tmp, err := os.CreateTemp("", "proton-drive-fs-*")
	if err != nil {
		slog.Error("creating temp file failed", "path", f.currentName(), "err", err)
		return nil, 0, syscall.EIO
	}

	if flags&syscall.O_TRUNC == 0 {
		defer parent.st.track("open", f.currentName())()
		opCtx, cancel := context.WithTimeout(ctx, parent.st.opTimeout)
		defer cancel()

		if err := downloadInto(opCtx, f.client, node, tmp); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			if timedOut(opCtx) {
				slog.Error("opening file timed out", "path", f.currentName(), "timeout", parent.st.opTimeout)
				return nil, 0, syscall.ETIMEDOUT
			}
			slog.Error("downloading file failed", "path", f.currentName(), "err", err)
			return nil, 0, syscall.EIO
		}
	}

	handle := &fileHandle{node: f, tmp: tmp, owns: true}

	f.mu.Lock()
	if f.handle != nil {
		f.mu.Unlock()
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, 0, syscall.EBUSY
	}
	f.handle = handle
	f.mu.Unlock()

	return handle, 0, 0
}

// uploadTimeout returns the deadline for uploading a file of size bytes: base, or one second per
// 256 KiB of it if that would be longer, so a large upload isn't cut off by the timeout meant to
// catch a hung small request.
func uploadTimeout(size int64, base time.Duration) time.Duration {
	if size <= 0 {
		return base
	}

	perByte := time.Duration(size/(256*1024)) * time.Second
	if perByte > base {
		return perByte
	}
	return base
}

// downloadInto streams node's full active-revision content into tmp, reusing the same
// block-caching ReadAt path as read-only opens.
func downloadInto(ctx context.Context, client *drive.Client, node *drive.Node, tmp *os.File) error {
	file, err := client.OpenFile(ctx, node)
	if err != nil {
		return err
	}

	buf := make([]byte, 256*1024)
	var off int64
	for {
		n, err := file.ReadAt(ctx, buf, off)
		if n > 0 {
			if _, werr := tmp.WriteAt(buf[:n], off); werr != nil {
				return werr
			}
			off += int64(n)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// fileHandle is an open handle to a file. A read-only open against an existing revision wraps
// drive.File (streamed, lazily downloaded, cached). A write-capable open buffers the whole file
// in a local temp file, uploaded in full on Release.
// ponytail: whole-file temp buffer; block-level partial writes later if needed
type fileHandle struct {
	node *fileNode
	file *drive.File // set for a read-only handle

	mu    sync.Mutex
	tmp   *os.File // set for a write-capable handle, or a read-only view of the write handle's tmp
	dirty bool
	owns  bool // true only for the write handle that owns tmp: it alone closes, removes and uploads it
}

var _ = (fs.FileReader)((*fileHandle)(nil))
var _ = (fs.FileWriter)((*fileHandle)(nil))
var _ = (fs.FileFlusher)((*fileHandle)(nil))
var _ = (fs.FileReleaser)((*fileHandle)(nil))

func (h *fileHandle) hasTmp() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tmp != nil
}

// sharedTmp returns the write handle's temp file for a borrowing read-only handle to read from,
// or nil if this handle has no temp buffer.
func (h *fileHandle) sharedTmp() *os.File {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tmp
}

func (h *fileHandle) tmpSize() (int64, bool) {
	h.mu.Lock()
	tmp := h.tmp
	h.mu.Unlock()

	if tmp == nil {
		return 0, false
	}

	info, err := tmp.Stat()
	if err != nil {
		return 0, false
	}
	return info.Size(), true
}

func (h *fileHandle) truncate(size int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.tmp == nil {
		return errors.New("no temp buffer")
	}
	if err := h.tmp.Truncate(size); err != nil {
		return err
	}
	h.dirty = true
	return nil
}

func (h *fileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.mu.Lock()
	tmp := h.tmp
	h.mu.Unlock()

	if tmp != nil {
		n, err := tmp.ReadAt(dest, off)
		if err != nil && err != io.EOF {
			slog.Error("read failed", "path", h.node.currentName(), "offset", off, "len", len(dest), "err", err)
			return nil, syscall.EIO
		}
		return fuse.ReadResultData(dest[:n]), 0
	}

	name := h.node.currentName()
	st := h.node.mountState()

	defer st.track("read", name)()
	opCtx, cancel := context.WithTimeout(ctx, st.opTimeout)
	defer cancel()

	n, err := h.file.ReadAt(opCtx, dest, off)
	if err != nil && err != io.EOF {
		if timedOut(opCtx) {
			slog.Error("read timed out", "path", name, "timeout", st.opTimeout)
			return nil, syscall.ETIMEDOUT
		}
		slog.Error("read failed", "path", name, "offset", off, "len", len(dest), "err", err)
		return nil, syscall.EIO
	}

	return fuse.ReadResultData(dest[:n]), 0
}

func (h *fileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.mu.Lock()
	tmp := h.tmp
	h.mu.Unlock()

	if tmp == nil {
		return 0, syscall.EBADF
	}

	n, err := tmp.WriteAt(data, off)
	if err != nil {
		slog.Error("writing temp buffer failed", "path", h.node.currentName(), "err", err)
		return 0, syscall.EIO
	}

	h.mu.Lock()
	h.dirty = true
	h.mu.Unlock()

	return uint32(n), 0
}

// Flush is a no-op: content is only uploaded once on Release.
func (h *fileHandle) Flush(ctx context.Context) syscall.Errno {
	return 0
}

// Release uploads a dirty temp buffer as a new file or a new revision, then patches the uploaded
// link into the parent listing and into this node so subsequent Getattr/Open see it. The temp
// file is always removed.
func (h *fileHandle) Release(ctx context.Context) syscall.Errno {
	h.mu.Lock()
	tmp := h.tmp
	dirty := h.dirty
	owns := h.owns
	h.tmp = nil
	h.mu.Unlock()

	// A borrowing read-only handle (owns == false) neither owns nor uploads the write handle's
	// temp file; only that write handle's own Release closes and removes it.
	if tmp == nil || !owns {
		return 0
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	fn := h.node
	fn.mu.Lock()
	if fn.handle == h {
		fn.handle = nil
	}
	fn.mu.Unlock()

	if !dirty {
		return 0
	}

	fn.mu.Lock()
	existing := fn.node
	name := fn.name
	parent := fn.parent
	fn.mu.Unlock()

	info, err := tmp.Stat()
	if err != nil {
		slog.Error("stat temp file failed", "path", name, "err", err)
		return syscall.EIO
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		slog.Error("seek temp file failed", "path", name, "err", err)
		return syscall.EIO
	}

	st := parent.st
	st.uploadsQueued.Add(1)

	semWait := time.Now()
	acquired := st.uploads.acquire(ctx)
	if waited := time.Since(semWait); waited > semWaitLogThreshold {
		slog.Debug("semaphore wait", "op", "upload", "path", name, "waited", waited)
	}
	if !acquired {
		st.uploadsFailed.Add(1)
		slog.Warn("upload cancelled while queued", "path", name)
		return syscall.EINTR
	}
	defer st.uploads.release()

	slog.Info("uploading file", "path", name, "size", logx.FormatSize(info.Size()))
	uploadStart := time.Now()

	timeout := uploadTimeout(info.Size(), st.opTimeout)
	defer st.track("upload", name)()
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	uploaded, err := fn.client.Upload(opCtx, parent.node, name, existing, tmp, info.Size(), time.Now())
	if err != nil {
		st.uploadsFailed.Add(1)
		if timedOut(opCtx) {
			slog.Error("uploading file timed out", "path", name, "timeout", timeout)
			return syscall.ETIMEDOUT
		}
		slog.Error("uploading file failed", "path", name, "err", err)
		return syscall.EIO
	}

	st.uploadsDone.Add(1)
	slog.Info("uploaded file", "path", name, "size", logx.FormatSize(info.Size()), logx.Elapsed(uploadStart))

	// Re-read parent/name: a concurrent Rename during the upload may have moved this file, and
	// the entry has to be patched in where it is now, not where it was.
	fn.mu.Lock()
	uploadedName := fn.name
	uploadedParent := fn.parent
	fn.mu.Unlock()

	// A nil node means the upload succeeded but reading the link back did not; expire the listing
	// so the next Lookup fetches the truth. Same when a Rename moved the file mid-upload.
	if uploaded == nil {
		uploadedParent.expire()
		return 0
	}

	if uploadedName != name {
		uploadedParent.expire()
		return 0
	}

	fn.setNode(uploaded)
	uploadedParent.upsertChild(uploaded)

	return 0
}
