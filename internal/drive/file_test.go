package drive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
	proton "github.com/henrybear327/go-proton-api"
)

// TestGetBlockSingleflight checks that two concurrent misses on the same block only download it
// once: the second caller waits for the first instead of racing it, so the fetch and the
// transfer's byte count both happen exactly once.
func TestGetBlockSingleflight(t *testing.T) {
	sk, err := crypto.GenerateSessionKey()
	if err != nil {
		t.Fatal(err)
	}

	plain := []byte("hello block")
	ciphertext, err := sk.Encrypt(crypto.NewPlainMessage(plain))
	if err != nil {
		t.Fatal(err)
	}

	f := &File{
		client:     &Client{},
		sessionKey: sk,
		blocks:     map[int]proton.Block{1: {Index: 1, BareURL: "http://example.invalid", Token: "tok"}},
		size:       int64(len(plain)),
		cache:      make(map[int][]byte),
	}

	var calls atomic.Int64
	orig := fetchBlock
	fetchBlock = func(_ context.Context, _ *Client, _, _ string) ([]byte, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond) // give the other goroutine a chance to race in
		return ciphertext, nil
	}
	defer func() { fetchBlock = orig }()

	var wg sync.WaitGroup
	results := make([][]byte, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = f.getBlock(context.Background(), 1)
		}(i)
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("fetchBlock called %d times, want 1", got)
	}
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if string(results[i]) != string(plain) {
			t.Fatalf("goroutine %d: got %q, want %q", i, results[i], plain)
		}
	}
	if got := f.BytesTransferred(); got != int64(len(plain)) {
		t.Fatalf("BytesTransferred() = %d, want %d (only the winner should Add)", got, len(plain))
	}
}

func TestIsNotFound(t *testing.T) {
	notFound := errors.Join(errors.New("404 GET /x"), &proton.APIError{Status: http.StatusNotFound, Message: "not found"})
	if !IsNotFound(notFound) {
		t.Error("expected a wrapped 404 APIError to be reported as not found")
	}

	forbidden := errors.Join(errors.New("403 GET /x"), &proton.APIError{Status: http.StatusForbidden, Message: "forbidden"})
	if IsNotFound(forbidden) {
		t.Error("expected a 403 APIError to not be reported as not found")
	}

	if IsNotFound(errors.New("boom")) {
		t.Error("expected a plain error to not be reported as not found")
	}
}

func TestBlockIndexForOffset(t *testing.T) {
	cases := []struct {
		name string
		off  int64
		want int
	}{
		{"start of file", 0, 1},
		{"middle of first block", blockSize / 2, 1},
		{"last byte of first block", blockSize - 1, 1},
		{"first byte of second block", blockSize, 2},
		{"middle of third block", 2*blockSize + 100, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := blockIndexForOffset(tc.off); got != tc.want {
				t.Errorf("blockIndexForOffset(%d) = %d, want %d", tc.off, got, tc.want)
			}
		})
	}
}

func TestBlockByteOffset(t *testing.T) {
	cases := []struct {
		name string
		idx  int
		want int64
	}{
		{"first block", 1, 0},
		{"second block", 2, blockSize},
		{"fifth block", 5, 4 * blockSize},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := blockByteOffset(tc.idx); got != tc.want {
				t.Errorf("blockByteOffset(%d) = %d, want %d", tc.idx, got, tc.want)
			}
		})
	}
}

func TestBlockIndexRoundTrip(t *testing.T) {
	// Every offset inside a block must map back to that block's own byte range.
	for idx := 1; idx <= 5; idx++ {
		start := blockByteOffset(idx)
		for _, off := range []int64{start, start + blockSize/2, start + blockSize - 1} {
			if got := blockIndexForOffset(off); got != idx {
				t.Errorf("blockIndexForOffset(%d) = %d, want %d (block start %d)", off, got, idx, start)
			}
		}
	}
}

