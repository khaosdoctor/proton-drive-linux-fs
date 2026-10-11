package fusefs

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/api"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/logx"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/state"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/thumbs"
)

// defaultOpTimeout bounds a handler's network calls when Options.OpTimeout is unset.
const defaultOpTimeout = 60 * time.Second

// defaultMetaTimeout bounds a metadata handler's network calls when Options.MetaTimeout is
// unset. Kept far shorter than OpTimeout: a metadata call (a listing, a lookup, a mkdir) blocks
// whichever caller made it in the kernel's uninterruptible D state, immune to signals, so a file
// indexer walking $HOME can wedge dozens of its own threads on a single slow directory. 10s is
// long enough to ride out an ordinary network hiccup without leaving a caller parked anywhere
// near as long as OpTimeout's 60s.
const defaultMetaTimeout = 10 * time.Second

// failedLoadCooldown is a short expiry applied after a failed refresh: without it, every readdir
// on an unhealthy directory starts a new fetch.
const failedLoadCooldown = 5 * time.Second

// Options configures the mount.
type Options struct {
	// Version is this binary's version, logged at startup and published in the status
	// snapshot so a stale daemon left running after a rebuild is easy to spot.
	Version string

	Debug        bool
	TTL          time.Duration
	PollInterval time.Duration

	// OpTimeout bounds every data-operation network call a handler makes (Open's revision/block-
	// list fetch, Read, Release's upload); a handler that misses it returns ETIMEDOUT instead of
	// hanging the caller. <=0 uses the default.
	OpTimeout time.Duration

	// MetaTimeout bounds every metadata-operation network call (a directory listing, Lookup's
	// attribute resolution, Mkdir, Unlink/Rmdir, Rename): much shorter than OpTimeout, since a
	// metadata call blocks its caller in uninterruptible sleep and a stale cached listing is
	// usually a fine answer while a fresh one is fetched in the background (see dirNode.load).
	// <=0 uses the default.
	MetaTimeout time.Duration

	// Thumbnails, when set, receives the previews Proton stores for listed files. nil disables
	// preview caching.
	Thumbnails *thumbs.Store

	// DenyReaders are process names refused a read of a file above the client's large-file
	// threshold. Empty allows every reader.
	DenyReaders []string

	// Exclude are filename patterns hidden from listings and never uploaded. Each entry
	// is a filepath.Match glob, or a regexp if prefixed with "re:".
	Exclude []string

	// UploadDelay is how long a closed, written file waits before it uploads, so an app's save
	// sequence (write "x.<uuid>", rename "x" to a backup, rename "x.<uuid>" to "x") finishes
	// first. <=0 uploads on close.
	UploadDelay time.Duration

	// UploadDelays override UploadDelay per filename, as "pattern=duration" with pattern in
	// Exclude's syntax. The first match wins.
	UploadDelays []string

	// Registry, when set, maps file extensions to their freedesktop thumbnailer commands.
	// The daemon runs these thumbnailers on downloaded files in the background, and blocks
	// their processes from reading through FUSE. nil disables external thumbnailing.
	Registry *thumbs.Registry

	// MaxUploads caps how many files upload at once. <=0 uses the default.
	MaxUploads int

	// OnAuthFailed is called when the event poller detects an unrecoverable auth error
	// (refresh token revoked by a new login). The callback should cancel the mount context
	// so the daemon exits and restarts with fresh tokens from session.json.
	OnAuthFailed func()
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
	if opts.MetaTimeout <= 0 {
		opts.MetaTimeout = defaultMetaTimeout
	}
	if opts.MaxUploads <= 0 {
		opts.MaxUploads = defaultMaxUploads
	}

	c.SetMetaTimeout(opts.MetaTimeout)

	// FUSE's kernel-side permission check compares these against the caller; 0:0 made every write ACCESS fail.
	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	st := newMountState(ctx, c, uid, gid, opts)

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

	if apiSrv, err := api.Start(api.Deps{
		// buildStatus leaves Updated zero so publishStatus can exclude it from its own
		// change-detection compare; a status built for a live API response needs it set, since
		// it's fresh by definition at the moment this runs.
		Status: func() state.Status {
			s := st.buildStatus(mountpoint, opts.Version)
			s.Updated = time.Now().Unix()
			return s
		},
		CacheDir: c.CacheDir(),
		ClearCache: func() (int64, error) {
			freed, err := api.ClearCacheDir(c.CacheDir())
			c.ResetCacheSize()
			return freed, err
		},
	}); err != nil {
		slog.Warn("starting local api failed", "err", err)
	} else {
		go func() {
			<-ctx.Done()
			_ = apiSrv.Close()
		}()
	}

	go c.Events(ctx, opts.PollInterval, st.handle, state.Paused, opts.OnAuthFailed)
	go st.publishStatus(ctx, mountpoint, opts.Version)
	go st.watchdog(ctx)

	if st.thumbs != nil {
		go st.runThumbWorker(ctx)
	}
	if st.externalThumbJobs != nil {
		st.startExternalThumbWorker(ctx)
	}

	// A plain Unmount() fails while the mountpoint is busy (e.g. a shell still cd'd into it),
	// and server.Wait() would then block forever waiting for a detach that never happens; fall
	// back to a lazy unmount so Ctrl-C always returns, and let the kernel finish detaching once
	// the last reference drops.
	unmountDone := make(chan struct{})
	go func() {
		defer close(unmountDone)

		<-ctx.Done()

		st.flushDelayed()

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

	// Handlers keep running until the server is gone, and a patch from one arms a listing write
	// whose timer the exiting process never waits for, so the flush runs only once they stopped.
	st.flushListings()

	// Safety net: if the mount is still present after the shutdown select, try one more time.
	if isMounted(mountpoint) {
		bin := fusermountBinary()
		out, err := exec.Command(bin, "-u", "-z", mountpoint).CombinedOutput()
		if err != nil {
			slog.Error("safety-net unmount failed", "path", mountpoint, "err", err, "output", string(out))
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

	for {
		current := st.buildStatus(mountpoint, version)
		if !reflect.DeepEqual(current, last) {
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

// buildStatus assembles a live status snapshot: upload counters, transfer count, every transfer
// in flight and the last few finished ones, and the pause marker. Updated is left zero; the
// caller sets it only once it decides the snapshot is worth publishing (see publishStatus).
// Shared with the local API's GET /v1/status, which calls this directly instead of reading the
// file back.
func (st *mountState) buildStatus(mountpoint, version string) state.Status {
	queued, done, failed := st.uploadCounters(time.Now())

	var current []state.CurrentTransfer
	for _, t := range st.client.CurrentTransfers() {
		current = append(current, state.CurrentTransfer{
			Path:    t.Path,
			Action:  t.Action,
			Bytes:   t.Bytes(),
			Total:   t.Total,
			Started: t.Started.Unix(),
		})
	}

	var recent []state.RecentTransfer
	for _, rt := range st.recentTransfers() {
		recent = append(recent, state.RecentTransfer{
			Path:     rt.Path,
			Action:   rt.Action,
			Status:   rt.Status,
			Bytes:    rt.Bytes,
			Finished: rt.Finished.Unix(),
			Err:      rt.Err,
		})
	}

	return state.Status{
		Mountpoint:    mountpoint,
		Version:       version,
		PID:           os.Getpid(),
		Transfers:     st.client.Transfers(),
		Paused:        state.Paused(),
		UploadsQueued: queued,
		UploadsDone:   done,
		UploadsFailed: failed,
		Current:       current,
		Recent:        recent,
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

// isMounted reports whether path appears as a mountpoint in /proc/mounts.
func isMounted(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == abs {
			return true
		}
	}
	return false
}

// mountState tracks every directory node the kernel still knows and the last-known parent of each
// child listed under one, so a remote event naming a LinkID can find which cached listings to drop.
type mountState struct {
	client      *drive.Client
	uid         uint32
	gid         uint32
	opTimeout   time.Duration
	metaTimeout time.Duration

	// cacheListing replaces client.CacheListing when set; tests use it to count listing writes.
	// It lives here rather than in a package var so a background refresh left running by one test
	// writes through its own mountState instead of racing the next test's swap.
	cacheListing func(node *drive.Node, children []*drive.Node)

	// ctx is the mount's own lifetime context, cancelled on unmount. A directory listing fetch
	// parents its own deadline on this instead of on whichever request's context happened to
	// trigger it, so one caller cancelling its request never fails every other goroutine waiting
	// on the same in-flight fetch (see dirNode.finishLoad). nil in tests that build a mountState
	// directly; finishLoad falls back to context.Background() then.
	ctx context.Context

	thumbs            *thumbs.Store
	thumbJobs         chan thumbJob
	registry          *thumbs.Registry
	externalThumbJobs chan externalThumbJob
	denyReaders       []string
	denyThumbnailers  []string
	excludes          []func(string) bool

	uploadDelay time.Duration
	delayRules  []delayRule

	// delayed holds every write buffer waiting out its upload delay, so unmount can upload them
	// instead of losing them; settling counts the ones uploading, plus the external thumbnail
	// worker so unmount waits for it to kill its thumbnailer and remove its temp file. kept holds
	// the buffers parked with no timer (an excluded name, an upload that failed for good), so
	// unmount can let go of them too (see fileHandle.abandon).
	delayedMu sync.Mutex
	delayed   map[*fileHandle]struct{}
	kept      map[*fileHandle]struct{}
	settling  sync.WaitGroup

	// closing is set once flushDelayed's deadline passed or its passes ended: an upload failing
	// from then on keeps its buffer for recovery instead of parking it for a retry.
	closing atomic.Bool

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

	// recentMu guards recent, a ring of the last recentCap finished transfers, oldest first, for
	// the tray's Recent submenu and the local API's GET /v1/transfers.
	recentMu sync.Mutex
	recent   []recentTransfer

	mu            sync.Mutex
	dirs          map[string]*dirNode // folder LinkID -> its inode
	parentOf      map[string]string   // child LinkID -> parent LinkID
	thumbInflight map[string]bool     // revision key -> queued, being fetched, or given up on

	// dirtyListings holds every directory with a coalesced listing write still waiting (see
	// dirNode.schedulePersist), so unmount can write them out (see flushListings).
	listingsMu    sync.Mutex
	dirtyListings map[*dirNode]struct{}
}

// recentTransfer is one finished upload or download, kept for the tray's Recent submenu and the
// local API.
type recentTransfer struct {
	Path     string
	Action   string // "upload" or "download"
	Status   string // "done" or "failed"
	Bytes    int64
	Finished time.Time
	Err      string
}

// recentCap bounds how many finished transfers mountState.recent remembers.
const recentCap = 50

// recordFinished appends a finished transfer to the recent ring, dropping the oldest entry once
// full.
func (st *mountState) recordFinished(rt recentTransfer) {
	st.recentMu.Lock()
	st.recent = append(st.recent, rt)
	if len(st.recent) > recentCap {
		st.recent = st.recent[len(st.recent)-recentCap:]
	}
	st.recentMu.Unlock()
}

// recentTransfers returns the finished transfers, most recent first.
func (st *mountState) recentTransfers() []recentTransfer {
	st.recentMu.Lock()
	defer st.recentMu.Unlock()

	out := make([]recentTransfer, len(st.recent))
	for i, rt := range st.recent {
		out[len(st.recent)-1-i] = rt
	}
	return out
}

func newMountState(ctx context.Context, c *drive.Client, uid, gid uint32, opts Options) *mountState {
	st := &mountState{
		client:        c,
		ctx:           ctx,
		uid:           uid,
		gid:           gid,
		opTimeout:     opts.OpTimeout,
		metaTimeout:   opts.MetaTimeout,
		thumbs:        opts.Thumbnails,
		registry:      opts.Registry,
		denyReaders:   opts.DenyReaders,
		excludes:      parseExcludes(opts.Exclude),
		uploadDelay:   opts.UploadDelay,
		delayRules:    parseDelayRules(opts.UploadDelays),
		uploads:       newSem(opts.MaxUploads),
		dirs:          make(map[string]*dirNode),
		parentOf:      make(map[string]string),
		thumbInflight: make(map[string]bool),
	}

	if st.thumbs != nil {
		st.thumbJobs = make(chan thumbJob, thumbQueueSize)
	}
	if st.registry != nil && st.thumbs != nil {
		st.externalThumbJobs = make(chan externalThumbJob, externalThumbQueueSize)
		st.denyThumbnailers = st.registry.ProcessNames()
	}

	return st
}

// sem is a counting semaphore. A buffered channel is the whole implementation; a nil sem lets
func parseExcludes(patterns []string) []func(string) bool {
	var matchers []func(string) bool
	for _, p := range patterns {
		if strings.HasPrefix(p, "re:") {
			re, err := regexp.Compile(p[3:])
			if err != nil {
				slog.Warn("invalid exclude regex, skipping", "pattern", p[3:], "err", err)
				continue
			}
			matchers = append(matchers, re.MatchString)
			continue
		}
		glob := p
		matchers = append(matchers, func(name string) bool {
			matched, _ := filepath.Match(glob, name)
			return matched
		})
	}
	return matchers
}

func (st *mountState) excluded(name string) bool {
	for _, match := range st.excludes {
		if match(name) {
			return true
		}
	}
	return false
}

type delayRule struct {
	match func(string) bool
	delay time.Duration
}

// parseDelayRules parses "pattern=duration" entries, splitting at the last "=" so a regexp
// pattern may contain one.
func parseDelayRules(rules []string) []delayRule {
	var out []delayRule
	for _, r := range rules {
		i := strings.LastIndex(r, "=")
		if i <= 0 {
			slog.Warn("invalid upload delay rule, want pattern=duration, skipping", "rule", r)
			continue
		}
		delay, err := time.ParseDuration(r[i+1:])
		if err != nil {
			slog.Warn("invalid upload delay rule, skipping", "rule", r, "err", err)
			continue
		}
		matchers := parseExcludes([]string{r[:i]})
		if len(matchers) == 0 {
			continue
		}
		out = append(out, delayRule{match: matchers[0], delay: delay})
	}
	return out
}

// delayFor returns how long a file closed under name waits before it uploads.
func (st *mountState) delayFor(name string) time.Duration {
	for _, r := range st.delayRules {
		if r.match(name) {
			return r.delay
		}
	}
	return st.uploadDelay
}

func (st *mountState) trackDelayed(h *fileHandle, on bool) {
	st.trackIn(&st.delayed, h, on)
}

func (st *mountState) trackKept(h *fileHandle, on bool) {
	st.trackIn(&st.kept, h, on)
}

func (st *mountState) trackIn(set *map[*fileHandle]struct{}, h *fileHandle, on bool) {
	st.delayedMu.Lock()
	defer st.delayedMu.Unlock()
	if on {
		if *set == nil {
			*set = make(map[*fileHandle]struct{})
		}
		(*set)[h] = struct{}{}
		return
	}
	delete(*set, h)
}

// unmountFlushTimeout bounds flushDelayed's uploads as a whole. It has to fit, with
// unmountCancelGrace and the moves to state.UnsavedDir after it, inside systemd's default
// TimeoutStopSec of 90s: past that the unit is SIGKILLed and buffers still in $TMPDIR are lost.
// A var only so tests can shorten it.
var unmountFlushTimeout = 60 * time.Second

// unmountCancelGrace is how long flushDelayed waits, once its deadline cancels the uploads still
// running, for them to return and keep their buffers (keepForRetry while closing).
const unmountCancelGrace = 5 * time.Second

// flushDelayed uploads every buffer still waiting out its delay and waits for them, so an unmount
// does not drop a save made just before it. An upload that fails parks its buffer again for a
// retry, after the snapshot a pass works from, so a second pass gives each of those one more try.
// Both passes share one deadline (unmountFlushTimeout). Whatever is still unsaved after that is
// kept out of $TMPDIR: an upload cancelled by the deadline keeps its buffer itself (keepForRetry
// sees closing), and abandonKept takes every buffer still parked.
// ponytail: a settle the timer started a moment before this may finish after the process exits
func (st *mountState) flushDelayed() {
	ctx, cancel := context.WithTimeout(context.Background(), unmountFlushTimeout)
	defer cancel()

	for range 2 {
		if !st.settleDelayed(ctx) {
			slog.Warn("unmount upload deadline passed, keeping the rest for recovery", "timeout", unmountFlushTimeout)
			break
		}
	}

	st.closing.Store(true)
	cancel()
	grace, cancelGrace := context.WithTimeout(context.Background(), unmountCancelGrace)
	defer cancelGrace()
	st.waitSettling(grace)
	st.abandonKept()
}

// waitSettling waits for every settle running to finish, or for ctx; false when ctx came first.
func (st *mountState) waitSettling(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		st.settling.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// settleDelayed uploads every buffer waiting out a delay right now under ctx, and waits for them
// until ctx is done; false when it ran out first.
func (st *mountState) settleDelayed(ctx context.Context) bool {
	st.delayedMu.Lock()
	pending := make([]*fileHandle, 0, len(st.delayed))
	for h := range st.delayed {
		pending = append(pending, h)
	}
	st.delayedMu.Unlock()

	if len(pending) > 0 {
		slog.Info("uploading delayed files before unmount", "count", len(pending))
	}
	for _, h := range pending {
		st.settling.Add(1)
		go func() {
			defer st.settling.Done()
			h.settle(ctx)
		}()
	}
	return st.waitSettling(ctx)
}

// abandonKept lets go of every buffer still parked once flushDelayed's uploads ran: the ones with
// no timer (an excluded name, an upload that failed for good) and the ones whose retry would only
// come after the process exits.
func (st *mountState) abandonKept() {
	st.delayedMu.Lock()
	kept := make([]*fileHandle, 0, len(st.kept)+len(st.delayed))
	for h := range st.kept {
		kept = append(kept, h)
	}
	for h := range st.delayed {
		kept = append(kept, h)
	}
	st.delayedMu.Unlock()

	for _, h := range kept {
		h.abandon()
	}
}

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
	id := d.linkID()
	st.mu.Lock()
	st.dirs[id] = d
	st.mu.Unlock()
}

// unregister drops d from st.dirs once the kernel has forgotten it (see dirNode.OnForget), so the
// map, and every listing it pins, only covers directories the kernel still knows instead of every
// directory ever walked. An event for a forgotten directory then has nothing to invalidate, which
// is fine: the kernel holds no dentries under it, and the next lookup builds a fresh, cold dirNode.
// A newer dirNode registered for the same LinkID (a lookup that raced this forget) is left alone.
// ponytail: a cold dirNode serves the disk-persisted listing once before its background refresh,
// so a remote change made while the directory was forgotten can show stale for that one call.
func (st *mountState) unregister(d *dirNode) {
	id := d.linkID()

	st.mu.Lock()
	current := st.dirs[id] == d
	st.mu.Unlock()

	// A coalesced listing write still waiting must not fire after a fresh dirNode for this folder
	// has persisted its own, newer listing: gens are per dirNode, so nothing would stop the older
	// view from overwriting it. While d is the registered one no such dirNode exists yet, so its
	// write goes out now; when a newer one already took over, d's view is the stale one and is
	// dropped.
	if !current {
		d.discardListing()
		return
	}
	d.flushListing()

	children := d.listing()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.dirs[id] != d {
		return
	}
	delete(st.dirs, id)
	st.forgetChildrenLocked(id, children)
}

// forgetChildren drops the parentOf entries of the children of a folder that no longer exists
// remotely (a deleted folder's children never get an event of their own). The dirNode itself stays
// registered: the kernel may still hold it, and a later Lookup hands that same inode back through
// GetChild without registering it again, so removing it here would leave it deaf to events until
// the kernel forgets it. OnForget unregisters it then.
func (st *mountState) forgetChildren(linkID string) {
	st.mu.Lock()
	d, ok := st.dirs[linkID]
	st.mu.Unlock()
	if !ok {
		return
	}

	// d.mu is never taken under st.mu, so the listing is read with st.mu released.
	children := d.listing()
	st.mu.Lock()
	st.forgetChildrenLocked(linkID, children)
	st.mu.Unlock()
}

// forgetChildrenLocked drops the parentOf entry of every child that still points at parentID; one
// that moved since and was re-listed under another parent keeps its entry. Caller holds st.mu.
func (st *mountState) forgetChildrenLocked(parentID string, children []*drive.Node) {
	for _, ch := range children {
		if st.parentOf[ch.Link.LinkID] == parentID {
			delete(st.parentOf, ch.Link.LinkID)
		}
	}
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
	slog.Info("remote change applied", "path", displayPath(d.relPath()))
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
		st.forgetChildren(ev.LinkID)
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

// metaOps are the track() op names bounded by MetaTimeout rather than OpTimeout (see
// dirNode.finishLoad, Mkdir, remove, Rename); logStuckOps uses it to pick the right staleness
// threshold per operation instead of a single one for every handler.
var metaOps = map[string]bool{
	"readdir": true,
	"mkdir":   true,
	"remove":  true,
	"rename":  true,
}

// staleThreshold is the age an in-flight op has to reach before logStuckOps reports it: a
// metadata op (see metaOps) is bounded by MetaTimeout, everything else by the much longer
// OpTimeout.
func staleThreshold(op string, opTimeout, metaTimeout time.Duration) time.Duration {
	if metaOps[op] {
		return 2 * metaTimeout
	}
	return 2 * opTimeout
}

func (st *mountState) logStuckOps() {
	now := time.Now()

	count := 0
	st.inflight.Range(func(_, v any) bool {
		count++
		op := v.(inflightOp)
		if age := now.Sub(op.started); age > staleThreshold(op.op, st.opTimeout, st.metaTimeout) {
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

// openErrno maps a failed OpenFile/downloadInto call to the errno returned to the kernel. A 404
// from Proton means the file's link or active revision no longer exists remotely — the case a
// listing served straight from the disk cache on a cold start can hit, since it was never checked
// against the network before being handed out (see dirNode.serveFromDiskCache) — so this also
// expires parent's cached listing, the same self-heal a remote delete event would trigger, instead
// of failing every open of this now-ghost entry the same way until the next TTL refetch.
func openErrno(err error, parent *dirNode, opCtx context.Context, verb, path string) syscall.Errno {
	if drive.IsNotFound(err) {
		if parent != nil {
			parent.expire()
		}
		slog.Warn(verb+" failed: no longer exists remotely, dropped cached listing", "path", path)
		return syscall.ENOENT
	}
	if timedOut(opCtx) {
		slog.Error(verb+" timed out", "path", path)
		return syscall.ETIMEDOUT
	}
	slog.Error(verb+" failed", "path", path, "err", err)
	return syscall.EIO
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

	// notifying is set while a background NotifyEntry pass for this dir is in flight, so a burst
	// of remote events dedupes to one pass instead of stacking up goroutines.
	notifying atomic.Bool

	// lastWarnedAt is the unix-nano time warnOncePerMinute last actually logged for this
	// directory, rate-limiting the noisy paths that would otherwise fire on every failed refresh
	// or every FUSE call while a directory stays unhealthy (stale-over-blocking, an expired
	// session).
	lastWarnedAt atomic.Int64

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

	// persistMu serializes writes to the disk-persisted listing (see persistListing), so two
	// concurrent patches - or a patch racing a network fetch's own persist - can't land on disk out
	// of order. persistedGen is the gen (see gen above) of the last write that won, guarded by
	// persistMu, not d.mu: a write for a gen it has already moved past is dropped instead of
	// clobbering newer content with older.
	persistMu    sync.Mutex
	persistedGen uint64

	// persistTimer is non-nil while a coalesced listing write is waiting (see schedulePersist).
	// Guarded by mu.
	persistTimer *time.Timer
}

// setNode replaces the node backing this directory, e.g. after a remote listing refresh.
func (d *dirNode) setNode(n *drive.Node) {
	d.mu.Lock()
	d.node = n
	d.mu.Unlock()
}

// linkID returns the LinkID of the folder this directory stands for. node is read under d.mu since
// setNode can swap it from another goroutine.
func (d *dirNode) linkID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.node.Link.LinkID
}

// relPath returns this folder's path relative to the mountpoint, "" for the root. The thumbnail
// cache is keyed by the absolute path of each file, so children need it to build theirs. It is
// read off the inode tree on every call instead of stored: a stored copy went stale when this
// folder or any folder above it was renamed, and thumbnails went to the old path.
func (d *dirNode) relPath() string {
	return d.Path(nil)
}

// inTree reports whether this directory is still attached to the mount's inode tree. One removed
// by Rmdir, or by a rename over it, is detached, and relPath then returns "", the root's own path:
// fine for a log line, wrong for anything keyed by the path, such as a thumbnail.
func (d *dirNode) inTree() bool {
	// A dirNode never handed to NewInode has no bridge, and IsRoot would dereference it. Every
	// attached dirNode, root included, carries S_IFDIR; the root's Ino can legitimately be 0.
	if d.StableAttr().Mode == 0 {
		return false
	}
	// Walk all the way up: a removed ancestor detaches the whole subtree, while each child still
	// points at its (now detached) parent, and relPath then stops short at that ancestor.
	n := &d.Inode
	for !n.IsRoot() {
		_, n = n.Parent()
		if n == nil {
			return false
		}
	}
	return true
}

// listing returns the in-memory listing as it is right now, nil when none was loaded.
func (d *dirNode) listing() []*drive.Node {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.children
}

// OnForget runs once the kernel has dropped its last reference to this directory and go-fuse has
// removed it from the tree (go-fuse keeps a directory alive while it has children, so every inode
// under it is gone by now). Unregistering it here is what bounds st.dirs to live directories.
func (d *dirNode) OnForget() {
	d.st.unregister(d)
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
var _ = (fs.NodeOnForgetter)((*dirNode)(nil))

func (d *dirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = d.Mode() | 0o755
	out.Ino = d.StableAttr().Ino
	out.Uid = d.st.uid
	out.Gid = d.st.gid
	d.mu.Lock()
	node := d.node
	d.mu.Unlock()
	if node != nil {
		t := uint64(node.ModTime().Unix())
		out.Mtime = t
		out.Atime = t
		out.Ctime = t
	}
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

// staleDecision is what load should do once a fresh listing cannot be delivered right now: serve
// the last-known one if there is one (the stale-over-blocking rule), or fail -- with ETIMEDOUT for a
// rate-limited caller that should just retry shortly rather than wait out Proton's backoff
// window, or with the refresh's own errno (usually ETIMEDOUT, now from the shorter MetaTimeout
// deadline) otherwise.
type staleDecision int

const (
	serveStale staleDecision = iota
	failRateLimited
	failRefresh
)

// decideStale is load's stale-over-blocking rule as a pure function: hasCached is whether a
// listing (in memory, even expired, or on disk) exists for this directory at all; refreshFailed
// is whether a network refresh was attempted and came back with an error; rateLimited is whether
// Proton's shared backoff window (drive.Client.RateLimited) is active. load never attempts a
// refresh while rate limited (see below), so the two are never both true in practice, but the
// function still resolves sensibly if they were.
func decideStale(hasCached, refreshFailed, rateLimited bool) staleDecision {
	if hasCached && (refreshFailed || rateLimited) {
		return serveStale
	}
	if rateLimited {
		return failRateLimited
	}
	return failRefresh
}

func (d *dirNode) logOncePerMinute(fn func(string, ...any), msg string, args ...any) {
	now := time.Now()
	last := d.lastWarnedAt.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < time.Minute {
		return
	}
	if !d.lastWarnedAt.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	fn(msg, args...)
}

func (d *dirNode) warnOncePerMinute(msg string, args ...any) {
	d.logOncePerMinute(slog.Warn, msg, args...)
}

func (d *dirNode) errorOncePerMinute(msg string, args ...any) {
	d.logOncePerMinute(slog.Error, msg, args...)
}

// load returns the cached children, refetching them once ttl has elapsed. The fetch itself runs
// detached from any single caller (see finishLoad): every caller here, whether it triggers the
// fetch or finds one already running, only ever waits for it to finish and respects nothing but
// its own ctx while doing so, so one caller's cancellation or timeout never fails another's.
//
// A refresh that fails, times out, or is skipped because Proton is rate-limiting the client never
// fails a caller that has something to show: the last-known listing is served instead, however
// stale, and the background refresh keeps running so the directory heals on its own once it can
// (see decideStale). Only a directory with nothing cached at all reports an error.
func (d *dirNode) load(ctx context.Context) ([]*drive.Node, syscall.Errno) {
	if children, ok := d.serveFromDiskCache(); ok {
		return children, 0
	}

	if _, limited := clientRateLimited(d.client); limited {
		d.mu.Lock()
		children := d.children
		d.mu.Unlock()

		if decideStale(children != nil, false, true) == serveStale {
			d.warnOncePerMinute("serving stale listing, rate limited", "path", displayPath(d.relPath()))
			return children, 0
		}
		return nil, syscall.ETIMEDOUT
	}

	for {
		children, done, wait := d.beginLoad()
		if children != nil {
			return children, 0
		}

		if done != nil {
			go d.finishLoad(done)
			wait = done
		} else if logx.DebugEnabled(ctx) {
			slog.Debug("singleflight wait", "op", "readdir", "path", displayPath(d.relPath()))
		}

		select {
		case <-wait:
			return d.loadResult()
		case <-ctx.Done():
			return nil, syscall.EINTR
		}
	}
}

// serveFromDiskCache checks the persisted listing cache on a cold directory (no in-memory listing
// yet, no fetch already in flight): if one is there, it is published as the current listing,
// already expired so the very next load call triggers a real refresh, and a background fetch
// through the normal singleflight path is kicked off immediately (see finishLoad) — every caller
// after this one waits on that fetch as usual, only this one gets to skip the network round-trip.
func (d *dirNode) serveFromDiskCache() ([]*drive.Node, bool) {
	d.mu.Lock()
	cold := d.children == nil && d.loading == nil
	node := d.node
	d.mu.Unlock()
	if !cold {
		return nil, false
	}

	children, ok := cachedChildren(d.client, node)
	if !ok {
		return nil, false
	}

	d.mu.Lock()
	if d.children != nil || d.loading != nil {
		// A real fetch started (or finished) while CachedChildren ran; let it win.
		d.mu.Unlock()
		return children, true
	}
	d.children = children
	d.expires = time.Time{}
	d.loading = make(chan struct{})
	done := d.loading
	d.mu.Unlock()

	go d.finishLoad(done)

	return children, true
}

// loadResult reads the outcome of the fetch that just finished: the freshly published listing on
// success, or, on failure, the stale-over-blocking fallback (see decideStale) -- the last-known
// listing if one is cached, however out of date, and only the failure's errno when nothing at all
// is cached yet.
func (d *dirNode) loadResult() ([]*drive.Node, syscall.Errno) {
	d.mu.Lock()
	children, expires, errno := d.children, d.expires, d.loadErrno
	d.mu.Unlock()

	if children != nil && time.Now().Before(expires) {
		return children, 0
	}

	if decideStale(children != nil, errno != 0, false) == serveStale {
		d.warnOncePerMinute("serving stale listing, refresh failed", "path", displayPath(d.relPath()), "err", errno)
		return children, 0
	}
	return nil, errno
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
	} else {
		d.expires = time.Now().Add(min(d.ttl, failedLoadCooldown))
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
	opCtx, cancel := context.WithTimeout(parent, d.st.metaTimeout)
	defer cancel()

	children, err := fetchChildren(opCtx, d.client, node)
	if err != nil {
		errno := syscall.EIO
		switch {
		case timedOut(opCtx):
			errno = syscall.ETIMEDOUT
			d.errorOncePerMinute("readdir timed out", "path", displayPath(d.relPath()), "timeout", d.st.metaTimeout)
		case drive.IsUnauthorized(err):
			d.warnOncePerMinute("session expired, run: proton-drive-fs login", "path", displayPath(d.relPath()))
		default:
			d.errorOncePerMinute("readdir failed", "path", displayPath(d.relPath()), "err", err)
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
	d.persistListing(node, children, startGen)

	parentID := node.Link.LinkID
	for _, ch := range children {
		d.st.setParent(ch.Link.LinkID, parentID)
	}

	// A directory removed while this fetch was in flight would key its thumbnails under the root.
	if d.st.thumbs != nil && d.inTree() {
		go d.st.queueThumbs(d.relPath(), children)
	}
}

// fetchChildren lists node's children over the network; overridden in tests to fake a slow or
// failing fetch without a real drive.Client.
var fetchChildren = func(ctx context.Context, c *drive.Client, node *drive.Node) ([]*drive.Node, error) {
	return c.Children(ctx, node)
}

// driveMove, driveTrash and driveUpload are the drive writes Rename and Release make; overridden in
// tests to fake them without a live drive.Client.
var driveMove = func(ctx context.Context, c *drive.Client, n, oldParent, newParent *drive.Node, newName string) (*drive.Node, error) {
	return c.Move(ctx, n, oldParent, newParent, newName)
}

var driveTrash = func(ctx context.Context, c *drive.Client, parent, n *drive.Node) error {
	return c.Trash(ctx, parent, n)
}

var driveUpload = func(ctx context.Context, c *drive.Client, parent *drive.Node, name string, existing *drive.Node, r io.Reader, size int64, modTime time.Time) (*drive.Node, error) {
	return c.Upload(ctx, parent, name, existing, r, size, modTime)
}

// clientRateLimited reports whether c's shared backoff window (see drive.Client.RateLimited) is
// active; overridden in tests to simulate Proton rate-limiting without a live drive.Client.
var clientRateLimited = func(c *drive.Client) (time.Duration, bool) {
	return c.RateLimited()
}

// cachedChildren rebuilds a directory's children from the persisted listing cache, without a
// network call; overridden in tests. See drive.Client.CachedChildren.
var cachedChildren = func(c *drive.Client, node *drive.Node) ([]*drive.Node, bool) {
	return c.CachedChildren(node)
}

// persistListing writes children to the disk-persisted listing cache, serialized against every
// other persist for this directory: gen (d.gen at the moment children was snapshotted, under
// d.mu) orders the writes, and one that would go backward - it landed after a persist for a newer
// gen already won - is dropped instead of clobbering that newer content with older.
func (d *dirNode) persistListing(node *drive.Node, children []*drive.Node, gen uint64) {
	d.persistMu.Lock()
	defer d.persistMu.Unlock()
	d.persistListingLocked(node, children, gen)
}

// persistListingLocked is persistListing for a caller already holding d.persistMu.
func (d *dirNode) persistListingLocked(node *drive.Node, children []*drive.Node, gen uint64) {
	if gen < d.persistedGen {
		return
	}
	d.persistedGen = gen

	if d.st != nil && d.st.cacheListing != nil {
		d.st.cacheListing(node, children)
		return
	}
	d.client.CacheListing(node, children)
}

// listingPersistDelay is how long a patched listing waits before it is written to disk. Each write
// re-marshals the whole folder, so writing on every patch made a bulk copy of N files into one
// folder write O(N^2) bytes (10k files is ~200 GB); coalescing caps it at one write per folder per
// delay.
const listingPersistDelay = 2 * time.Second

// schedulePersist marks the listing dirty and, unless a write is already waiting, arms one for
// listingPersistDelay from now. The timer is not reset by later patches, so a long bulk copy still
// writes every listingPersistDelay instead of only once it goes quiet.
// ponytail: a crash loses at most the last listingPersistDelay of patches to the disk cache, which
// only serves a cold start once before the background refresh replaces it.
func (d *dirNode) schedulePersist() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.persistTimer != nil {
		return
	}
	d.persistTimer = time.AfterFunc(listingPersistDelay, d.flushListing)
	d.st.trackListing(d, true)
}

// flushListing writes the listing now if a coalesced write is waiting, and is a no-op otherwise,
// so the timer and an unmount flush racing each other write it once. persistMu is held from the
// check through the write, so a flush that finds the write already taken by the timer still waits
// for it to finish: an unmount flush that returned mid-write would let the process exit under it.
// Taking persistMu before d.mu is safe because nothing takes persistMu while holding d.mu.
func (d *dirNode) flushListing() {
	d.persistMu.Lock()
	defer d.persistMu.Unlock()

	d.mu.Lock()
	if d.persistTimer == nil {
		d.mu.Unlock()
		return
	}
	d.persistTimer.Stop()
	d.persistTimer = nil
	node, children, gen := d.node, d.children, d.gen
	d.mu.Unlock()

	d.persistListingLocked(node, children, gen)

	// d stays tracked until its write is done, so an unmount flushListings running meanwhile still
	// finds it and waits on persistMu above. A schedulePersist that re-armed during the write keeps
	// its tracking.
	d.mu.Lock()
	if d.persistTimer == nil {
		d.st.trackListing(d, false)
	}
	d.mu.Unlock()
}

// discardListing cancels a coalesced listing write still waiting, without writing it.
func (d *dirNode) discardListing() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.persistTimer == nil {
		return
	}
	d.persistTimer.Stop()
	d.persistTimer = nil
	d.st.trackListing(d, false)
}

// trackListing adds d to, or removes it from, the directories with a listing write waiting.
func (st *mountState) trackListing(d *dirNode, on bool) {
	st.listingsMu.Lock()
	defer st.listingsMu.Unlock()
	if on {
		if st.dirtyListings == nil {
			st.dirtyListings = make(map[*dirNode]struct{})
		}
		st.dirtyListings[d] = struct{}{}
		return
	}
	delete(st.dirtyListings, d)
}

// flushListings writes every listing still waiting out listingPersistDelay, so an unmount does
// not leave the disk cache behind the last patches.
func (st *mountState) flushListings() {
	st.listingsMu.Lock()
	dirty := make([]*dirNode, 0, len(st.dirtyListings))
	for d := range st.dirtyListings {
		dirty = append(dirty, d)
	}
	st.listingsMu.Unlock()

	for _, d := range dirty {
		d.flushListing()
	}
}

// upsertChild patches one entry into the cached listing, replacing an entry with the same link or
// the same name. Handlers do this instead of re-listing the parent after a write: a re-list per
// written file makes a bulk copy quadratic (10k files into one folder is ~340k listing requests
// at 150 links per page).
func (d *dirNode) upsertChild(n *drive.Node) {
	d.mu.Lock()
	d.children = upsertNode(d.children, n)
	d.gen++
	node := d.node
	d.mu.Unlock()

	d.st.setParent(n.Link.LinkID, node.Link.LinkID)
	d.schedulePersist()
}

// removeChild drops the entry named name from the cached listing.
func (d *dirNode) removeChild(name string) {
	d.mu.Lock()
	removed, children := removeNode(d.children, name)
	d.children = children
	d.gen++
	d.mu.Unlock()

	// Delete, rename source, trashed rename destination. A detached dir's relPath is "", which
	// would hit a same-named file at the root instead.
	if d.inTree() {
		d.st.forgetThumbs(path.Join(d.relPath(), name))
	}
	if removed != nil {
		d.st.forgetParent(removed.Link.LinkID)
	}
	d.schedulePersist()
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
	if d.st.excluded(name) {
		// Listings never show an excluded name; only a local, not-yet-uploaded file can have one.
		fn := d.pendingChild(name)
		if fn == nil {
			return nil, syscall.ENOENT
		}
		var attr fuse.AttrOut
		_ = fn.Getattr(ctx, nil, &attr) // never fails: it only reads local state
		out.Attr = attr.Attr
		return fn.EmbeddedInode(), 0
	}

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

	// A file still waiting out its upload delay is not on the drive yet.
	if fn := d.pendingChild(name); fn != nil {
		var attr fuse.AttrOut
		_ = fn.Getattr(ctx, nil, &attr) // never fails: it only reads local state
		out.Attr = attr.Attr
		return fn.EmbeddedInode(), 0
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
	target := path.Join(d.relPath(), name)
	slog.Info("creating folder", "path", target)

	defer d.st.track("mkdir", target)()
	opCtx, cancel := context.WithTimeout(ctx, d.st.metaTimeout)
	defer cancel()

	created, err := d.client.CreateDir(opCtx, d.node, name)
	if err != nil {
		if timedOut(opCtx) {
			slog.Error("creating folder timed out", "path", target, "timeout", d.st.metaTimeout)
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
		slog.Error("creating temp file failed", "path", path.Join(d.relPath(), name), "err", err)
		return nil, nil, 0, syscall.EIO
	}

	fn := &fileNode{client: d.client, parent: d, name: name, pinned: true}
	handle := &fileHandle{node: fn, tmp: tmp, dirty: true, owns: true}
	fn.handle = handle

	// Persistent while pending: the tree is the only place a file not on the drive can be found
	// by name, and a plain inode leaves it once the kernel forgets it, stranding a parked buffer
	// (an excluded "x.3mf.tmp" waiting for its rename) where no Lookup or Rename reaches it.
	// detach unpins it once the buffer is uploaded or dropped.
	// ponytail: inode number derived from parent+name until the real LinkID exists after upload
	attr := fs.StableAttr{Mode: fuse.S_IFREG, Ino: inodeNum(d.node.Link.LinkID + "/" + name)}
	inode := d.NewPersistentInode(ctx, fn, attr)

	out.Ino = attr.Ino
	out.Mode = fuse.S_IFREG | 0o644
	out.Uid = d.st.uid
	out.Gid = d.st.gid

	return inode, handle, 0, 0
}

// Unlink trashes a file.
func (d *dirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	// A parked buffer was never uploaded, so drop it; a file that never reached the drive needs
	// nothing else.
	if fn := d.fileChild(name); fn != nil {
		if h := fn.parkedHandle(); h != nil && h.unpark(false) {
			_ = h.Release(ctx)
			if fn.pending() {
				return 0
			}
		}
		// Still open or uploading (create, unlink, write, close is a common temp-file pattern):
		// it is not on the drive yet, so there is nothing to trash, only an upload to stop.
		if fn.unlinkPending() {
			return 0
		}
	}
	return d.remove(ctx, name)
}

// fileChild returns the file mounted under name, if any.
func (d *dirNode) fileChild(name string) *fileNode {
	child := d.GetChild(name)
	if child == nil {
		return nil
	}
	fn, _ := child.Operations().(*fileNode)
	return fn
}

// pendingChild returns the file mounted under name that has not been uploaded yet, if any.
func (d *dirNode) pendingChild(name string) *fileNode {
	if fn := d.fileChild(name); fn != nil && fn.pending() {
		return fn
	}
	return nil
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

	targetPath := path.Join(d.relPath(), name)
	slog.Info("deleting", "path", targetPath)

	defer d.st.track("remove", targetPath)()
	opCtx, cancel := context.WithTimeout(ctx, d.st.metaTimeout)
	defer cancel()

	if err := d.client.Trash(opCtx, d.node, target); err != nil {
		if timedOut(opCtx) {
			slog.Error("deleting timed out", "path", targetPath, "timeout", d.st.metaTimeout)
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
	if errno == syscall.ENOENT {
		return d.renamePending(ctx, name, newDir, newName)
	}
	if errno != 0 {
		return errno
	}

	moved, errno := d.moveLink(ctx, target, name, newDir, newName)
	if errno != 0 {
		return errno
	}

	// A pending file under newName, or a parked edit of the file just trashed there, would
	// otherwise still upload over the file moved in.
	newDir.replace(ctx, newName, d.fileChild(name), target.Link.LinkID)

	// The kernel holds both directories locked through this call, so the inode under name is
	// the one it is renaming.
	if child := d.GetChild(name); child != nil {
		refreshNode(child, moved)
		if fn, ok := child.Operations().(*fileNode); ok {
			fn.mu.Lock()
			fn.name = newName
			fn.parent = newDir
			fn.mu.Unlock()
		}
	}

	// An existing file with a parked buffer (edited, closed, still waiting) starts its wait over
	// under the new name, the same as a pending file does in renamePending.
	if child := d.GetChild(name); child != nil {
		if fn, ok := child.Operations().(*fileNode); ok {
			if h := fn.parkedHandle(); h != nil && h.unpark(true) {
				return h.Release(ctx)
			}
		}
	}

	return 0
}

// moveLink moves target, listed as name in d, to newName in newDir on the drive and patches both
// cached listings, without touching any inode: Rename updates the one the kernel renamed, and
// Release the file whose upload it moves, which by then may not be the inode under name any more.
func (d *dirNode) moveLink(ctx context.Context, target *drive.Node, name string, newDir *dirNode, newName string) (*drive.Node, syscall.Errno) {
	from := path.Join(d.relPath(), name)
	to := path.Join(newDir.relPath(), newName)

	action := "moving"
	if d == newDir {
		action = "renaming"
	}
	slog.Info(action, "from", from, "to", to)

	defer d.st.track("rename", from+" -> "+to)()
	opCtx, cancel := context.WithTimeout(ctx, d.st.metaTimeout)
	defer cancel()

	// POSIX rename() atomically replaces the destination when it exists. Proton's Move API does
	// not, so trash the existing destination first.
	var trashed *drive.Node
	if existing, err := newDir.findChild(ctx, newName); err == 0 && existing.Link.LinkID != target.Link.LinkID {
		if trashErr := driveTrash(opCtx, d.client, newDir.node, existing); trashErr != nil {
			slog.Error("trashing rename destination failed", "path", to, "err", trashErr)
			return nil, syscall.EIO
		}
		newDir.removeChild(newName)
		trashed = existing
	}

	moved, err := driveMove(opCtx, d.client, target, d.node, newDir.node, newName)
	if err != nil {
		newDir.unlinkTrashed(newName, trashed)
		if timedOut(opCtx) {
			slog.Error(action+" timed out", "from", from, "to", to, "timeout", d.st.metaTimeout)
			return nil, syscall.ETIMEDOUT
		}
		slog.Error(action+" failed", "from", from, "to", to, "err", err)
		return nil, syscall.EIO
	}

	d.removeChild(name)
	newDir.upsertChild(moved)
	return moved, 0
}

// unlinkTrashed is moveLink's way back when the move failed after the destination, trashed (nil
// when there was none), already went to the trash; the drive client has no restore call. A write
// buffer of the file under name (an edit still waiting, possibly the newest save) would otherwise
// upload as a revision of that trashed link, out of sight, so the file goes back to pending and
// the buffer uploads as a new file under its name. A file with no buffer stays as it is, its
// content in Proton's trash.
// ponytail: an upload of that buffer already running still targets the trashed link
func (d *dirNode) unlinkTrashed(name string, trashed *drive.Node) {
	if trashed == nil {
		return
	}
	dst := d.fileChild(name)
	if dst == nil {
		return
	}

	dst.mu.Lock()
	defer dst.mu.Unlock()
	if dst.handle == nil || dst.node == nil || dst.node.Link.LinkID != trashed.Link.LinkID {
		return
	}
	slog.Warn("rename destination trashed but the move failed; its unsaved edit will upload as a new file", "path", path.Join(d.relPath(), name))
	dst.node = nil
}

// renamePending handles rename when the source is a pending file (created via Create, not yet
// uploaded to Proton). The file has a FUSE inode but no drive.Node in the children list.
// We update the fileNode's name/parent so Release uploads to the final path, and if the
// destination already exists we set fn.node so Upload creates a new revision instead of a
// new file.
func (d *dirNode) renamePending(ctx context.Context, name string, newDir *dirNode, newName string) syscall.Errno {
	child := d.GetChild(name)
	if child == nil {
		return syscall.ENOENT
	}
	fn, ok := child.Operations().(*fileNode)
	if !ok {
		return syscall.ENOENT
	}

	slog.Info("renaming pending file", "from", path.Join(d.relPath(), name), "to", path.Join(newDir.relPath(), newName))

	newDir.replace(ctx, newName, fn, "")
	existing, _ := newDir.findChild(ctx, newName)

	fn.mu.Lock()
	fn.name = newName
	fn.parent = newDir
	if existing != nil {
		fn.node = existing
	}
	fn.mu.Unlock()

	// Already closed and parked: release it again under the new name, which uploads it now, starts
	// the new name's upload delay over, or keeps it parked if the new name is excluded too.
	if h := fn.parkedHandle(); h != nil && h.unpark(true) {
		return h.Release(ctx)
	}

	return 0
}

// replace takes the file mounted under name out of the way of a rename putting src over it
// (srcLink is src's LinkID when it is on the drive). The rename replaces that file, so its write
// buffer is marked gone and, when parked, dropped right away. Without this a pending file renamed
// over another pending one (two atomic saves within the upload delay: x.tmp1 -> x, x.tmp2 -> x)
// left the first one's timer to upload "x" as a new file, and the second then failed to create
// "x" on every retry. src waits out an upload of it already in flight (see awaitReplaced). A
// file under name whose link is srcLink is the moved file itself, so it is left alone.
//
// A destination still open for write loses what that writer goes on to save: its close drops
// the buffer. That is POSIX rename(), deliberately: the writer's descriptor refers to the
// replaced inode, which is unlinked now, and uploading its close over the renamed-in file (what
// happened before this) overwrote the newer save with the older one. A buffer whose upload
// already failed for good is kept anyway (discard).
func (d *dirNode) replace(ctx context.Context, name string, src *fileNode, srcLink string) {
	dst := d.fileChild(name)
	if dst == nil || dst == src {
		return
	}

	dst.mu.Lock()
	if srcLink != "" && dst.node != nil && dst.node.Link.LinkID == srcLink {
		dst.mu.Unlock()
		return
	}
	h := dst.handle
	dst.mu.Unlock()

	if src != nil {
		src.mu.Lock()
		src.replaces = dst
		src.mu.Unlock()
	}

	if h == nil {
		return
	}
	h.gone.Store(true)
	if h.unpark(false) {
		_ = h.Release(ctx)
	}
}

func (d *dirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	children, errno := d.load(ctx)
	if errno != 0 {
		return nil, errno
	}

	entries := make([]fuse.DirEntry, 0, len(children))
	listed := make(map[string]bool, len(children))
	for _, child := range children {
		if d.st.excluded(child.Name) {
			continue
		}

		mode := uint32(fuse.S_IFREG)
		if child.IsDir() {
			mode = fuse.S_IFDIR
		}

		listed[child.Name] = true
		entries = append(entries, fuse.DirEntry{
			Name: child.Name,
			Ino:  inodeNum(child.Link.LinkID),
			Mode: mode,
		})
	}

	// Files written here but not uploaded yet (waiting out their upload delay) belong in the
	// listing too, or a just-saved file would vanish from it until the upload finishes.
	for name, child := range d.Children() {
		if listed[name] || d.st.excluded(name) || d.pendingChild(name) == nil {
			continue
		}
		entries = append(entries, fuse.DirEntry{Name: name, Ino: child.StableAttr().Ino, Mode: fuse.S_IFREG})
	}

	return fs.NewListDirStream(entries), 0
}

func (d *dirNode) makeChild(ctx context.Context, child *drive.Node, out *fuse.EntryOut) *fs.Inode {
	d.fillEntryOut(out, child)
	attr := fs.StableAttr{Ino: out.Ino}

	if child.IsDir() {
		attr.Mode = fuse.S_IFDIR
		dn := &dirNode{client: d.client, node: child, ttl: d.ttl, st: d.st}
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
		t := uint64(child.ModTime().Unix())
		out.Mtime = t
		out.Atime = t
		out.Ctime = t
		return
	}

	child.ResolveAttrs()
	out.Mode = fuse.S_IFREG | 0o644
	out.Size = uint64(child.Size())
	t := uint64(child.ModTime().Unix())
	out.Mtime = t
	out.Atime = t
	out.Ctime = t
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
	pinned bool        // a persistent inode Create made, until its buffer is uploaded or dropped (unpin)

	// replaces is the file this one was renamed over (see dirNode.replace). Release waits out an
	// upload of it, or of the files it replaced in turn, before uploading this one, so the two
	// never race for the same name.
	replaces *fileNode

	// uploadMu is held by Release from its last gone check until the upload's result is claimed
	// (see claim), so a file renamed over this one can wait for an upload already in flight.
	uploadMu sync.Mutex
}

var _ = (fs.NodeGetattrer)((*fileNode)(nil))
var _ = (fs.NodeSetattrer)((*fileNode)(nil))
var _ = (fs.NodeOpener)((*fileNode)(nil))

// unpin drops the persistence Create gave the inode, once its buffer is uploaded or dropped: from
// then on the kernel's lookups alone keep it, like any other file's.
func (f *fileNode) unpin() {
	f.mu.Lock()
	pinned := f.pinned
	f.pinned = false
	f.mu.Unlock()

	// ForgetPersistent takes go-fuse's inode locks, so it runs with f.mu released.
	if pinned {
		f.ForgetPersistent()
	}
}

// setNode replaces the node backing this file, e.g. after a remote listing refresh.
func (f *fileNode) setNode(n *drive.Node) {
	f.mu.Lock()
	f.node = n
	f.name = n.Name
	f.mu.Unlock()
}

// pending reports whether the file has not been uploaded yet.
func (f *fileNode) pending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.node == nil
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

// displayPath returns this file's path relative to the mountpoint, for the tray and the local
// API to show which file a transfer belongs to.
func (f *fileNode) displayPath() string {
	f.mu.Lock()
	parent, name := f.parent, f.name
	f.mu.Unlock()
	// relPath takes go-fuse's inode locks, so it runs with f.mu released.
	return path.Join(parent.relPath(), name)
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
				t := uint64(node.ModTime().Unix())
				out.Mtime = t
				out.Atime = t
				out.Ctime = t
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
	t := uint64(node.ModTime().Unix())
	out.Mtime = t
	out.Atime = t
	out.Ctime = t
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
	if !ok || !handle.owns || !handle.hasTmp() {
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

	readOnly := flags&syscall.O_ACCMODE == syscall.O_RDONLY

	// A parked buffer is the file's current content: a writer takes it over (an app saving again
	// within the upload delay), which also stops its timer.
	if h := f.parkedHandle(); h != nil && !readOnly && h.unpark(true) {
		if flags&syscall.O_TRUNC != 0 {
			if err := h.truncate(0); err != nil {
				slog.Error("truncating failed", "path", f.currentName(), "err", err)
				return nil, 0, syscall.EIO
			}
		}
		return h, 0, 0
	}

	if node == nil {
		// A pending (not yet uploaded) file only has its write handle's buffer to read from.
		f.mu.Lock()
		writer := f.handle
		f.mu.Unlock()
		if writer != nil && readOnly {
			if r := readerOf(f, writer); r != nil {
				return r, fuse.FOPEN_DIRECT_IO, 0
			}
		}
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

	if readOnly {
		// A dirty write handle's buffered bytes are the current content; read from the same
		// temp file instead of serving the stale committed revision. Direct I/O keeps the kernel
		// from caching pre-write bytes alongside them.
		f.mu.Lock()
		writer := f.handle
		f.mu.Unlock()

		if writer != nil {
			if r := readerOf(f, writer); r != nil {
				return r, fuse.FOPEN_DIRECT_IO, 0
			}
		}

		defer parent.st.track("open", f.currentName())()
		opCtx, cancel := context.WithTimeout(ctx, parent.st.opTimeout)
		defer cancel()

		file, err := f.client.OpenFile(opCtx, node, f.displayPath())
		if err != nil {
			return nil, 0, openErrno(err, parent, opCtx, "opening file", f.currentName())
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

		displayPath := f.displayPath()
		moved, err := downloadInto(opCtx, f.client, node, tmp, displayPath)
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			parent.st.recordFinished(recentTransfer{Path: displayPath, Action: "download", Status: "failed", Bytes: moved, Finished: time.Now(), Err: err.Error()})
			return nil, 0, openErrno(err, parent, opCtx, "downloading file", f.currentName())
		}
		if moved > 0 {
			parent.st.recordFinished(recentTransfer{Path: displayPath, Action: "download", Status: "done", Bytes: moved, Finished: time.Now()})
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
// block-caching ReadAt path as read-only opens. It returns the bytes actually moved over the
// network (0 if everything came from cache), for the caller to record as a finished transfer.
func downloadInto(ctx context.Context, client *drive.Client, node *drive.Node, tmp *os.File, path string) (int64, error) {
	file, err := client.OpenFile(ctx, node, path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()

	buf := make([]byte, 256*1024)
	var off int64
	for {
		n, err := file.ReadAt(ctx, buf, off)
		if n > 0 {
			if _, werr := tmp.WriteAt(buf[:n], off); werr != nil {
				return file.BytesTransferred(), werr
			}
			off += int64(n)
		}
		if err == io.EOF {
			return file.BytesTransferred(), nil
		}
		if err != nil {
			return file.BytesTransferred(), err
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

	mu     sync.Mutex
	tmp    *os.File // set for a write-capable handle, or a read-only view of the write handle's tmp
	dirty  bool
	owns   bool        // true only for the write handle that owns tmp: it alone closes, removes and uploads it
	parked bool        // released but not uploaded: tmp kept until timer, a rename, a reopen or Unlink moves it on
	timer  *time.Timer // set while parked for an upload delay; nil while parked under an excluded name
	due    bool        // the upload delay ran out, so the next Release uploads instead of parking again

	// retryIn is the wait before the next retry of a failed upload, doubled per failure
	// (keepForRetry) and reset when something takes the buffer back to upload it again. failed is
	// set while the buffer is parked after an upload that failed for good, so abandon keeps it.
	retryIn time.Duration
	failed  bool

	// gone is set when a rename puts another file over this buffer's file, or an Unlink removes
	// it, before the buffer reached the drive: Release drops it instead of uploading it, and
	// trashes a new file an upload already in flight created. It lives on the handle, not the
	// node, because go-fuse can hand the same inode out again for a later file under that name.
	gone atomic.Bool
}

var _ = (fs.FileReader)((*fileHandle)(nil))
var _ = (fs.FileWriter)((*fileHandle)(nil))
var _ = (fs.FileFlusher)((*fileHandle)(nil))
var _ = (fs.FileFsyncer)((*fileHandle)(nil))
var _ = (fs.FileReleaser)((*fileHandle)(nil))

// parkedHandle returns the write handle Release parked, if any.
func (f *fileNode) parkedHandle() *fileHandle {
	f.mu.Lock()
	h := f.handle
	f.mu.Unlock()

	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.parked {
		return nil
	}
	return h
}

// awaitReplaced waits out an upload still in flight of the files this one was renamed over, so
// this one's upload never races theirs for the same name: a new file one of them created would
// make this one's create fail, and a revision it committed after this one's would win. Their
// buffers are all marked gone, so none of them starts an upload once this returns.
func (f *fileNode) awaitReplaced() {
	f.mu.Lock()
	r := f.replaces
	f.mu.Unlock()

	for r != nil {
		r.uploadMu.Lock() // held for the length of r's upload, if one is running
		r.mu.Lock()
		next := r.replaces
		r.mu.Unlock()
		r.uploadMu.Unlock()
		r = next
	}
}

// unlinkPending marks the write buffer of a file not on the drive yet gone, so its Release drops
// it instead of uploading it, or trashes the new file an upload already in flight creates. False
// when the file is on the drive, for the caller to trash it there. Under f.mu, same as claim, so
// an upload finishing meanwhile is seen by exactly one of the two.
// ponytail: an Unlink between claim and the parent's upsertChild finds nothing to trash (ENOENT)
func (f *fileNode) unlinkPending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.node != nil || f.handle == nil {
		return false
	}
	f.handle.gone.Store(true)
	return true
}

// claim points the node at the link h's upload created, unless a rename over the file or an
// Unlink marked h gone while the upload ran; false means the caller trashes what it created
// instead. A claimed upload ends the wait chain, since every file it replaced has settled by now.
func (f *fileNode) claim(h *fileHandle, n *drive.Node) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if h.gone.Load() {
		return false
	}
	if n != nil {
		f.node = n
	}
	f.replaces = nil
	return true
}

// park keeps tmp after Release; with delay > 0 it uploads once delay passes without a rename,
// reopen or unlink taking it back first.
func (h *fileHandle) park(tmp *os.File, delay time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tmp = tmp
	h.parked = true

	st := h.node.mountState()
	if delay <= 0 {
		st.trackKept(h, true)
		return
	}

	st.trackDelayed(h, true)
	h.timer = time.AfterFunc(delay, func() {
		st.settling.Add(1)
		defer st.settling.Done()
		h.settle(context.Background())
	})
}

// uploadRetryDelay is how long a buffer whose upload failed waits before its first retry. Each
// further failure doubles the wait, up to uploadRetryMax, so a file that keeps failing costs one
// attempt and its few log lines every half hour instead of every 30 seconds.
const (
	uploadRetryDelay = 30 * time.Second
	uploadRetryMax   = 30 * time.Minute
)

// keepForRetry parks a buffer whose upload failed with err, so the only copy of a save is never
// deleted: its close already returned success, and nothing else would try again. The log names
// the temp file, and an unmount before the upload works moves it out of $TMPDIR for recovery
// (keepUnsaved). A permanent failure (see
// permanentUploadError) parks it with no timer instead, since retrying cannot help: it stays
// readable on the mount until a rename or a save takes it back to try again (a delete drops it).
func (h *fileHandle) keepForRetry(tmp *os.File, name string, err error) {
	// Replaced or unlinked while it tried: there is no save left to keep.
	if h.gone.Load() {
		h.discard(tmp)
		return
	}

	// Unmount's deadline cancelled it, or it failed after: a retry would come after the process
	// exits, and abandonKept may already have run, so keep the save now.
	if h.node.mountState().closing.Load() {
		h.keepUnsaved(tmp)
		return
	}

	if permanentUploadError(err) {
		slog.Error("upload failed for good, keeping the buffer; rename or save the file to try again", "path", name, "buffer", tmp.Name(), "err", err)
		h.mu.Lock()
		h.failed = true
		h.mu.Unlock()
		h.park(tmp, 0)
		return
	}

	h.mu.Lock()
	h.retryIn = min(max(2*h.retryIn, uploadRetryDelay), uploadRetryMax)
	delay := h.retryIn
	h.mu.Unlock()

	slog.Warn("keeping the buffer to retry the upload", "path", name, "buffer", tmp.Name(), "retry_in", delay)
	h.park(tmp, delay)
}

// permanentUploadError reports whether retrying an upload that failed with err cannot help. Only
// Proton codes go-proton-api itself names count: the name is taken by another file (2500 with a
// 422; the same code with a 409 is a stale draft, which clears), or the account needs a paid plan
// (10004). Anything else, other 4xx conflicts included, is retried with backoff, since a wrong
// "permanent" strands a save while a wrong "transient" only costs a retry every half hour.
func permanentUploadError(err error) bool {
	// go-proton-api wraps a pointer, drive's hand-rolled requests a value (see drive.asAPIError).
	var apiErr proton.APIError
	var p *proton.APIError
	switch {
	case err == nil:
		return false
	case errors.As(err, &p):
		apiErr = *p
	case errors.As(err, &apiErr):
	default:
		return false
	}

	switch {
	case apiErr.Code == proton.AFileOrFolderNameExist && apiErr.Status == http.StatusUnprocessableEntity:
		return true
	case apiErr.Code == proton.PaidPlanRequired:
		return true
	}
	return false
}

// discard lets go of a buffer a rename over its file, or an unlink, made obsolete. One whose
// upload failed for good is kept anyway (keepUnsaved): the log and the tray told the user it
// was, and it may be the only copy of that save.
func (h *fileHandle) discard(tmp *os.File) {
	h.mu.Lock()
	failed := h.failed
	h.mu.Unlock()

	if failed {
		h.keepUnsaved(tmp)
		return
	}
	h.drop(tmp)
}

// drop lets go of a buffer that was uploaded or never written to: the node stops pointing at this
// handle, and the temp file is closed and removed. Readers keep their own descriptors (readerOf).
func (h *fileHandle) drop(tmp *os.File) {
	h.detach()
	_ = tmp.Close()
	_ = os.Remove(tmp.Name())
}

// detach unhooks the buffer from this handle and the handle from its node, which then has nothing
// left to upload and needs no pinning (unpin).
func (h *fileHandle) detach() {
	h.mu.Lock()
	h.tmp = nil
	h.mu.Unlock()

	fn := h.node
	fn.mu.Lock()
	mine := fn.handle == h
	if mine {
		fn.handle = nil
	}
	fn.mu.Unlock()

	if mine {
		fn.unpin()
	}
}

// settle uploads a buffer whose upload delay ran out, unless something took it back first. ctx
// bounds the upload: unmount's deadline (flushDelayed), or none for a timer.
func (h *fileHandle) settle(ctx context.Context) {
	h.mu.Lock()
	if !h.parked || h.timer == nil {
		h.mu.Unlock()
		return
	}
	h.timer.Stop()
	h.parked, h.timer, h.dirty, h.due = false, nil, true, true
	h.mu.Unlock()

	h.node.mountState().trackDelayed(h, false)
	_ = h.Release(ctx)
}

// unpark takes a parked handle back for a second Release: dirty uploads (or delays again) the
// buffer, clean drops it. False when it was no longer parked, e.g. its delay just ran out.
func (h *fileHandle) unpark(dirty bool) bool {
	h.mu.Lock()
	if !h.parked {
		h.mu.Unlock()
		return false
	}
	h.parked = false
	h.dirty = dirty
	if dirty {
		h.retryIn = 0 // taken back to upload again: a fresh start, not the next step of a backoff
		h.failed = false
	}
	timer := h.timer
	h.timer = nil
	h.mu.Unlock()

	if timer == nil {
		h.node.mountState().trackKept(h, false)
		return true
	}
	timer.Stop()
	h.node.mountState().trackDelayed(h, false)
	return true
}

// abandon lets go of a parked buffer at unmount, once nothing can take it back any more. (A
// kernel forget is no such point: a pending file stays pinned in the tree, see Create.) An
// excluded name was never going to upload, so its temp file is removed. Anything else is a save
// that never reached the drive, kept for recovery (keepUnsaved).
func (h *fileHandle) abandon() {
	h.mu.Lock()
	if !h.parked {
		h.mu.Unlock()
		return
	}
	h.parked = false
	timer, tmp, failed := h.timer, h.tmp, h.failed
	h.timer = nil
	h.mu.Unlock()

	st := h.node.mountState()
	st.trackKept(h, false)
	st.trackDelayed(h, false)
	if timer != nil {
		timer.Stop()
	}

	if timer == nil && !failed {
		h.drop(tmp)
		return
	}
	h.keepUnsaved(tmp)
}

// keepUnsaved lets go of the buffer of a save that never reached the drive without losing it: the
// temp file moves out of $TMPDIR (tmpfs on many systems, emptied on reboot) into
// state.UnsavedDir, and the log names both the file and where its save went. A failed move leaves
// it where it is, still named in the log.
func (h *fileHandle) keepUnsaved(tmp *os.File) {
	where := h.node.displayPath()
	h.detach()

	saved, err := saveUnsaved(tmp, path.Base(where))
	_ = tmp.Close()
	if err != nil {
		slog.Error("save not uploaded, and moving its buffer out of the temp dir failed; it is still there", "path", where, "buffer", tmp.Name(), "err", err)
		return
	}
	slog.Error("save not uploaded, buffer moved for recovery", "path", where, "saved", saved)
}

// saveUnsaved moves tmp's file into state.UnsavedDir as "<time>-<random>-<name>", by a rename on
// the same filesystem or a copy across them, and returns its new path.
func saveUnsaved(tmp *os.File, name string) (string, error) {
	dir, err := state.UnsavedDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	dst, err := os.CreateTemp(dir, time.Now().Format("20060102-150405")+"-*-"+name)
	if err != nil {
		return "", err
	}
	defer func() { _ = dst.Close() }()

	if os.Rename(tmp.Name(), dst.Name()) == nil {
		return dst.Name(), nil
	}

	// Across filesystems (tmpfs $TMPDIR to the home disk): copy, and remove the temp file only
	// once the copy is safely written.
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		_ = os.Remove(dst.Name())
		return "", err
	}
	if _, err := io.Copy(dst, tmp); err != nil {
		_ = os.Remove(dst.Name())
		return "", err
	}
	if err := dst.Sync(); err != nil {
		_ = os.Remove(dst.Name())
		return "", err
	}
	_ = os.Remove(tmp.Name())
	return dst.Name(), nil
}

func (h *fileHandle) hasTmp() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tmp != nil
}

// sharedTmp returns the write handle's temp file, or nil if this handle has no temp buffer.
func (h *fileHandle) sharedTmp() *os.File {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tmp
}

// readerOf returns a read-only handle on writer's temp file with its own descriptor, so it keeps
// reading after the writer's upload closes and removes the file. Nil when there is no buffer, or
// it was removed in the meantime.
func readerOf(f *fileNode, writer *fileHandle) *fileHandle {
	tmp := writer.sharedTmp()
	if tmp == nil {
		return nil
	}
	r, err := os.Open(tmp.Name())
	if err != nil {
		return nil
	}
	return &fileHandle{node: f, tmp: r}
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
		// A rate-limited read maps to ETIMEDOUT, same as a deadline miss.
		if timedOut(opCtx) || errors.Is(err, drive.ErrRateLimited) {
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

// Fsync is a no-op: the temp buffer is already on local disk, so the data is durable
// from the caller's perspective. Without this, go-fuse returns ENOSYS and apps like
// LibreOffice, Kdenlive and KDE editors interpret that as a failed save.
func (h *fileHandle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	return 0
}

// Flush is a no-op: content is only uploaded once on Release.
func (h *fileHandle) Flush(ctx context.Context) syscall.Errno {
	return 0
}

// Release uploads a dirty temp buffer as a new file or a new revision, then patches the uploaded
// link into the parent listing and into this node so subsequent Getattr/Open see it. The temp
// file is always removed.
func (h *fileHandle) Release(ctx context.Context) syscall.Errno {
	// A real read-only handle (wrapping a drive.File) ends its tracked download transfer here
	// and records it as finished, if it ever touched the network.
	if h.file != nil {
		st := h.node.mountState()
		path := h.node.displayPath()
		moved := h.file.BytesTransferred()
		err := h.file.Close()
		if moved == 0 {
			return 0
		}
		if err != nil {
			st.recordFinished(recentTransfer{Path: path, Action: "download", Status: "failed", Bytes: moved, Finished: time.Now(), Err: err.Error()})
			return 0
		}
		st.recordFinished(recentTransfer{Path: path, Action: "download", Status: "done", Bytes: moved, Finished: time.Now()})
		return 0
	}

	h.mu.Lock()
	tmp := h.tmp
	dirty := h.dirty
	owns := h.owns
	due := h.due
	if !owns {
		h.tmp = nil
	}
	h.due = false
	h.mu.Unlock()

	if tmp == nil {
		return 0
	}

	// A reader (owns == false) has its own descriptor on the write handle's temp file (readerOf);
	// closing it is all its Release does. Only the write handle uploads and removes the file.
	if !owns {
		_ = tmp.Close()
		return 0
	}

	fn := h.node
	fn.mu.Lock()
	existing := fn.node
	name := fn.name
	parent := fn.parent
	fn.mu.Unlock()

	// Renamed over (see dirNode.replace) or unlinked before it reached the drive: nothing to upload.
	if h.gone.Load() {
		h.discard(tmp)
		return 0
	}

	// An excluded name (Orca saves "x.3mf.tmp", then renames it) is never uploaded. The buffer
	// stays parked on the node so renamePending uploads it under the real name, or Unlink drops it.
	// Unmount removes it (abandon).
	// ponytail: a parked buffer never renamed or unlinked stays in $TMPDIR until unmount
	if dirty && parent.st.excluded(name) {
		h.park(tmp, 0)
		return 0
	}

	// Anything else waits out its upload delay first, so the rest of an app's save sequence
	// (renames, deletes) has happened by the time the file goes to the drive.
	if delay := parent.st.delayFor(name); dirty && !due && delay > 0 {
		h.park(tmp, delay)
		return 0
	}

	if !dirty {
		h.drop(tmp)
		return 0
	}

	// The buffer stays on the node until the upload succeeds: a reader opened meanwhile still
	// finds it, and a failed upload keeps it for a retry instead of deleting the only copy of a
	// save whose close already returned success.
	displayPath := path.Join(parent.relPath(), name)

	info, err := tmp.Stat()
	if err != nil {
		slog.Error("stat temp file failed", "path", name, "err", err)
		h.keepForRetry(tmp, name, err)
		return syscall.EIO
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		slog.Error("seek temp file failed", "path", name, "err", err)
		h.keepForRetry(tmp, name, err)
		return syscall.EIO
	}

	// Wait out the upload of a file this one replaced, then check again under uploadMu: a rename
	// over this file from here on either finds this upload running and waits for it, or finds the
	// buffer gone before it starts.
	fn.awaitReplaced()
	fn.uploadMu.Lock()
	defer fn.uploadMu.Unlock()
	if h.gone.Load() {
		h.discard(tmp)
		return 0
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
		h.keepForRetry(tmp, name, ctx.Err())
		return syscall.EINTR
	}
	defer st.uploads.release()

	slog.Info("uploading file", "path", name, "size", logx.FormatSize(info.Size()))
	uploadStart := time.Now()

	timeout := uploadTimeout(info.Size(), st.opTimeout)
	defer st.track("upload", name)()
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	uploaded, err := driveUpload(opCtx, fn.client, parent.node, name, existing, tmp, info.Size(), time.Now())
	if err != nil {
		st.uploadsFailed.Add(1)
		// The tray's Recent menu is where a user notices a save that will not retry on its own.
		msg := err.Error()
		if permanentUploadError(err) {
			msg = "gave up, buffer kept at " + tmp.Name() + ": " + msg
		}
		st.recordFinished(recentTransfer{Path: displayPath, Action: "upload", Status: "failed", Bytes: info.Size(), Finished: time.Now(), Err: msg})
		if timedOut(opCtx) {
			slog.Error("uploading file timed out", "path", name, "timeout", timeout)
			h.keepForRetry(tmp, name, err)
			return syscall.ETIMEDOUT
		}
		slog.Error("uploading file failed", "path", name, "err", err)
		h.keepForRetry(tmp, name, err)
		return syscall.EIO
	}

	// Uploaded: drop the buffer once the node points at the uploaded file (below), so an open in
	// between never finds neither.
	defer h.drop(tmp)

	st.uploadsDone.Add(1)
	st.recordFinished(recentTransfer{Path: displayPath, Action: "upload", Status: "done", Bytes: info.Size(), Finished: time.Now()})
	slog.Info("uploaded file", "path", name, "size", logx.FormatSize(info.Size()), logx.Elapsed(uploadStart))

	// Renamed over or unlinked while the upload ran: a new file it created is junk, or in the way
	// of the file that replaced it, so it goes to the trash. A revision is left alone; the file it
	// revised was trashed by the rename, or now belongs to the replacing file, which uploads after
	// it. (Unlink only marks files not on the drive yet, so it never leaves a revision here.)
	if !fn.claim(h, uploaded) {
		if existing != nil {
			return 0
		}
		if uploaded == nil {
			parent.expire()
			return 0
		}
		slog.Info("deleting upload of a replaced or deleted file", "path", displayPath)
		trashCtx, cancel := context.WithTimeout(ctx, st.metaTimeout)
		defer cancel()
		if err := driveTrash(trashCtx, fn.client, parent.node, uploaded); err != nil {
			slog.Error("deleting upload of a replaced or deleted file failed", "path", displayPath, "err", err)
		}
		return 0
	}

	// Re-read parent/name: a concurrent Rename during the upload may have moved this file, and
	// the entry has to be patched in where it is now, not where it was.
	fn.mu.Lock()
	uploadedName := fn.name
	uploadedParent := fn.parent
	fn.mu.Unlock()

	// A Rename that arrived mid-upload only retargeted this node (FreeCAD writes "x.FCStd.<uuid>"
	// and renames it over "x.FCStd" while the upload runs), so the drive still has the file under
	// the old name. Move it to where it is now, by the link the upload returned, and update this
	// node only: the inode under the old name may already be another file (renamed away, then
	// saved anew under it), which going through Rename would retarget onto this file's link.
	if uploadedName != name || uploadedParent != parent {
		// Without the link, looking the old name up could find that other file, so nothing moves.
		// ponytail: the upload then stays on the drive under the old name until the user moves it
		if uploaded == nil {
			slog.Warn("uploaded file renamed meanwhile, but its link could not be read back; it stays under its old name on the drive", "path", displayPath, "renamed_to", path.Join(uploadedParent.relPath(), uploadedName))
			parent.expire()
			uploadedParent.expire()
			return 0
		}
		moved, errno := parent.moveLink(ctx, uploaded, name, uploadedParent, uploadedName)
		if errno != 0 {
			return errno
		}
		fn.setNode(moved)
		return 0
	}

	// A nil node means the upload succeeded but reading the link back did not; expire the listing
	// so the next Lookup fetches the truth.
	if uploaded == nil {
		uploadedParent.expire()
		return 0
	}

	fn.setNode(uploaded)
	uploadedParent.upsertChild(uploaded)

	return 0
}
