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
