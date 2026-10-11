package fusefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
)

// thumbQueueSize bounds the pending thumbnail fetches. Listing a huge folder queues what fits
// and drops the rest; the next listing after the TTL picks them up.
const thumbQueueSize = 4096

// externalThumbQueueSize bounds pending external thumbnailer jobs. Smaller than the Proton
// thumbnail queue because each job downloads the full file.
const externalThumbQueueSize = 256

// externalThumbTimeout bounds the download + thumbnailer execution for one file.
const externalThumbTimeout = 2 * time.Minute

// thumbFetchTimeout bounds one Proton thumbnail fetch: two small requests (the URL, then the
// encrypted preview of a few KB).
const thumbFetchTimeout = time.Minute

// thumbJob is one file whose Proton thumbnail should be fetched and cached.
type thumbJob struct {
	node     *drive.Node
	relPath  string
	parentID string // LinkID of the folder whose listing queued it; see stillListed
}

// externalThumbJob is a file that needs an external thumbnailer run on a downloaded copy.
type externalThumbJob struct {
	thumbJob
	ext string // file extension including dot
}

// thumbKey identifies a revision, so the same fetch is never queued twice.
func thumbKey(n *drive.Node) string {
	if n.Link.FileProperties == nil {
		return n.Link.LinkID
	}
	return n.Link.LinkID + "/" + n.Link.FileProperties.ActiveRevision.ID
}

// runThumbWorker fetches queued thumbnails one at a time until ctx is done. One goroutine per
// mount keeps thumbnail traffic well behind whatever the user is actually doing.
func (st *mountState) runThumbWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-st.thumbJobs:
			st.fetchThumb(ctx, job)
		}
	}
}

func (st *mountState) fetchThumb(ctx context.Context, job thumbJob) {
	// The worker is the only one, and the HTTP client has no deadline of its own: one stalled GET
	// would wedge it for good while the queue fills and drops every later preview.
	opCtx, cancel := context.WithTimeout(ctx, thumbFetchTimeout)
	defer cancel()

	img, err := st.client.Thumbnail(opCtx, job.node)
	st.finishThumb(job, img, err)
}

// finishThumb writes a fetched or generated preview to the cache and settles the revision's mark.
// A success unmarks it right away. A transient failure unmarks it after thumbRetryAfter. Any other
// failure (a preview that does not decode, a thumbnailer that exits non-zero) keeps it marked for
// the life of the mount: it would fail the same way every time, and for an external thumbnailer
// every try downloads the whole file again.
func (st *mountState) finishThumb(job thumbJob, img []byte, err error) {
	if err == nil {
		err = st.writeThumb(job, img)
	}
	if err == nil {
		slog.Debug("thumbnail written", "path", job.relPath, "uri", st.thumbs.URI(job.relPath))
		st.releaseThumb(job.node)
		return
	}

	if transientThumbErr(err) {
		slog.Debug("thumbnail failed, retrying later", "path", job.relPath, "err", err)
		st.retryThumbLater(job.node)
		return
	}
	slog.Warn("thumbnail failed", "path", job.relPath, "err", err)
}

// writeThumb writes img for job, then takes it back out when the file was deleted, renamed or
// moved while the job sat in the queue or was in flight. removeChild drops the listing entry before
// it calls forgetThumbs, so either stillListed sees the entry gone, or that forgetThumbs runs after
// this Write: no interleaving leaves a preview behind for a path the file no longer has.
func (st *mountState) writeThumb(job thumbJob, img []byte) error {
	if err := st.thumbs.Write(job.relPath, job.node.ModTime(), job.node.Size(), img); err != nil {
		return err
	}
	if !st.stillListed(job) {
		st.thumbs.Remove(job.relPath)
	}
	return nil
}

// stillListed reports whether job's file is still in its folder's cached listing under the name
// it was queued with.
func (st *mountState) stillListed(job thumbJob) bool {
	st.mu.Lock()
	d := st.dirs[job.parentID]
	st.mu.Unlock()
	if d == nil {
		return false
	}

	d.mu.Lock()
	children := d.children
	d.mu.Unlock()

	name := path.Base(job.relPath)
	for _, ch := range children {
		if ch.Link.LinkID == job.node.Link.LinkID {
			return ch.Name == name
		}
	}
	return false
}

