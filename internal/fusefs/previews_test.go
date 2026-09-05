package fusefs

import "testing"

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
