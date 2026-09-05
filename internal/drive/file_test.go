package drive

import "testing"

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