// transientThumbErr reports whether a failed preview may work on a later try: a timeout or
// cancel, a network error, Proton rate-limiting, or a server-side 5xx.
func transientThumbErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, drive.ErrRateLimited) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	// proton.APIError's Error method has a value receiver, so it travels as either shape.
	var apiPtr *proton.APIError
	if errors.As(err, &apiPtr) {
		return apiPtr.Status >= 500
	}
	var apiVal proton.APIError
	return errors.As(err, &apiVal) && apiVal.Status >= 500
}

// thumbRetryAfter is how long a transiently failed revision stays marked before a listing may
// queue it again: long enough that an outage is not retried on every 30s listing refresh, short
// enough that it does not block that preview until restart. A var so tests can shorten it.
var thumbRetryAfter = 10 * time.Minute

// retryThumbLater unmarks a failed revision once thumbRetryAfter has passed.
func (st *mountState) retryThumbLater(n *drive.Node) {
	time.AfterFunc(thumbRetryAfter, func() { st.releaseThumb(n) })
}

// runExternalThumbWorker downloads files and runs external thumbnailers on them. One goroutine
// per mount; these are heavier than Proton thumbnail fetches (full file download + exec).
func (st *mountState) runExternalThumbWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-st.externalThumbJobs:
			st.generateExternalThumb(ctx, job)
		}
	}
}

func (st *mountState) generateExternalThumb(ctx context.Context, job externalThumbJob) {
	opCtx, cancel := context.WithTimeout(ctx, externalThumbTimeout)
	defer cancel()

	img, err := st.externalThumb(opCtx, job)
	st.finishThumb(job.thumbJob, img, err)
}

