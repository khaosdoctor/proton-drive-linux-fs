package drive

import (
	"context"
	"errors"
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
