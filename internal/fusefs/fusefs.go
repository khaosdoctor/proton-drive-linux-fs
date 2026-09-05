package fusefs

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
)

// Options configures the mount.
type Options struct {
	Debug        bool
	TTL          time.Duration
	PollInterval time.Duration
}

// Mount publishes c's drive tree, rooted at root, at mountpoint and blocks until it is unmounted
// or ctx is cancelled.
func Mount(ctx context.Context, mountpoint string, c *drive.Client, root *drive.Node, opts Options) error {
	if opts.TTL <= 0 {
		opts.TTL = 30 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 10 * time.Second
	}

	// FUSE's kernel-side permission check compares these against the caller; 0:0 made every write ACCESS fail.
	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	st := newMountState(c, uid, gid)

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

	go c.Events(ctx, opts.PollInterval, st.handle)

	// A plain Unmount() fails while the mountpoint is busy (e.g. a shell still cd'd into it),
	// and server.Wait() would then block forever waiting for a detach that never happens; fall
	// back to a lazy unmount so Ctrl-C always returns, and let the kernel finish detaching once
	// the last reference drops.
	unmountDone := make(chan struct{})
	go func() {
		defer close(unmountDone)

		<-ctx.Done()

		if err := server.Unmount(); err != nil {
			log.Printf("fusefs: unmount %s: %v; retrying lazily", mountpoint, err)

			bin := fusermountBinary()
			out, lazyErr := exec.Command(bin, "-u", "-z", mountpoint).CombinedOutput()
			if lazyErr != nil {
				log.Printf("fusefs: lazy unmount %s: %v: %s", mountpoint, lazyErr, out)
				log.Printf("fusefs: could not unmount %s; run: fusermount3 -uz %s", mountpoint, mountpoint)
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
	client *drive.Client
	uid    uint32
	gid    uint32

	mu       sync.Mutex
	dirs     map[string]*dirNode // folder LinkID -> its inode
	parentOf map[string]string   // child LinkID -> parent LinkID
}

func newMountState(c *drive.Client, uid, gid uint32) *mountState {
	return &mountState{client: c, uid: uid, gid: gid, dirs: make(map[string]*dirNode), parentOf: make(map[string]string)}
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
	_ = child.NotifyContent(0, -1)
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

	mu       sync.Mutex
	node     *drive.Node
	children []*drive.Node
	expires  time.Time
}

// setNode replaces the node backing this directory, e.g. after a remote listing refresh.
func (d *dirNode) setNode(n *drive.Node) {
	d.mu.Lock()
	d.node = n
	d.mu.Unlock()
}

// invalidate expires the cached listing and drops the kernel dentry for every previously
// listed child, forcing a re-Lookup (and re-Getattr) on next access.
func (d *dirNode) invalidate() {
	d.mu.Lock()
	d.expires = time.Time{}
	names := make([]string, 0, len(d.children))
	for _, ch := range d.children {
		names = append(names, ch.Name)
	}
	d.mu.Unlock()

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

// load returns the cached children, refetching them once ttl has elapsed.
func (d *dirNode) load(ctx context.Context) ([]*drive.Node, syscall.Errno) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.children != nil && time.Now().Before(d.expires) {
		return d.children, 0
	}

	children, err := d.client.Children(ctx, d.node)
	if err != nil {
		log.Printf("fusefs: readdir %q: %v", d.node.Name, err)
		return nil, syscall.EIO
	}

	d.children = children
	d.expires = time.Now().Add(d.ttl)

	parentID := d.node.Link.LinkID
	for _, ch := range children {
		d.st.setParent(ch.Link.LinkID, parentID)
	}

	return children, 0
}

// refresh drops the cached listing and forces an immediate refetch, so a just-made local change
// (create, delete, move) is visible to the next Lookup/Readdir without waiting for ttl.
func (d *dirNode) refresh(ctx context.Context) ([]*drive.Node, syscall.Errno) {
	d.invalidate()
	return d.load(ctx)
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

// Mkdir creates a folder on the drive, then reuses Lookup to mount and return its inode.
func (d *dirNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if err := d.client.CreateDir(ctx, d.node, name); err != nil {
		log.Printf("fusefs: creating dir %q: %v", name, err)
		return nil, syscall.EIO
	}

	if _, errno := d.refresh(ctx); errno != 0 {
		return nil, errno
	}

	return d.Lookup(ctx, name, out)
}

// Create makes a temp file to buffer the new file's content and mounts a pending fileNode
// (node == nil) for it; the file isn't created on the drive until Release uploads it.
// ponytail: whole-file temp buffer; block-level partial writes later if needed
func (d *dirNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	tmp, err := os.CreateTemp("", "proton-drive-fs-*")
	if err != nil {
		log.Printf("fusefs: creating temp file for %q: %v", name, err)
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

	if err := d.client.Trash(ctx, d.node, target); err != nil {
		log.Printf("fusefs: trashing %q: %v", name, err)
		return syscall.EIO
	}

	_, errno = d.refresh(ctx)
	return errno
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

	if err := d.client.Move(ctx, target, d.node, newDir.node, newName); err != nil {
		log.Printf("fusefs: moving %q to %q: %v", name, newName, err)
		return syscall.EIO
	}

	if child := d.GetChild(name); child != nil {
		if fn, ok := child.Operations().(*fileNode); ok {
			fn.mu.Lock()
			fn.name = newName
			fn.parent = newDir
			fn.mu.Unlock()
		}
	}

	if _, errno := d.refresh(ctx); errno != 0 {
		return errno
	}
	if newDir == d {
		return 0
	}

	_, errno = newDir.refresh(ctx)
	return errno
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
		return
	}

	out.Mode = fuse.S_IFREG | 0o644
	out.Size = uint64(child.Size)
	out.Mtime = uint64(child.ModTime.Unix())
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
				out.Mtime = uint64(node.ModTime.Unix())
			}
			return 0
		}
	}

	if node == nil {
		return 0
	}

	out.Size = uint64(node.Size)
	out.Mtime = uint64(node.ModTime.Unix())
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
		log.Printf("fusefs: truncate on %q without an open write handle", f.currentName())
		return syscall.EIO
	}

	if err := handle.truncate(int64(size)); err != nil {
		log.Printf("fusefs: truncating %q: %v", f.currentName(), err)
		return syscall.EIO
	}

	return f.Getattr(ctx, fh, out)
}

