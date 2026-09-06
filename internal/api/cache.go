package api

import (
	"io/fs"
	"os"
	"path/filepath"
)

// dirStats walks dir and returns how many files it holds and their total size. A missing or
// unreadable dir counts as empty rather than an error: cache stats are best-effort.
func dirStats(dir string) (entries int, size int64) {
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		entries++
		size += info.Size()
		return nil
	})
	return entries, size
}

// ClearCacheDir removes everything under dir, keeping the directory itself, and returns the
// bytes freed. An empty dir (no cache configured) is a no-op.
func ClearCacheDir(dir string) (int64, error) {
	if dir == "" {
		return 0, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	var freed int64
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())

		size, err := pathSize(p)
		if err != nil {
			return freed, err
		}
		if err := os.RemoveAll(p); err != nil {
			return freed, err
		}
		freed += size
	}
	return freed, nil
}

// pathSize returns p's size: its own size if it's a file, or the total size of everything under
// it if it's a directory.
func pathSize(p string) (int64, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return info.Size(), nil
	}

	_, size := dirStats(p)
	return size, nil
}
