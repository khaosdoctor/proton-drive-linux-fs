package fusefs

import (
	"testing"
	"time"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
)

func TestPublishFailedRefreshSetsShortExpiry(t *testing.T) {
	d := &dirNode{
		ttl:     10 * time.Second,
		loading: make(chan struct{}),
		children: []*drive.Node{
			{Name: "existing"},
		},
	}

	done := make(chan struct{})
	startGen := uint64(42)

	before := time.Now()
	d.publish(nil, done, startGen)
	after := time.Now()

	<-done

	d.mu.Lock()
	expires := d.expires
	d.mu.Unlock()

	if expires.Before(before) {
		t.Errorf("expires before now: %v", expires)
	}

	maxExpiry := after.Add(failedLoadCooldown)
	if expires.After(maxExpiry) {
		t.Errorf("expires after now + failedLoadCooldown: %v", expires)
	}

	maxTTL := after.Add(d.ttl)
	if expires.After(maxTTL) {
		t.Errorf("expires after now + ttl: %v", expires)
	}
}