func TestCacheOnDisk(t *testing.T) {
	cases := []struct {
		name      string
		size      int64
		largeFile int64
		want      bool
	}{
		{"below threshold", 100, 300, true},
		{"at threshold", 300, 300, true},
		{"above threshold", 301, 300, false},
		{"threshold disabled", 1 << 40, 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cacheOnDisk(tc.size, tc.largeFile); got != tc.want {
				t.Errorf("cacheOnDisk(%d, %d) = %v, want %v", tc.size, tc.largeFile, got, tc.want)
			}
		})
	}
}

func TestParseXAttr(t *testing.T) {
	cases := []struct {
		name      string
		json      string
		wantSize  int64
		wantMTime string
		wantErr   bool
	}{
		{
			name:      "well-formed common block",
			json:      `{"Common":{"ModificationTime":"2021-09-16T07:40:54+00:00","Size":13283,"BlockSizes":[1,2,3],"Digests":{"SHA1":"abc"}}}`,
			wantSize:  13283,
			wantMTime: "2021-09-16T07:40:54+00:00",
		},
		{
			name:     "missing common block",
			json:     `{}`,
			wantSize: 0,
		},
		{
			name:    "invalid json",
			json:    `not json`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			common, err := parseXAttr([]byte(tc.json))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseXAttr(%q) expected error, got nil", tc.json)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseXAttr(%q) unexpected error: %v", tc.json, err)
			}
			if common.Size != tc.wantSize {
				t.Errorf("Size = %d, want %d", common.Size, tc.wantSize)
			}
			if common.ModificationTime != tc.wantMTime {
				t.Errorf("ModificationTime = %q, want %q", common.ModificationTime, tc.wantMTime)
			}
		})
	}
}

// newTestFile builds a File backed by a single encrypted block. The caller controls the reported
// size independently from the actual plaintext length so tests can simulate a size mismatch
// (encrypted size leaking through as the reported size).
func newTestFile(t *testing.T, plain []byte, reportedSize int64) *File {
	t.Helper()

	sk, err := crypto.GenerateSessionKey()
	if err != nil {
		t.Fatal(err)
	}

	ciphertext, err := sk.Encrypt(crypto.NewPlainMessage(plain))
	if err != nil {
		t.Fatal(err)
	}

	orig := fetchBlock
	fetchBlock = func(_ context.Context, _ *Client, _, _ string) ([]byte, error) {
		return ciphertext, nil
	}
	t.Cleanup(func() { fetchBlock = orig })

	return &File{
		client:     &Client{},
		sessionKey: sk,
		blocks:     map[int]proton.Block{1: {Index: 1, BareURL: "http://example.invalid", Token: "tok"}},
		size:       reportedSize,
		cache:      make(map[int][]byte),
	}
}

// newTestFileMultiBlock builds a File backed by two independently encrypted blocks, each with its
// own ciphertext. The reported size is set by the caller.
func newTestFileMultiBlock(t *testing.T, block1, block2 []byte, reportedSize int64) *File {
	t.Helper()

	sk, err := crypto.GenerateSessionKey()
	if err != nil {
		t.Fatal(err)
	}

	ct1, err := sk.Encrypt(crypto.NewPlainMessage(block1))
	if err != nil {
		t.Fatal(err)
	}
	ct2, err := sk.Encrypt(crypto.NewPlainMessage(block2))
	if err != nil {
		t.Fatal(err)
	}

	ciphertexts := map[int][]byte{1: ct1, 2: ct2}

	orig := fetchBlock
	fetchBlock = func(_ context.Context, _ *Client, url, _ string) ([]byte, error) {
		switch url {
		case "http://block1":
			return ciphertexts[1], nil
		case "http://block2":
			return ciphertexts[2], nil
		}
		return nil, errors.New("unknown block URL")
	}
	t.Cleanup(func() { fetchBlock = orig })

	return &File{
		client:     &Client{},
		sessionKey: sk,
		blocks: map[int]proton.Block{
			1: {Index: 1, BareURL: "http://block1", Token: "tok1"},
			2: {Index: 2, BareURL: "http://block2", Token: "tok2"},
		},
		size:  reportedSize,
		cache: make(map[int][]byte),
	}
}

