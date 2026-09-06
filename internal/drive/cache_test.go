package drive

import (
	"encoding/json"
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

func TestListingCachePutGetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	bc, err := OpenBlockCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	want := []ListingEntry{
		{Name: "a.txt", Link: json.RawMessage(`{"LinkID":"l1"}`)},
		{Name: "b.txt", Link: json.RawMessage(`{"LinkID":"l2"}`)},
	}
	bc.PutListing("parent1", want)

	got, ok := bc.GetListing("parent1")
	if !ok {
		t.Fatal("expected listing cache hit")
	}
	if len(got) != len(want) || got[0].Name != "a.txt" || got[1].Name != "b.txt" {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	if _, ok := bc.GetListing("parent2"); ok {
		t.Fatal("expected miss for unknown parent")
	}

	// The listing holds decrypted names: it must land at 0600 inside a 0700 directory, same as the
	// session file, and via an atomic rename (no .tmp file left behind).
	listingsDir := filepath.Join(dir, listingsSubdir)
	dirInfo, err := os.Stat(listingsDir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0700 {
		t.Errorf("listings dir mode = %v, want 0700", dirInfo.Mode().Perm())
	}

	entries, err := os.ReadDir(listingsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("listings dir has %d entries, want exactly 1 (no leftover .tmp file): %v", len(entries), entries)
	}
	fileInfo, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0600 {
		t.Errorf("listing file mode = %v, want 0600", fileInfo.Mode().Perm())
	}
}

func TestListingCacheDisabled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "unused")
	bc, err := OpenBlockCache(dir, 0)
	if err != nil {
		t.Fatal(err)
	}

	bc.PutListing("parent1", []ListingEntry{{Name: "a"}})

	if _, ok := bc.GetListing("parent1"); ok {
		t.Fatal("expected disabled cache to never hit")
	}
}

// TestEvictionSpansBlocksAndListings checks the shared byte budget is enforced across both kinds:
// the oldest entry evicted first regardless of whether it is a block or a persisted listing.
func TestEvictionSpansBlocksAndListings(t *testing.T) {
	bc, err := OpenBlockCache(t.TempDir(), 20)
	if err != nil {
		t.Fatal(err)
	}

	bc.Put("link1", "rev1", 1, make([]byte, 10)) // block, size 10
	time.Sleep(10 * time.Millisecond)
	bc.PutListing("parent1", []ListingEntry{{Name: "aaaaaaaaaa"}}) // listing, oversized on purpose
	time.Sleep(10 * time.Millisecond)
	bc.Put("link1", "rev1", 2, make([]byte, 10)) // pushes the cache over budget

	if _, ok := bc.Get("link1", "rev1", 1); ok {
		t.Fatal("expected the oldest block to be evicted")
	}
	if _, ok := bc.GetListing("parent1"); ok {
		t.Fatal("expected the persisted listing to be evicted next, it is older than block 2")
	}
	if _, ok := bc.Get("link1", "rev1", 2); !ok {
		t.Fatal("expected the newest block to survive")
	}
}

// TestMigrateLayoutMovesOldEntriesUnderBlocks checks a pre-listing-cache install (blocks stored
// directly at <dir>/<linkID>/...) gets its entries moved under <dir>/blocks on the next open,
// without disturbing their content.
func TestMigrateLayoutMovesOldEntriesUnderBlocks(t *testing.T) {
	dir := t.TempDir()

	oldPath := filepath.Join(dir, "link1", "rev1", "0")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("old block"), 0600); err != nil {
		t.Fatal(err)
	}

	bc, err := OpenBlockCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	got, ok := bc.Get("link1", "rev1", 0)
	if !ok {
		t.Fatal("expected the migrated block to be readable at its new path")
	}
	if string(got) != "old block" {
		t.Fatalf("got %q, want %q", got, "old block")
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expected the old path to be gone after migration, stat err = %v", err)
	}
}

// TestMigrateLayoutIsNoOpOnFreshCache checks a brand-new cache dir (nothing to migrate) does not
// error and simply gets the new blocks/ and listings/ subdirectories.
func TestMigrateLayoutIsNoOpOnFreshCache(t *testing.T) {
	dir := t.TempDir()

	bc, err := OpenBlockCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	bc.Put("link1", "rev1", 0, []byte("data"))
	if _, ok := bc.Get("link1", "rev1", 0); !ok {
		t.Fatal("expected the block to be readable after opening a fresh cache")
	}

	for _, sub := range []string{"blocks", "listings"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			t.Errorf("expected %s to exist: %v", sub, err)
		}
	}
}
