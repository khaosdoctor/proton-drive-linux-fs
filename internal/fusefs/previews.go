package fusefs

import (
	"context"
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
)

// thumbQueueSize bounds the pending thumbnail fetches. Listing a huge folder queues what fits
// and drops the rest; the next listing after the TTL picks them up.
const thumbQueueSize = 256

// thumbJob is one file whose Proton thumbnail should be fetched and cached.
type thumbJob struct {
	node    *drive.Node
	relPath string
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

	if err := st.thumbs.Write(job.relPath, job.node.ModTime, job.node.Size, img); err != nil {
		st.thumbFailed(job, err)
		return
	}

	st.mu.Lock()
	delete(st.thumbInflight, thumbKey(job.node))
	st.mu.Unlock()
}

// thumbFailed logs the failure and leaves the revision marked, so a file Proton cannot give us a
// preview for is not retried (and re-logged) on every listing refresh.
func (st *mountState) thumbFailed(job thumbJob, err error) {
	log.Printf("fusefs: thumbnail %q: %v", job.relPath, err)
}

// queueThumbs fetches previews for the files in a fresh listing that are not already cached.
// It runs off the FUSE handler goroutine because the freshness check reads the cache from disk.
func (st *mountState) queueThumbs(dirPath string, children []*drive.Node) {
	if st.thumbs == nil {
		return
	}

	for _, ch := range children {
		if !ch.HasThumbnail() {
			continue
		}

		relPath := path.Join(dirPath, ch.Name)
		if st.thumbs.Fresh(relPath, ch.ModTime) {
			continue
		}

		if !st.claimThumb(ch) {
			continue
		}

		select {
		case st.thumbJobs <- thumbJob{node: ch, relPath: relPath}:
		default:
			st.releaseThumb(ch)
			if st.debug {
				log.Printf("fusefs: thumbnail queue full, skipping %q", relPath)
			}
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
// on the denylist. Those walk every entry in a folder, and letting them read a large file turns
// browsing into a download; the user's own applications are not on the list.
func (st *mountState) deniedReader(ctx context.Context, size int64) (procName string, pid uint32, denied bool) {
	limit := st.client.LargeFileLimit()
	if limit <= 0 || size <= limit || len(st.denyReaders) == 0 {
		return "", 0, false
	}

	caller, ok := fuse.FromContext(ctx)
	if !ok {
		return "", 0, false
	}

	for _, name := range callerNames(caller.Pid) {
		for _, deny := range st.denyReaders {
			if nameMatches(deny, name) {
				return name, caller.Pid, true
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