// TestReadAtSizeMismatchReturnsEOF verifies that ReadAt returns io.EOF (not a zero-length
// success) when the reported file size is larger than the actual decrypted data. This happens
// when the encrypted/armored size leaks through as the file's reported size, which is common
// enough in practice that ZIP-based format readers (.xlsx, .docx, .3mf, etc.) break if the
// read silently returns (0, nil) instead.
func TestReadAtSizeMismatchReturnsEOF(t *testing.T) {
	plain := bytes.Repeat([]byte("x"), 100)
	inflatedSize := int64(112) // simulates PGP overhead leaking into the reported size

	f := newTestFile(t, plain, inflatedSize)

	// Offset 105 is within the reported size (112) but past the actual plaintext (100 bytes).
	buf := make([]byte, 7)
	n, err := f.ReadAt(context.Background(), buf, 105)
	if n != 0 {
		t.Errorf("ReadAt returned n=%d, want 0 (no data available at that offset)", n)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt returned err=%v, want io.EOF", err)
	}
}

// TestReadAtAccurateSize verifies normal behavior when the reported size matches the plaintext:
// reading from offset 0 returns the full content, and reading at or past the end returns EOF.
func TestReadAtAccurateSize(t *testing.T) {
	plain := []byte("accurate-size-content")
	f := newTestFile(t, plain, int64(len(plain)))

	// Full read from the start.
	buf := make([]byte, len(plain))
	n, err := f.ReadAt(context.Background(), buf, 0)
	if err != nil {
		t.Fatalf("ReadAt(0): unexpected error: %v", err)
	}
	if n != len(plain) {
		t.Fatalf("ReadAt(0): n=%d, want %d", n, len(plain))
	}
	if !bytes.Equal(buf[:n], plain) {
		t.Fatalf("ReadAt(0): got %q, want %q", buf[:n], plain)
	}

	// Read at exactly the file size returns EOF.
	n, err = f.ReadAt(context.Background(), buf, int64(len(plain)))
	if n != 0 {
		t.Errorf("ReadAt(size): n=%d, want 0", n)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt(size): err=%v, want io.EOF", err)
	}

	// Read past the file size also returns EOF.
	n, err = f.ReadAt(context.Background(), buf, int64(len(plain))+10)
	if n != 0 {
		t.Errorf("ReadAt(size+10): n=%d, want 0", n)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt(size+10): err=%v, want io.EOF", err)
	}
}

// TestReadAtPartialBlockThenEOF uses two blocks where the reported size extends past the actual
// data. A read starting in the middle of the second block should return whatever bytes are
// available, and a subsequent read past the actual data should return EOF.
func TestReadAtPartialBlockThenEOF(t *testing.T) {
	block1 := bytes.Repeat([]byte("A"), blockSize) // full first block
	block2 := bytes.Repeat([]byte("B"), 200)       // short second block

	actualLen := int64(blockSize) + 200
	inflatedSize := actualLen + 50 // reported size extends 50 bytes past actual data

	f := newTestFileMultiBlock(t, block1, block2, inflatedSize)

	// Read starting 100 bytes into the second block: should get the remaining 100 bytes.
	// The buffer is larger than the available data, so io.ReaderAt requires io.EOF.
	readOff := int64(blockSize) + 100
	buf := make([]byte, 256)
	n, err := f.ReadAt(context.Background(), buf, readOff)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt(blockSize+100): err=%v, want io.EOF (short read)", err)
	}
	if n != 100 {
		t.Fatalf("ReadAt(blockSize+100): n=%d, want 100", n)
	}
	expected := bytes.Repeat([]byte("B"), 100)
	if !bytes.Equal(buf[:n], expected) {
		t.Fatalf("ReadAt(blockSize+100): got %q, want %q", buf[:n], expected)
	}

	// Read past actual data but within the inflated reported size: must return EOF.
	pastDataOff := actualLen + 10
	n, err = f.ReadAt(context.Background(), buf, pastDataOff)
	if n != 0 {
		t.Errorf("ReadAt(past data): n=%d, want 0", n)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt(past data): err=%v, want io.EOF", err)
	}
}

