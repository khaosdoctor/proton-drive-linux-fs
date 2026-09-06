package fusefs

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
)

func node(linkID, name string) *drive.Node {
	return &drive.Node{Link: proton.Link{LinkID: linkID}, Name: name}
}

func names(children []*drive.Node) []string {
	out := make([]string, 0, len(children))
	for _, ch := range children {
		out = append(out, ch.Name)
	}
	return out
}

func TestUpsertNode(t *testing.T) {
	base := []*drive.Node{node("l1", "a"), node("l2", "b")}

	t.Run("appends a new entry", func(t *testing.T) {
		got := upsertNode(append([]*drive.Node(nil), base...), node("l3", "c"))
		if want := []string{"a", "b", "c"}; !slices.Equal(names(got), want) {
			t.Errorf("names = %v, want %v", names(got), want)
		}
	})

	t.Run("replaces by link id even when renamed", func(t *testing.T) {
		got := upsertNode(append([]*drive.Node(nil), base...), node("l2", "b2"))
		if want := []string{"a", "b2"}; !slices.Equal(names(got), want) {
			t.Errorf("names = %v, want %v", names(got), want)
		}
	})

	t.Run("replaces by name for a new link", func(t *testing.T) {
		replacement := node("l9", "b")
		got := upsertNode(append([]*drive.Node(nil), base...), replacement)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		if got[1] != replacement {
			t.Errorf("entry b was not replaced: %v", got[1].Link.LinkID)
		}
	})

	t.Run("appends into an empty listing", func(t *testing.T) {
		got := upsertNode(nil, node("l1", "a"))
		if len(got) != 1 {
			t.Errorf("len = %d, want 1", len(got))
		}
	})
}

func TestRemoveNode(t *testing.T) {
	base := []*drive.Node{node("l1", "a"), node("l2", "b"), node("l3", "c")}

	removed, got := removeNode(append([]*drive.Node(nil), base...), "b")
	if removed == nil || removed.Link.LinkID != "l2" {
		t.Fatalf("removed = %v, want the node for l2", removed)
	}
	if want := []string{"a", "c"}; !slices.Equal(names(got), want) {
		t.Errorf("names = %v, want %v", names(got), want)
	}

	removed, got = removeNode(append([]*drive.Node(nil), base...), "missing")
	if removed != nil {
		t.Errorf("removed = %v, want nil", removed)
	}
	if len(got) != 3 {
		t.Errorf("len = %d, want the listing untouched", len(got))
	}
}

// TestBeginLoadCollapsesFetches checks the singleflight guard: while one goroutine owns the
// fetch, a second one is told to wait, and it sees the published listing once the first is done.
func TestBeginLoadCollapsesFetches(t *testing.T) {
	d := &dirNode{ttl: time.Minute}

	children, done, wait := d.beginLoad()
	if children != nil || done == nil || wait != nil {
		t.Fatalf("first caller should own the fetch, got children=%v done=%v wait=%v", children, done, wait)
	}

	children, done2, wait2 := d.beginLoad()
	if children != nil || done2 != nil || wait2 == nil {
		t.Fatalf("second caller should wait, got children=%v done=%v wait=%v", children, done2, wait2)
	}

	waited := make(chan []*drive.Node, 1)
	go func() {
		<-wait2
		got, _, _ := d.beginLoad()
		waited <- got
	}()

	d.publish([]*drive.Node{node("l1", "a")}, done, 0)

	select {
	case got := <-waited:
		if len(got) != 1 {
			t.Errorf("waiter saw %d children, want the published listing", len(got))
		}
	case <-time.After(time.Second):
		t.Fatal("waiter was never woken by publish")
	}
}

// TestPublishFailureLetsTheNextCallerFetch checks a failed fetch does not leave the guard held.
func TestPublishFailureLetsTheNextCallerFetch(t *testing.T) {
	d := &dirNode{ttl: time.Minute}

	_, done, _ := d.beginLoad()
	d.publish(nil, done, 0)

	children, done2, wait := d.beginLoad()
	if children != nil || done2 == nil || wait != nil {
		t.Fatalf("next caller should own a fresh fetch, got children=%v done=%v wait=%v", children, done2, wait)
	}
}

