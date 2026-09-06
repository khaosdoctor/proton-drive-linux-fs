package fusefs

import (
	"context"
	"errors"
	"net/http"
	"syscall"
	"testing"
	"time"

	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
)

func TestUploadTimeout(t *testing.T) {
	base := 60 * time.Second

	cases := []struct {
		name string
		size int64
		want time.Duration
	}{
		{"empty file uses base", 0, base},
		{"small file uses base", 1 << 20, base}, // 1MiB well under base's allowance
		{"large file scales past base", 400 * (256 << 10), 400 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := uploadTimeout(tc.size, base)
			if got != tc.want {
				t.Errorf("uploadTimeout(%d, %s) = %s, want %s", tc.size, base, got, tc.want)
			}
		})
	}
}

// TestOpenErrnoNotFoundExpiresParent checks a 404 from Proton (the file's link or revision is
// gone) maps to ENOENT and expires the parent directory's cached listing, so a ghost entry served
// from a stale in-memory or disk-persisted listing self-heals instead of failing every open the
// same way until the next TTL refetch.
func TestOpenErrnoNotFoundExpiresParent(t *testing.T) {
	d := &dirNode{
		ttl:      time.Minute,
		children: []*drive.Node{node("l1", "a")},
		expires:  time.Now().Add(time.Minute),
	}

	notFound := errors.Join(errors.New("404 GET /x"), &proton.APIError{Status: http.StatusNotFound})

	errno := openErrno(notFound, d, context.Background(), "opening file", "/some/path")

	if errno != syscall.ENOENT {
		t.Fatalf("errno = %v, want ENOENT", errno)
	}
	if !d.expires.IsZero() {
		t.Error("expected the parent's cached listing to be expired")
	}
}

func TestOpenErrnoOtherErrorReturnsEIO(t *testing.T) {
	errno := openErrno(errors.New("boom"), nil, context.Background(), "opening file", "/p")
	if errno != syscall.EIO {
		t.Fatalf("errno = %v, want EIO", errno)
	}
}

func TestOpenErrnoTimeoutReturnsETIMEDOUT(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()

	errno := openErrno(errors.New("network gone"), nil, ctx, "opening file", "/p")
	if errno != syscall.ETIMEDOUT {
		t.Fatalf("errno = %v, want ETIMEDOUT", errno)
	}
}
