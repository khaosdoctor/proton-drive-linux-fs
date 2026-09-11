package fusefs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

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

// thumbJob is one file whose Proton thumbnail should be fetched and cached.
type thumbJob struct {
	node    *drive.Node
	relPath string
}

// externalThumbJob is a file that needs an external thumbnailer run on a downloaded copy.
type externalThumbJob struct {
	node    *drive.Node
	relPath string
	ext     string // file extension including dot
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
	img, err := st.client.Thumbnail(ctx, job.node)
	if err != nil {
		st.thumbFailed(job, err)
		return
	}

	if err := st.thumbs.Write(job.relPath, job.node.ModTime(), job.node.Size(), img); err != nil {
		st.thumbFailed(job, err)
		return
	}
	slog.Debug("thumbnail written", "path", job.relPath, "uri", st.thumbs.URI(job.relPath))

	st.mu.Lock()
	delete(st.thumbInflight, thumbKey(job.node))
	st.mu.Unlock()
}

// thumbFailed logs the failure and leaves the revision marked, so a file Proton cannot give us a
// preview for is not retried (and re-logged) on every listing refresh.
func (st *mountState) thumbFailed(job thumbJob, err error) {
	slog.Warn("thumbnail failed", "path", job.relPath, "err", err)
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

	tmp, err := os.CreateTemp("", "proton-thumb-src-*"+job.ext)
	if err != nil {
		slog.Warn("external thumbnail: temp file failed", "path", job.relPath, "err", err)
		return
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	file, err := st.client.OpenFile(opCtx, job.node, job.relPath)
	if err != nil {
		slog.Debug("external thumbnail: open failed", "path", job.relPath, "err", err)
		return
	}

	buf := make([]byte, 256*1024)
	var off int64
	for {
		n, readErr := file.ReadAt(opCtx, buf, off)
		if n > 0 {
			if _, werr := tmp.WriteAt(buf[:n], off); werr != nil {
				_ = file.Close()
				slog.Warn("external thumbnail: write failed", "path", job.relPath, "err", werr)
				return
			}
			off += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = file.Close()
			slog.Debug("external thumbnail: download failed", "path", job.relPath, "err", readErr)
			return
		}
	}
	_ = file.Close()
	_ = tmp.Close()

	// ponytail: 256px = large thumbnail size from the freedesktop spec
	img, err := st.registry.Generate(opCtx, job.ext, tmp.Name(), 256)
	if err != nil {
		slog.Debug("external thumbnail: generation failed", "path", job.relPath, "err", err)
		return
	}

	if err := st.thumbs.Write(job.relPath, job.node.ModTime(), job.node.Size(), img); err != nil {
		slog.Warn("external thumbnail: cache write failed", "path", job.relPath, "err", err)
		return
	}
	slog.Debug("external thumbnail written", "path", job.relPath)

	st.mu.Lock()
	delete(st.thumbInflight, thumbKey(job.node))
	st.mu.Unlock()
}

// queueThumbs fetches previews for the files in a fresh listing that are not already cached.
// It runs off the FUSE handler goroutine because the freshness check reads the cache from disk.
func (st *mountState) queueThumbs(dirPath string, children []*drive.Node) {
	if st.thumbs == nil {
		return
	}

	for _, ch := range children {
		relPath := path.Join(dirPath, ch.Name)
		if st.thumbs.Fresh(relPath, ch.ModTime()) {
			continue
		}

		if ch.HasThumbnail() {
			if !st.claimThumb(ch) {
				continue
			}
			select {
			case st.thumbJobs <- thumbJob{node: ch, relPath: relPath}:
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
		case st.externalThumbJobs <- externalThumbJob{node: ch, relPath: relPath, ext: ext}:
		default:
			st.releaseThumb(ch)
			slog.Debug("external thumbnail queue full, skipping", "path", relPath)
		}
	}
}

// claimThumb reports whether this revision's thumbnail still needs fetching, marking it as
// taken if so. A revision stays marked once it fails, so a broken preview is tried once.
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