// TestLoadSurvivesLeaderCancel checks the singleflight fetch is detached from whichever caller
// triggered it: the caller that cancels while the fetch is still in flight only fails for itself
// (EINTR), and a second caller with a live context still gets the fetch's result once it lands.
func TestLoadSurvivesLeaderCancel(t *testing.T) {
	st := &mountState{opTimeout: time.Second, parentOf: make(map[string]string)}
	d := &dirNode{ttl: time.Minute, st: st, node: node("root", "root")}

	release := make(chan struct{})
	orig := fetchChildren
	fetchChildren = func(ctx context.Context, c *drive.Client, n *drive.Node) ([]*drive.Node, error) {
		<-release
		return []*drive.Node{node("l1", "a")}, nil
	}
	defer func() { fetchChildren = orig }()

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		if _, errno := d.load(leaderCtx); errno != syscall.EINTR {
			t.Errorf("leader errno = %v, want EINTR", errno)
		}
	}()

	// Let the leader become the fetch owner (its beginLoad call) before cancelling it.
	time.Sleep(50 * time.Millisecond)
	cancelLeader()

	select {
	case <-leaderDone:
	case <-time.After(2 * time.Second):
		t.Fatal("leader never returned after its context was cancelled")
	}

	waiterDone := make(chan struct{})
	var waiterChildren []*drive.Node
	var waiterErrno syscall.Errno
	go func() {
		defer close(waiterDone)
		waiterChildren, waiterErrno = d.load(context.Background())
	}()

	// The leader cancelling must not have cancelled the still in-flight fetch; let it complete now.
	close(release)

	select {
	case <-waiterDone:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never returned once the fetch completed")
	}

	if waiterErrno != 0 {
		t.Fatalf("waiter errno = %v, want 0", waiterErrno)
	}
	if want := []string{"a"}; !slices.Equal(names(waiterChildren), want) {
		t.Errorf("waiter children = %v, want %v", names(waiterChildren), want)
	}
}

// TestServeFromDiskCacheTriggersImmediateBackgroundRefresh checks a cold directory's disk-cached
// listing is served right away, and that it kicks off the normal network fetch immediately in the
// background instead of waiting for a later TTL expiry: without that, a disk-served listing (with
// no automatic staleness check of its own) could sit unrefreshed indefinitely.
func TestServeFromDiskCacheTriggersImmediateBackgroundRefresh(t *testing.T) {
	origCached, origFetch := cachedChildren, fetchChildren
	defer func() { cachedChildren, fetchChildren = origCached, origFetch }()

	cachedChildren = func(c *drive.Client, n *drive.Node) ([]*drive.Node, bool) {
		return []*drive.Node{node("l1", "cached")}, true
	}

	fetchCalled := make(chan struct{}, 1)
	fetchChildren = func(ctx context.Context, c *drive.Client, n *drive.Node) ([]*drive.Node, error) {
		fetchCalled <- struct{}{}
		return []*drive.Node{node("l1", "fresh")}, nil
	}

	d := &dirNode{
		ttl:  time.Minute,
		st:   &mountState{opTimeout: time.Second, parentOf: make(map[string]string)},
		node: node("root", "root"),
	}

	children, errno := d.load(context.Background())
	if errno != 0 {
		t.Fatalf("errno = %v, want 0", errno)
	}
	if want := []string{"cached"}; !slices.Equal(names(children), want) {
		t.Fatalf("children = %v, want the disk-cached listing served immediately: %v", names(children), want)
	}

	select {
	case <-fetchCalled:
	case <-time.After(time.Second):
		t.Fatal("expected the network refresh to start immediately after the disk-cache serve")
	}
}

// newTestDirNodeWithCache builds a dirNode backed by a real, temp-dir-rooted on-disk listing
// cache, so persistListing's writes can be read back with cache.GetListing directly - no keyring
// needed for that, unlike going through Client.CachedChildren.
func newTestDirNodeWithCache(t *testing.T) (*dirNode, *drive.BlockCache) {
	t.Helper()

	cache, err := drive.OpenBlockCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	client := &drive.Client{}
	client.SetBlockCache(cache, 0)

	return &dirNode{client: client, node: node("root", "root"), ttl: time.Minute}, cache
}

// TestPersistListingDropsOutOfOrderWrites checks a persist for an older gen is dropped once a
// newer gen has already won, instead of clobbering it - the out-of-order write two concurrent
// upsertChild/removeChild patches (or a patch racing a network fetch's own persist) could produce.
func TestPersistListingDropsOutOfOrderWrites(t *testing.T) {
	d, cache := newTestDirNodeWithCache(t)

	d.persistListing(d.node, []*drive.Node{node("l1", "newer")}, 5)
	d.persistListing(d.node, []*drive.Node{node("l1", "older")}, 2) // stale: must not win

	entries, ok := cache.GetListing("root")
	if !ok {
		t.Fatal("expected a persisted listing")
	}
	if len(entries) != 1 || entries[0].Name != "newer" {
		t.Fatalf("entries = %+v, want the gen-5 write to have won over the stale gen-2 one", entries)
	}
}