// externalThumb downloads the file to a temp copy and runs its external thumbnailer on it.
func (st *mountState) externalThumb(ctx context.Context, job externalThumbJob) ([]byte, error) {
	tmp, err := os.CreateTemp("", "proton-thumb-src-*"+job.ext)
	if err != nil {
		return nil, fmt.Errorf("external thumbnail temp file: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	file, err := st.client.OpenFile(ctx, job.node, job.relPath)
	if err != nil {
		return nil, fmt.Errorf("external thumbnail open: %w", err)
	}
	defer func() { _ = file.Close() }()

	buf := make([]byte, 256*1024)
	var off int64
	for {
		n, readErr := file.ReadAt(ctx, buf, off)
		if n > 0 {
			if _, werr := tmp.WriteAt(buf[:n], off); werr != nil {
				return nil, fmt.Errorf("external thumbnail temp write: %w", werr)
			}
			off += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("external thumbnail download: %w", readErr)
		}
	}
	_ = tmp.Close()

	// ponytail: 256px = large thumbnail size from the freedesktop spec
	return st.registry.Generate(ctx, job.ext, tmp.Name(), 256)
}

// queueThumbs fetches previews for the files in a fresh listing that are not already cached.
// It runs off the FUSE handler goroutine because the freshness check reads the cache from disk.
// ponytail: runs for every fetched listing, so a crawler (find, an indexer) previews whatever it
// walks. Limiting it to Readdir would not help: find reads folders through Readdir too, and the
// fetch is shared by Readdir and Lookup, so telling them apart means plumbing the caller through
// load and finishLoad.
func (st *mountState) queueThumbs(dirPath string, children []*drive.Node) {
	if st.thumbs == nil {
		return
	}

	for _, ch := range children {
		relPath := path.Join(dirPath, ch.Name)
		if st.thumbs.Fresh(relPath, ch.ModTime()) {
			continue
		}

		job := thumbJob{node: ch, relPath: relPath, parentID: st.oldParent(ch.Link.LinkID)}
		if ch.HasThumbnail() {
			if !st.claimThumb(ch) {
				continue
			}
			select {
			case st.thumbJobs <- job:
			default:
				st.releaseThumb(ch)
				slog.Debug("thumbnail queue full, skipping", "path", relPath)
			}
			continue
		}

		// No Proton thumbnail; try an external thumbnailer if registered.
		if st.registry == nil || st.externalThumbJobs == nil || ch.IsDir() {
			continue
		}
		ext := filepath.Ext(ch.Name)
		if st.registry.ForExt(ext) == "" {
			continue
		}
		// ponytail: skip large files, not worth downloading just for a thumbnail
		if limit := st.client.LargeFileLimit(); limit > 0 && ch.Size() > limit {
			continue
		}
		if !st.claimThumb(ch) {
			continue
		}
		select {
		case st.externalThumbJobs <- externalThumbJob{thumbJob: job, ext: ext}:
		default:
			st.releaseThumb(ch)
			slog.Debug("external thumbnail queue full, skipping", "path", relPath)
		}
	}
}

// forgetThumbs removes the cached thumbnails of a file this mount deleted or renamed away.
// ponytail: the path itself only. A trashed or moved folder leaves its descendants' thumbnails
// behind (finding them means listing the whole subtree), and so does a delete or rename made on
// another device, which reaches us as an event that expires listings, not through removeChild.
func (st *mountState) forgetThumbs(relPath string) {
	if st.thumbs == nil {
		return
	}
	st.thumbs.Remove(relPath)
}

// claimThumb reports whether this revision's thumbnail still needs fetching, marking it as
// taken if so. See finishThumb for how long a failed revision stays marked.
func (st *mountState) claimThumb(n *drive.Node) bool {
	key := thumbKey(n)

	st.mu.Lock()
	defer st.mu.Unlock()

	if st.thumbInflight[key] {
		return false
	}
	st.thumbInflight[key] = true
	return true
}

func (st *mountState) releaseThumb(n *drive.Node) {
	st.mu.Lock()
	delete(st.thumbInflight, thumbKey(n))
	st.mu.Unlock()
}

// deniedReader reports whether an open of a file this size comes from a thumbnailer or indexer
// on the denylist. Thumbnailer processes are always blocked (the daemon generates thumbnails
// itself); indexer processes are blocked only for files above the large-file threshold.
func (st *mountState) deniedReader(ctx context.Context, size int64) (procName string, pid uint32, denied bool) {
	sizeGated := false
	if len(st.denyReaders) > 0 {
		limit := st.client.LargeFileLimit()
		sizeGated = limit > 0 && size > limit
	}
	if !sizeGated && len(st.denyThumbnailers) == 0 {
		return "", 0, false
	}

	caller, ok := fuse.FromContext(ctx)
	if !ok {
		return "", 0, false
	}

	for _, name := range callerNames(caller.Pid) {
		for _, deny := range st.denyThumbnailers {
			if nameMatches(deny, name) {
				return name, caller.Pid, true
			}
		}
		if sizeGated {
			for _, deny := range st.denyReaders {
				if nameMatches(deny, name) {
					return name, caller.Pid, true
				}
			}
		}
	}

	return "", caller.Pid, false
}

// callerNames returns the names a pid is known by: /proc/<pid>/comm, and the basename of its
// executable when that link is readable (it is not, for a process owned by another user).
func callerNames(pid uint32) []string {
	procDir := filepath.Join("/proc", strconv.FormatUint(uint64(pid), 10))

	var names []string
	if comm, err := os.ReadFile(filepath.Join(procDir, "comm")); err == nil {
		if name := strings.TrimSpace(string(comm)); name != "" {
			names = append(names, name)
		}
	}
	if exe, err := os.Readlink(filepath.Join(procDir, "exe")); err == nil {
		names = append(names, filepath.Base(exe))
	}

	return names
}

// commMax is the length the kernel truncates /proc/<pid>/comm to, so a longer binary name such
// as gnome-desktop-thumbnailer arrives as "gnome-desktop-t" and only matches as a prefix.
const commMax = 15

func nameMatches(deny, name string) bool {
	if deny == name {
		return true
	}

	return len(name) == commMax && strings.HasPrefix(deny, name)
}
