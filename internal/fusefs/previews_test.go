package fusefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net"
	"os/exec"
	"testing"
	"time"

	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/thumbs"
)

// A transient failure (outage, timeout) retries after thumbRetryAfter instead of blocking that
// preview until restart. A permanent one (bad image, thumbnailer exit) stays marked, so a folder
// of files whose thumbnailer fails does not re-download every file every retry period.
func TestFailedThumbRetriesOnlyTransient(t *testing.T) {
	prev := thumbRetryAfter
	thumbRetryAfter = 20 * time.Millisecond
	t.Cleanup(func() { thumbRetryAfter = prev })

	st := &mountState{thumbInflight: make(map[string]bool)}
	transient := node("l1", "a.jpg")
	permanent := node("l2", "b.jpg")

	for _, n := range []*drive.Node{transient, permanent} {
		if !st.claimThumb(n) {
			t.Fatal("first claim refused")
		}
	}
	st.finishThumb(thumbJob{node: transient, relPath: "a.jpg"}, nil, fmt.Errorf("get block: %w", context.DeadlineExceeded))
	st.finishThumb(thumbJob{node: permanent, relPath: "b.jpg"}, nil, errors.New("decode thumbnail: invalid JPEG"))

	if st.claimThumb(transient) {
		t.Fatal("failed revision claimable again right away; it should wait out thumbRetryAfter")
	}

	deadline := time.Now().Add(2 * time.Second)
	for !st.claimThumb(transient) {
		if time.Now().After(deadline) {
			t.Fatal("transiently failed revision still claimed long after thumbRetryAfter")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if st.claimThumb(permanent) {
		t.Fatal("permanently failed revision claimable again; it should stay marked for the mount")
	}
}

// A fetch still in flight when its file is deleted or renamed must not leave a preview behind for
// the old path: forgetThumbs already ran, so nothing else would remove it.
func TestWriteThumbDropsPreviewOfRemovedFile(t *testing.T) {
	store, err := thumbs.New(t.TempDir(), "/mnt/proton")
	if err != nil {
		t.Fatalf("thumbs.New: %v", err)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatalf("encoding jpeg: %v", err)
	}

	st := &mountState{thumbs: store, dirs: make(map[string]*dirNode), parentOf: make(map[string]string)}
	file := node("l1", "a.jpg")
	dir := &dirNode{node: node("p", "pics"), st: st, children: []*drive.Node{file}}
	st.dirs["p"] = dir
	job := thumbJob{node: file, relPath: "pics/a.jpg", parentID: "p"}

	if err := st.writeThumb(job, buf.Bytes()); err != nil {
		t.Fatalf("writeThumb: %v", err)
	}
	if !store.Fresh(job.relPath, file.ModTime()) {
		t.Fatal("no preview written for a file that is still listed")
	}
	store.Remove(job.relPath)

	// removeChild ran (and called forgetThumbs) while the job was in flight.
	dir.removeChild("a.jpg")
	if err := st.writeThumb(job, buf.Bytes()); err != nil {
		t.Fatalf("writeThumb: %v", err)
	}
	if store.Fresh(job.relPath, file.ModTime()) {
		t.Fatal("preview left behind for a file removed while its fetch was in flight")
	}

	// Renamed in place: same link, new name.
	dir.upsertChild(node("l1", "b.jpg"))
	if err := st.writeThumb(job, buf.Bytes()); err != nil {
		t.Fatalf("writeThumb: %v", err)
	}
	if store.Fresh(job.relPath, file.ModTime()) {
		t.Fatal("preview left behind under the old name of a renamed file")
	}
}

func TestTransientThumbErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"deadline", fmt.Errorf("get: %w", context.DeadlineExceeded), true},
		{"canceled", context.Canceled, true},
		{"rate limited", fmt.Errorf("thumbnail: %w", drive.ErrRateLimited), true},
		{"network", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"5xx pointer", &proton.APIError{Status: 503}, true},
		{"5xx value", fmt.Errorf("get: %w", proton.APIError{Status: 502}), true},
		{"4xx", &proton.APIError{Status: 404}, false},
		{"decode", errors.New("decode thumbnail: unknown format"), false},
		{"thumbnailer exit", fmt.Errorf("thumbnailer x: %w", &exec.ExitError{}), false},
	}

	for _, tc := range tests {
		if got := transientThumbErr(tc.err); got != tc.want {
			t.Errorf("%s: transientThumbErr = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNameMatches(t *testing.T) {
	tests := []struct {
		deny string
		name string
		want bool
	}{
		{"tumblerd", "tumblerd", true},
		{"tumblerd", "bambu-studio", false},
		// /proc/<pid>/comm is truncated to 15 characters, so a longer binary name arrives short.
		{"gnome-desktop-thumbnailer", "gnome-desktop-t", true},
		{"totem-video-thumbnailer", "totem-video-thu", true},
		// A short name must match in full: "tracker" is not "tracker-miner-fs".
		{"tracker-miner-fs", "tracker", false},
		{"baloo_file_extractor", "baloo_file", false},
	}

	for _, tc := range tests {
		if got := nameMatches(tc.deny, tc.name); got != tc.want {
			t.Errorf("nameMatches(%q, %q) = %v, want %v", tc.deny, tc.name, got, tc.want)
		}
	}
}
