package fusefs

import (
	"errors"
	"testing"
	"time"
)

// A failed thumbnail must not stay claimed for the life of the mount: a network outage or a
// timeout would otherwise block that preview until restart, and the map would grow per failure.
func TestFailedThumbRetriesLater(t *testing.T) {
	prev := thumbRetryAfter
	thumbRetryAfter = 20 * time.Millisecond
	t.Cleanup(func() { thumbRetryAfter = prev })

	st := &mountState{thumbInflight: make(map[string]bool)}
	n := node("l1", "a.jpg")

	if !st.claimThumb(n) {
		t.Fatal("first claim refused")
	}
	st.thumbFailed(thumbJob{node: n, relPath: "a.jpg"}, errors.New("network down"))

	if st.claimThumb(n) {
		t.Fatal("failed revision claimable again right away; it should wait out thumbRetryAfter")
	}

	deadline := time.Now().Add(2 * time.Second)
	for !st.claimThumb(n) {
		if time.Now().After(deadline) {
			t.Fatal("failed revision still claimed long after thumbRetryAfter")
		}
		time.Sleep(5 * time.Millisecond)
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
