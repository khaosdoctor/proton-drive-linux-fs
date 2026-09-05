package drive

import (
	"bytes"
	"testing"
)

func TestBlockManifest(t *testing.T) {
	tests := []struct {
		name   string
		hashes [][]byte
		want   []byte
	}{
		{name: "no blocks", hashes: nil, want: nil},
		{name: "one block", hashes: [][]byte{{1, 2, 3}}, want: []byte{1, 2, 3}},
		{
			name:   "concatenates in order",
			hashes: [][]byte{{1, 2}, {3, 4}, {5}},
			want:   []byte{1, 2, 3, 4, 5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := blockManifest(tt.hashes)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("blockManifest(%v) = %v, want %v", tt.hashes, got, tt.want)
			}
		})
	}
}

func TestBlockCount(t *testing.T) {
	tests := []struct {
		name string
		size int64
		want int
	}{
		{name: "zero size", size: 0, want: 0},
		{name: "negative size", size: -1, want: 0},
		{name: "one byte", size: 1, want: 1},
		{name: "exactly one block", size: blockSize, want: 1},
		{name: "one byte over a block", size: blockSize + 1, want: 2},
		{name: "exactly three blocks", size: blockSize * 3, want: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := blockCount(tt.size); got != tt.want {
				t.Errorf("blockCount(%d) = %d, want %d", tt.size, got, tt.want)
			}
		})
	}
}
