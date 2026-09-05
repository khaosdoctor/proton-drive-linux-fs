package thumbs

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// sampleJPEG returns a w by h JPEG, standing in for the bytes Proton hands back.
func sampleJPEG(t *testing.T, w, h int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 0xff})
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encoding sample jpeg: %v", err)
	}
	return buf.Bytes()
}

func newStore(t *testing.T) *Store {
	t.Helper()

	s, err := New(t.TempDir(), "/mnt/proton drive")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestURIAndFileName(t *testing.T) {
	s := newStore(t)

	const wantURI = "file:///mnt/proton%20drive/pics/a%20b.jpg"
	if got := s.URI("pics/a b.jpg"); got != wantURI {
		t.Fatalf("URI = %q, want %q", got, wantURI)
	}

	sum := md5.Sum([]byte(wantURI))
	want := hex.EncodeToString(sum[:]) + ".png"
	if got := s.fileName("pics/a b.jpg"); got != want {
		t.Fatalf("fileName = %q, want %q", got, want)
	}
}

func TestTextChunkRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatalf("encoding png: %v", err)
	}

	out, err := insertPNGText(buf.Bytes(), [][2]string{
		{"Thumb::URI", "file:///mnt/x.jpg"},
		{"Thumb::MTime", "1700000000"},
	})
	if err != nil {
		t.Fatalf("insertPNGText: %v", err)
	}

	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("png with tEXt chunks no longer decodes: %v", err)
	}

	for _, tc := range []struct{ key, want string }{
		{"Thumb::URI", "file:///mnt/x.jpg"},
		{"Thumb::MTime", "1700000000"},
	} {
		got, ok := pngText(out, tc.key)
		if !ok {
			t.Fatalf("%s missing", tc.key)
		}
		if got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}

	if _, ok := pngText(out, "Thumb::Size"); ok {
		t.Fatal("Thumb::Size present but never written")
	}
}

func TestWriteProducesBothSizes(t *testing.T) {
	s := newStore(t)
	mtime := time.Unix(1700000000, 0)

	if err := s.Write("pics/a b.jpg", mtime, 4096, sampleJPEG(t, 640, 400)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	name := s.fileName("pics/a b.jpg")
	for dir, max := range map[string]int{"normal": normalSize, "large": largeSize} {
		path := filepath.Join(s.dir, dir, name)

		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", path, info.Mode().Perm())
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if cfg.Width != max {
			t.Fatalf("%s width = %d, want %d (landscape source scaled to the long edge)", path, cfg.Width, max)
		}

		if got, _ := pngText(data, "Thumb::Size"); got != "4096" {
			t.Fatalf("%s Thumb::Size = %q, want 4096", path, got)
		}
		if got, _ := pngText(data, "Thumb::URI"); got != s.URI("pics/a b.jpg") {
			t.Fatalf("%s Thumb::URI = %q, want %q", path, got, s.URI("pics/a b.jpg"))
		}
	}
}

func TestFresh(t *testing.T) {
	s := newStore(t)
	mtime := time.Unix(1700000000, 0)

	if s.Fresh("a.jpg", mtime) {
		t.Fatal("Fresh true before anything was written")
	}

	if err := s.Write("a.jpg", mtime, 0, sampleJPEG(t, 64, 64)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if !s.Fresh("a.jpg", mtime) {
		t.Fatal("Fresh false right after Write")
	}
	if s.Fresh("a.jpg", mtime.Add(time.Second)) {
		t.Fatal("Fresh true for a changed mtime")
	}
	if s.Fresh("b.jpg", mtime) {
		t.Fatal("Fresh true for a file with no thumbnail")
	}
}

func TestFitLeavesSmallImagesAlone(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 40, 20))
	if got := fit(src, largeSize); got != image.Image(src) {
		t.Fatal("fit rescaled an image that already fits")
	}

	got := fit(image.NewRGBA(image.Rect(0, 0, 100, 1000)), 250)
	if b := got.Bounds(); b.Dx() != 25 || b.Dy() != 250 {
		t.Fatalf("fit portrait = %dx%d, want 25x250", b.Dx(), b.Dy())
	}
}