// TestReadAtTrailingStructures covers several file formats where readers seek near the end of
// the file to find a trailing structure (xref table, trailer, moov atom, IFD, etc.). When the
// reported size includes PGP overhead, the computed seek offset is past the actual plaintext
// and the read must return (0, io.EOF) instead of a silent (0, nil).
func TestReadAtTrailingStructures(t *testing.T) {
	cases := []struct {
		name      string
		readSize  int   // bytes the format reader tries to read from the end
		overhead  int64 // how much larger encrypted size is vs plaintext
		plainSize int64 // actual plaintext file size
	}{
		{
			name:      "pdf xref scan",
			readSize:  64,
			overhead:  112,
			plainSize: 50000,
		},
		{
			name:      "gzip trailer",
			readSize:  8,
			overhead:  48,
			plainSize: 1024,
		},
		{
			name:      "mp4 moov atom header",
			readSize:  8,
			overhead:  96,
			plainSize: 80000,
		},
		{
			name:      "sqlite page read",
			readSize:  4096,
			overhead:  4200,
			plainSize: 32768,
		},
		{
			name:      "png iend chunk",
			readSize:  12,
			overhead:  64,
			plainSize: 4096,
		},
		{
			name:      "tiff ifd near eof",
			readSize:  12,
			overhead:  80,
			plainSize: 16384,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plain := bytes.Repeat([]byte("D"), int(tc.plainSize))

			t.Run("inflated size", func(t *testing.T) {
				inflated := tc.plainSize + tc.overhead
				f := newTestFile(t, plain, inflated)

				// The format reader computes offset = reportedSize - readSize.
				// With inflated size this is past the actual plaintext.
				seekOff := inflated - int64(tc.readSize)
				buf := make([]byte, tc.readSize)
				n, err := f.ReadAt(context.Background(), buf, seekOff)
				if n != 0 {
					t.Errorf("n=%d, want 0 (offset past actual data)", n)
				}
				if !errors.Is(err, io.EOF) {
					t.Errorf("err=%v, want io.EOF", err)
				}
			})

			t.Run("correct size", func(t *testing.T) {
				f := newTestFile(t, plain, tc.plainSize)

				seekOff := tc.plainSize - int64(tc.readSize)
				buf := make([]byte, tc.readSize)
				n, err := f.ReadAt(context.Background(), buf, seekOff)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if n != tc.readSize {
					t.Fatalf("n=%d, want %d", n, tc.readSize)
				}
			})
		})
	}
}

// TestReadAtZipEndOfCentralDirectory simulates the access pattern a ZIP reader uses: it seeks to
// (file_size - 22) to read the End of Central Directory record (22 bytes). When the reported size
// includes PGP overhead, that seek offset ends up past the actual plaintext, which used to return
// (0, nil) and silently break every ZIP-based format (.xlsx, .docx, .3mf, .epub, .jar, .odt).
func TestReadAtZipEndOfCentralDirectory(t *testing.T) {
	const eocdSize = 22

	// Build a plaintext large enough that the last 22 bytes are meaningful.
	plain := bytes.Repeat([]byte("Z"), 200)
	// Overwrite the last 22 bytes with a recognizable pattern.
	copy(plain[len(plain)-eocdSize:], bytes.Repeat([]byte("E"), eocdSize))

	overhead := int64(16) // simulated PGP overhead
	inflatedSize := int64(len(plain)) + overhead

	t.Run("inflated size causes EOF", func(t *testing.T) {
		// Overhead large enough that (inflated - 22) > plaintext length.
		bigOverhead := int64(30)
		bigInflated := int64(len(plain)) + bigOverhead

		f := newTestFile(t, plain, bigInflated)
		// Offset = 230 - 22 = 208, past the 200-byte plaintext.
		seekOff := bigInflated - eocdSize
		buf := make([]byte, eocdSize)
		n, err := f.ReadAt(context.Background(), buf, seekOff)
		if n != 0 {
			t.Errorf("inflated read: n=%d, want 0", n)
		}
		if !errors.Is(err, io.EOF) {
			t.Errorf("inflated read: err=%v, want io.EOF", err)
		}
	})

	t.Run("small overhead still returns partial data", func(t *testing.T) {
		// With small overhead, (inflated - 22) is still within the plaintext. The read
		// returns however many bytes are left from that offset to the actual end, with
		// io.EOF because fewer than len(buf) bytes were returned (io.ReaderAt contract).
		f := newTestFile(t, plain, inflatedSize)
		seekOff := inflatedSize - eocdSize // 216 - 22 = 194, within 200 bytes
		buf := make([]byte, eocdSize)
		n, err := f.ReadAt(context.Background(), buf, seekOff)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("ReadAt: err=%v, want io.EOF (short read)", err)
		}
		wantN := len(plain) - int(seekOff) // 200 - 194 = 6 bytes available
		if n != wantN {
			t.Fatalf("ReadAt: n=%d, want %d", n, wantN)
		}
	})

	t.Run("accurate size returns EOCD", func(t *testing.T) {
		f := newTestFile(t, plain, int64(len(plain)))

		seekOff := int64(len(plain)) - eocdSize // 200 - 22 = 178
		buf := make([]byte, eocdSize)
		n, err := f.ReadAt(context.Background(), buf, seekOff)
		if err != nil {
			t.Fatalf("ReadAt: unexpected error: %v", err)
		}
		if n != eocdSize {
			t.Fatalf("ReadAt: n=%d, want %d", n, eocdSize)
		}

		want := bytes.Repeat([]byte("E"), eocdSize)
		if !bytes.Equal(buf[:n], want) {
			t.Fatalf("ReadAt: got %q, want %q", buf[:n], want)
		}
	})
}

