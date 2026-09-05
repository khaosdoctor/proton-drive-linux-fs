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

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
)

// Options configures the mount.
type Options struct {
	Debug bool
	TTL   time.Duration
}

// Mount publishes c's drive tree, rooted at root, at mountpoint and blocks until it is unmounted
// or ctx is cancelled.
func Mount(ctx context.Context, mountpoint string, c *drive.Client, root *drive.Node, opts Options) error {
	if opts.TTL <= 0 {
		opts.TTL = 30 * time.Second
	}

	rootNode := &dirNode{client: c, node: root, ttl: opts.TTL}
	ttl := opts.TTL

	server, err := fs.Mount(mountpoint, rootNode, &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:        "proton-drive-fs",
			Name:          "proton-drive-fs",
			Options:       []string{"ro"},
			DisableXAttrs: true,
			Debug:         opts.Debug,
		},
		EntryTimeout: &ttl,
		AttrTimeout:  &ttl,
	})
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		_ = server.Unmount()
	}()

	server.Wait()
	return nil
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
	node   *drive.Node
	ttl    time.Duration

	mu       sync.Mutex
	children []*drive.Node
	expires  time.Time
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

	return children, 0
}

func (d *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	children, errno := d.load(ctx)
	if errno != 0 {
		return nil, errno
	}

	for _, child := range children {
		if child.Name == name {
			return d.makeChild(ctx, child, out), 0
		}
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
	attr := fs.StableAttr{Ino: inodeNum(child.Link.LinkID)}

	if child.IsDir() {
		attr.Mode = fuse.S_IFDIR
		out.Mode = fuse.S_IFDIR | 0o555
		out.Ino = attr.Ino
		return d.NewInode(ctx, &dirNode{client: d.client, node: child, ttl: d.ttl}, attr)
	}

	attr.Mode = fuse.S_IFREG
	out.Mode = fuse.S_IFREG | 0o444
	out.Size = uint64(child.Size)
	out.Mtime = uint64(child.ModTime.Unix())
	out.Ino = attr.Ino

	return d.NewInode(ctx, &fileNode{client: d.client, node: child}, attr)
}

// fileNode is a regular, read-only file. Content is fetched lazily through drive.File on Open/Read.
type fileNode struct {
	fs.Inode

	client *drive.Client
	node   *drive.Node
}

var _ = (fs.NodeGetattrer)((*fileNode)(nil))
var _ = (fs.NodeOpener)((*fileNode)(nil))

func (f *fileNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = f.Mode() | 0o444
	out.Ino = f.StableAttr().Ino
	out.Size = uint64(f.node.Size)
	out.Mtime = uint64(f.node.ModTime.Unix())
	return 0
}

func (f *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}

	file, err := f.client.OpenFile(ctx, f.node)
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
