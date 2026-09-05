package fusefs

import (
	"testing"
	"time"
)

func TestUploadTimeout(t *testing.T) {
	base := 60 * time.Second

	cases := []struct {
		name string
		size int64
		want time.Duration
	}{
		{"empty file uses base", 0, base},
		{"small file uses base", 1 << 20, base}, // 1MiB well under base's allowance
		{"large file scales past base", 400 * (256 << 10), 400 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := uploadTimeout(tc.size, base)
			if got != tc.want {
				t.Errorf("uploadTimeout(%d, %s) = %s, want %s", tc.size, base, got, tc.want)
			}
		})
	}
}
