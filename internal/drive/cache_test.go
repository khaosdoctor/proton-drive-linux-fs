package drive

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBlockCachePutGetRoundTrip(t *testing.T) {
	bc, err := OpenBlockCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	want := []byte("hello block")
	bc.Put("link1", "rev1", 3, want)

	got, ok := bc.Get("link1", "rev1", 3)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	if _, ok := bc.Get("link1", "rev1", 4); ok {
		t.Fatal("expected miss for unknown index")
	}
}

func TestBlockCacheEvictsOldest(t *testing.T) {
	bc, err := OpenBlockCache(t.TempDir(), 20)
	if err != nil {
		t.Fatal(err)
	}

	bc.Put("link1", "rev1", 1, make([]byte, 10))
	time.Sleep(10 * time.Millisecond) // distinct mtimes for LRU ordering
	bc.Put("link1", "rev1", 2, make([]byte, 10))
	time.Sleep(10 * time.Millisecond)
	bc.Put("link1", "rev1", 3, make([]byte, 10)) // size now 30 > limit 20, evicts block 1

	if _, ok := bc.Get("link1", "rev1", 1); ok {
		t.Fatal("expected oldest block to be evicted")
	}
	if _, ok := bc.Get("link1", "rev1", 2); !ok {
		t.Fatal("expected block 2 to survive")
	}
	if _, ok := bc.Get("link1", "rev1", 3); !ok {
		t.Fatal("expected block 3 to survive")
	}
}

func TestBlockCacheDisabled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "unused")
	bc, err := OpenBlockCache(dir, 0)
	if err != nil {
		t.Fatal(err)
	}

	bc.Put("link1", "rev1", 1, []byte("data"))

	if _, ok := bc.Get("link1", "rev1", 1); ok {
		t.Fatal("expected disabled cache to never hit")
	}

	if _, err := os.Stat(dir); err == nil {
		t.Fatal("expected disabled cache to not create a directory")
	}
}