// TestPersistListingConcurrentWritesStayInGenOrder fires many persistListing calls at once, each
// for a distinct gen, and checks the write for the highest gen is what survives regardless of
// goroutine scheduling: persistedGen only moves forward, so once the highest-gen write lands, every
// other one is guaranteed stale.
func TestPersistListingConcurrentWritesStayInGenOrder(t *testing.T) {
	d, cache := newTestDirNodeWithCache(t)

	const highest = 20
	var wg sync.WaitGroup
	for gen := uint64(1); gen <= highest; gen++ {
		wg.Add(1)
		go func(gen uint64) {
			defer wg.Done()
			d.persistListing(d.node, []*drive.Node{node("l1", fmt.Sprintf("gen%d", gen))}, gen)
		}(gen)
	}
	wg.Wait()

	entries, ok := cache.GetListing("root")
	if !ok {
		t.Fatal("expected a persisted listing")
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want exactly one persisted entry", entries)
	}
	if want := fmt.Sprintf("gen%d", highest); entries[0].Name != want {
		t.Fatalf("entries[0].Name = %q, want %q (the highest gen must always win)", entries[0].Name, want)
	}
}

func TestShouldPublish(t *testing.T) {
	tests := []struct {
		name        string
		hadChildren bool
		startGen    uint64
		curGen      uint64
		want        bool
	}{
		{"first ever fetch installs", false, 0, 0, true},
		{"no patch during the fetch installs", true, 3, 3, true},
		{"a patch during the fetch is kept", true, 3, 4, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPublish(tt.hadChildren, tt.startGen, tt.curGen); got != tt.want {
				t.Errorf("shouldPublish(%v, %d, %d) = %v, want %v", tt.hadChildren, tt.startGen, tt.curGen, got, tt.want)
			}
		})
	}
}

// TestPublishKeepsLocalPatchOverStaleFetch checks that a fetch which started before a handler
// patched the cache does not clobber that patch once it lands, but still refreshes the TTL so the
// stale fetch does not trigger an immediate re-fetch.
func TestPublishKeepsLocalPatchOverStaleFetch(t *testing.T) {
	d := &dirNode{
		ttl:      time.Minute,
		node:     node("root", "root"),
		st:       &mountState{parentOf: make(map[string]string)},
		children: []*drive.Node{node("l1", "a")},
	}

	_, done, _ := d.beginLoad()
	startGen := d.gen

	// A handler patches the cache while the fetch above is still in flight.
	d.upsertChild(node("l2", "b"))

	d.publish([]*drive.Node{node("l1", "a")}, done, startGen)

	if want := []string{"a", "b"}; !slices.Equal(names(d.children), want) {
		t.Fatalf("children = %v, want the patch kept: %v", names(d.children), want)
	}
	if !d.expires.After(time.Now()) {
		t.Error("expires was not refreshed even though the patch was kept")
	}
}

func TestSemBounds(t *testing.T) {
	s := newSem(1)
	if !s.acquire(context.Background()) {
		t.Fatal("first acquire should succeed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.acquire(ctx) {
		t.Error("acquire on a full semaphore with a cancelled context should fail")
	}

	s.release()
	if !s.acquire(context.Background()) {
		t.Error("acquire after release should succeed")
	}

	var unbounded sem
	if !unbounded.acquire(context.Background()) {
		t.Error("a nil semaphore should let everything through")
	}
	unbounded.release()
}

func TestUploadCountersResetWhenDrained(t *testing.T) {
	st := &mountState{}
	now := time.Now()

	st.uploadsQueued.Add(3)
	st.uploadsDone.Add(1)

	if q, d, f := st.uploadCounters(now); q != 3 || d != 1 || f != 0 {
		t.Fatalf("counters = %d/%d/%d, want 3/1/0", q, d, f)
	}

	st.uploadsDone.Add(1)
	st.uploadsFailed.Add(1)

	// The queue has just drained; the counters stay up so the tray can show the final total.
	if q, d, f := st.uploadCounters(now); q != 3 || d != 2 || f != 1 {
		t.Fatalf("counters = %d/%d/%d, want 3/2/1 right after draining", q, d, f)
	}
	if q, _, _ := st.uploadCounters(now.Add(counterResetIdle - time.Second)); q != 3 {
		t.Errorf("queued = %d, want the counters kept before the idle delay", q)
	}

	if q, d, f := st.uploadCounters(now.Add(counterResetIdle)); q != 0 || d != 0 || f != 0 {
		t.Errorf("counters = %d/%d/%d, want them zeroed after the idle delay", q, d, f)
	}
}

func TestUploadCountersKeepCountingWhileQueued(t *testing.T) {
	st := &mountState{}
	now := time.Now()

	st.uploadsQueued.Add(2)
	st.uploadsDone.Add(2)
	st.uploadCounters(now) // marks the queue as drained

	st.uploadsQueued.Add(1)
	if q, d, _ := st.uploadCounters(now.Add(2 * counterResetIdle)); q != 3 || d != 2 {
		t.Errorf("counters = %d/%d, want 3/2: a new upload cancels the reset", q, d)
	}
}
