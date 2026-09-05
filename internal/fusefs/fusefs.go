package fusefs

import (
	"context"
	"hash/fnv"
	"io"
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

	st := newMountState(c)

	rootNode := &dirNode{client: c, node: root, ttl: opts.TTL, st: st}
	st.register(rootNode)

	ttl := opts.TTL
	negTTL := time.Second

	server, err := fs.Mount(mountpoint, rootNode, &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:        "proton-drive-fs",
			Name:          "proton-drive-fs",
			Options:       []string{"ro"},
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

	go func() {
		<-ctx.Done()
		_ = server.Unmount()
	}()

	server.Wait()
	return nil
}

// mountState tracks every registered directory node and each node's last-known parent, so a
// remote event naming a LinkID can find which cached listings to drop.
type mountState struct {
	client *drive.Client

	mu       sync.Mutex
	dirs     map[string]*dirNode // folder LinkID -> its inode
	parentOf map[string]string   // child LinkID -> parent LinkID
}

func newMountState(c *drive.Client) *mountState {
	return &mountState{client: c, dirs: make(map[string]*dirNode), parentOf: make(map[string]string)}
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
	var link proton.Link
	found := false
	for _, ch := range parent.children {
		if ch.Link.LinkID == ev.LinkID {
			link = ch.Link
			found = true
			break
		}
	}
	parentNode := parent.node
	parent.mu.Unlock()
	if !found {
		return
	}

	name, err := st.client.DecryptName(link, parentNode)
	if err != nil {
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
var _ = (fs.NodeLookuper)((*dirNode)(nil))
var _ = (fs.NodeReaddirer)((*dirNode)(nil))

func (d *dirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = d.Mode() | 0o555
	out.Ino = d.StableAttr().Ino
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

func (d *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	children, errno := d.load(ctx)
	if errno != 0 {
		return nil, errno
	}

	for _, child := range children {
		if child.Name != name {
			continue
		}

		fillEntryOut(out, child)

		if existing := d.GetChild(name); existing != nil {
			refreshNode(existing, child)
			return existing, 0
		}

		return d.makeChild(ctx, child, out), 0
	}

	return nil, syscall.ENOENT
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
	fillEntryOut(out, child)
	attr := fs.StableAttr{Ino: out.Ino}

	if child.IsDir() {
		attr.Mode = fuse.S_IFDIR
		dn := &dirNode{client: d.client, node: child, ttl: d.ttl, st: d.st}
		inode := d.NewInode(ctx, dn, attr)
		d.st.register(dn)
		return inode
	}

	attr.Mode = fuse.S_IFREG
	return d.NewInode(ctx, &fileNode{client: d.client, node: child}, attr)
}

// fillEntryOut populates a Lookup/makeChild EntryOut from child's current attributes.
func fillEntryOut(out *fuse.EntryOut, child *drive.Node) {
	out.Ino = inodeNum(child.Link.LinkID)

	if child.IsDir() {
		out.Mode = fuse.S_IFDIR | 0o555
		return
	}

	out.Mode = fuse.S_IFREG | 0o444
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

// fileNode is a regular, read-only file. Content is fetched lazily through drive.File on Open/Read.
type fileNode struct {
	fs.Inode

	client *drive.Client

	mu   sync.Mutex
	node *drive.Node
}

var _ = (fs.NodeGetattrer)((*fileNode)(nil))
var _ = (fs.NodeOpener)((*fileNode)(nil))

// setNode replaces the node backing this file, e.g. after a remote listing refresh.
func (f *fileNode) setNode(n *drive.Node) {
	f.mu.Lock()
	f.node = n
	f.mu.Unlock()
}

func (f *fileNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	f.mu.Lock()
	node := f.node
	f.mu.Unlock()

	out.Mode = f.Mode() | 0o444
	out.Ino = f.StableAttr().Ino
	out.Size = uint64(node.Size)
	out.Mtime = uint64(node.ModTime.Unix())
	return 0
}

func (f *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}

	f.mu.Lock()
	node := f.node
	f.mu.Unlock()

	file, err := f.client.OpenFile(ctx, node)
	if err != nil {
		return nil, 0, syscall.EIO
	}

	return &fileHandle{file: file}, fuse.FOPEN_KEEP_CACHE, 0
}

type fileHandle struct {
	file *drive.File
}

var _ = (fs.FileReader)((*fileHandle)(nil))

func (h *fileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := h.file.ReadAt(ctx, dest, off)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}

	return fuse.ReadResultData(dest[:n]), 0
}
