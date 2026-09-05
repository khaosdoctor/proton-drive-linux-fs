// Package thumbs writes image previews into the freedesktop thumbnail cache, so file managers
// find a preview already there instead of running a thumbnailer that would download the file.
//
// Layout and metadata follow the freedesktop thumbnail managing standard: PNG files named
// md5(file URI) under normal/ (128px) and large/ (256px), carrying Thumb::URI and Thumb::MTime
// tEXt chunks that the file manager checks before trusting the preview.
package thumbs

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "golang.org/x/image/webp"
	_ "image/jpeg" // Proton stores thumbnails as JPEG; PNG is registered by the png import above.

	"golang.org/x/image/draw"
)

// Sizes of the two thumbnail directories the standard defines, as the longest edge in pixels.
const (
	normalSize = 128
	largeSize  = 256
)

// Store writes thumbnails for files under one mountpoint into one thumbnail cache directory.
type Store struct {
	dir        string // $XDG_CACHE_HOME/thumbnails or ~/.cache/thumbnails
	mountpoint string // absolute path the relative paths passed to Fresh and Write are joined onto
}

// New returns a Store writing into dir for files under mountpoint. mountpoint is made absolute
// because the thumbnail file name is the md5 of the file's absolute URI.
func New(dir, mountpoint string) (*Store, error) {
	abs, err := filepath.Abs(mountpoint)
	if err != nil {
		return nil, err
	}

	return &Store{dir: dir, mountpoint: abs}, nil
}

// URI returns the file:// URI of relPath under the mountpoint, percent-encoded the way GIO
// encodes it, which is what the thumbnail name is hashed from.
func (s *Store) URI(relPath string) string {
	u := url.URL{Scheme: "file", Path: filepath.Join(s.mountpoint, relPath)}
	return u.String()
}

// fileName returns the thumbnail file name for relPath: the hex md5 of its URI, plus ".png".
func (s *Store) fileName(relPath string) string {
	sum := md5.Sum([]byte(s.URI(relPath))) // #nosec G401 -- the standard mandates md5 for the name
	return hex.EncodeToString(sum[:]) + ".png"
}

// Fresh reports whether a large thumbnail for relPath already exists and records mtime, meaning
// there is nothing to fetch.
func (s *Store) Fresh(relPath string, mtime time.Time) bool {
	data, err := os.ReadFile(filepath.Join(s.dir, "large", s.fileName(relPath)))
	if err != nil {
		return false
	}

	recorded, ok := pngText(data, "Thumb::MTime")
	if !ok {
		return false
	}

	return recorded == strconv.FormatInt(mtime.Unix(), 10)
}

// Write decodes img (JPEG or PNG or WebP) and stores it as a normal and a large thumbnail for relPath,
// tagged with mtime and size so a file manager can tell when it goes stale. A size of 0 or less
// leaves Thumb::Size out.
func (s *Store) Write(relPath string, mtime time.Time, size int64, img []byte) error {
	src, _, err := image.Decode(bytes.NewReader(img))
	if err != nil {
		return fmt.Errorf("decode thumbnail (%d bytes, starts %x): %w", len(img), img[:min(16, len(img))], err)
	}

	text := [][2]string{
		{"Thumb::URI", s.URI(relPath)},
		{"Thumb::MTime", strconv.FormatInt(mtime.Unix(), 10)},
	}
	if size > 0 {
		text = append(text, [2]string{"Thumb::Size", strconv.FormatInt(size, 10)})
	}

	name := s.fileName(relPath)
	sizes := []struct {
		dir string
		max int
	}{{"normal", normalSize}, {"large", largeSize}}

	for _, sz := range sizes {
		encoded, err := encodePNG(fit(src, sz.max), text)
		if err != nil {
			return err
		}

		if err := writeAtomic(filepath.Join(s.dir, sz.dir), name, encoded); err != nil {
			return err
		}
	}

	return nil
}

// fit scales src down so neither edge exceeds max, preserving the aspect ratio. An image that
// already fits is returned untouched.
func fit(src image.Image, max int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || (w <= max && h <= max) {
		return src
	}

	if w >= h {
		h = h * max / w
		w = max
	} else {
		w = w * max / h
		h = max
	}
	w = atLeastOne(w)
	h = atLeastOne(h)

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}

func atLeastOne(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// encodePNG encodes img and inserts the given tEXt chunks before IEND.
func encodePNG(img image.Image, text [][2]string) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}

	return insertPNGText(buf.Bytes(), text)
}

// writeAtomic writes data to dir/name through a temp file and a rename, so a reader never sees
// a half-written thumbnail. dir is created 0700 and the file ends up 0600.
func writeAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, name+".tmp*")
	if err != nil {
		return err
	}
	path := tmp.Name()

	defer func() { _ = os.Remove(path) }() // no-op once the rename succeeded

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(path, filepath.Join(dir, name))
}

const pngSignatureLen = 8

// pngText returns the value of the first tEXt chunk with the given keyword.
func pngText(data []byte, keyword string) (string, bool) {
	off := pngSignatureLen
	for off+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[off : off+4]))
		typ := string(data[off+4 : off+8])
		start := off + 8
		if length < 0 || start+length > len(data) {
			return "", false
		}

		if typ == "tEXt" {
			key, value, found := bytes.Cut(data[start:start+length], []byte{0})
			if found && string(key) == keyword {
				return string(value), true
			}
		}

		off = start + length + 4 // skip the chunk data and its CRC
	}

	return "", false
}

// insertPNGText inserts one tEXt chunk per key/value pair immediately before the IEND chunk.
func insertPNGText(data []byte, text [][2]string) ([]byte, error) {
	iend := findIEND(data)
	if iend < 0 {
		return nil, errors.New("no IEND chunk in encoded PNG")
	}

	out := make([]byte, 0, len(data)+len(text)*64)
	out = append(out, data[:iend]...)
	for _, kv := range text {
		chunk, err := textChunk(kv[0], kv[1])
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
	}

	return append(out, data[iend:]...), nil
}

// findIEND returns the offset of the IEND chunk's length field, or -1 if there is none.
func findIEND(data []byte) int {
	off := pngSignatureLen
	for off+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[off : off+4]))
		if string(data[off+4:off+8]) == "IEND" {
			return off
		}
		if length < 0 || off+8+length+4 > len(data) {
			return -1
		}
		off += 8 + length + 4
	}

	return -1
}

// textChunk builds a PNG tEXt chunk: length, "tEXt", keyword, NUL, value, CRC over type+data.
func textChunk(keyword, value string) ([]byte, error) {
	if keyword == "" || len(keyword) > 79 {
		return nil, fmt.Errorf("tEXt keyword %q must be 1 to 79 bytes", keyword)
	}

	payload := make([]byte, 0, 4+len(keyword)+1+len(value))
	payload = append(payload, "tEXt"...)
	payload = append(payload, keyword...)
	payload = append(payload, 0)
	payload = append(payload, value...)

	chunk := make([]byte, 0, 4+len(payload)+4)
	chunk = binary.BigEndian.AppendUint32(chunk, uint32(len(payload)-4))
	chunk = append(chunk, payload...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(payload))

	return chunk, nil
}