// TestReadAtShortReadReturnsEOF verifies the io.ReaderAt contract: when fewer than len(p) bytes
// are returned, the error must be non-nil (io.EOF when at end of data).
func TestReadAtShortReadReturnsEOF(t *testing.T) {
	plain := []byte("short content")
	f := newTestFile(t, plain, int64(len(plain)))

	// Request more bytes than the file contains; should return all bytes plus io.EOF.
	buf := make([]byte, 100)
	n, err := f.ReadAt(context.Background(), buf, 0)
	if n != len(plain) {
		t.Fatalf("n=%d, want %d", n, len(plain))
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v, want io.EOF (n < len(p) per io.ReaderAt)", err)
	}
}

// TestCorrectSizeFromBlocksSingleBlock verifies that correctSizeFromBlocks computes the right
// plaintext size from a single block's decrypted length.
func TestCorrectSizeFromBlocksSingleBlock(t *testing.T) {
	plain := []byte("hello zip file content")
	inflatedSize := int64(len(plain)) + 20 // simulated encrypted overhead

	f := newTestFile(t, plain, inflatedSize)

	got, err := f.correctSizeFromBlocks(context.Background())
	if err != nil {
		t.Fatalf("correctSizeFromBlocks: %v", err)
	}
	if got != int64(len(plain)) {
		t.Errorf("correctSizeFromBlocks = %d, want %d", got, len(plain))
	}
}

// TestCorrectSizeFromBlocksMultiBlock verifies size computation across two blocks where the
// last block is shorter than blockSize.
func TestCorrectSizeFromBlocksMultiBlock(t *testing.T) {
	block1 := bytes.Repeat([]byte("A"), blockSize)
	block2 := bytes.Repeat([]byte("B"), 500)
	actualSize := int64(blockSize) + 500
	inflatedSize := actualSize + 100

	f := newTestFileMultiBlock(t, block1, block2, inflatedSize)

	got, err := f.correctSizeFromBlocks(context.Background())
	if err != nil {
		t.Fatalf("correctSizeFromBlocks: %v", err)
	}
	if got != actualSize {
		t.Errorf("correctSizeFromBlocks = %d, want %d", got, actualSize)
	}
}

// TestCorrectSizeFromBlocksEmpty verifies that an empty block list returns size 0.
func TestCorrectSizeFromBlocksEmpty(t *testing.T) {
	f := &File{
		client: &Client{},
		blocks: map[int]proton.Block{},
		cache:  make(map[int][]byte),
	}

	got, err := f.correctSizeFromBlocks(context.Background())
	if err != nil {
		t.Fatalf("correctSizeFromBlocks: %v", err)
	}
	if got != 0 {
		t.Errorf("correctSizeFromBlocks = %d, want 0", got)
	}
}