func (f *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	f.mu.Lock()
	node := f.node
	f.mu.Unlock()

	if node == nil {
		// A pending (not yet uploaded) file only has the handle Create already returned.
		return nil, 0, syscall.EIO
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

		file, err := f.client.OpenFile(ctx, node)
		if err != nil {
			log.Printf("fusefs: opening %q: %v", f.currentName(), err)
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
		log.Printf("fusefs: creating temp file for %q: %v", f.currentName(), err)
		return nil, 0, syscall.EIO
	}

	if flags&syscall.O_TRUNC == 0 {
		if err := downloadInto(ctx, f.client, node, tmp); err != nil {
			log.Printf("fusefs: downloading %q: %v", f.currentName(), err)
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
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
			log.Printf("fusefs: read %q at offset %d, len %d: %v", h.node.currentName(), off, len(dest), err)
			return nil, syscall.EIO
		}
		return fuse.ReadResultData(dest[:n]), 0
	}

	n, err := h.file.ReadAt(ctx, dest, off)
	if err != nil && err != io.EOF {
		log.Printf("fusefs: read %q at offset %d, len %d: %v", h.node.currentName(), off, len(dest), err)
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
		log.Printf("fusefs: writing temp buffer for %q: %v", h.node.currentName(), err)
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

// Release uploads a dirty temp buffer as a new file or a new revision, then refreshes the
// parent listing and this node so subsequent Getattr/Open see the uploaded revision. The temp
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
		log.Printf("fusefs: stat temp file for %q: %v", name, err)
		return syscall.EIO
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		log.Printf("fusefs: seek temp file for %q: %v", name, err)
		return syscall.EIO
	}

	if err := fn.client.Upload(ctx, parent.node, name, existing, tmp, info.Size(), time.Now()); err != nil {
		log.Printf("fusefs: uploading %q: %v", name, err)
		return syscall.EIO
	}

	// Re-read parent/name: a concurrent Rename during the upload may have moved this file, and
	// the post-upload refresh/lookup must target where it is now, not where it was.
	fn.mu.Lock()
	refreshName := fn.name
	refreshParent := fn.parent
	fn.mu.Unlock()

	children, errno := refreshParent.refresh(ctx)
	if errno != 0 {
		log.Printf("fusefs: reloading %q after upload", refreshName)
		return errno
	}

	for _, child := range children {
		if child.Name == refreshName {
			fn.setNode(child)
			break
		}
	}

	return 0
}
