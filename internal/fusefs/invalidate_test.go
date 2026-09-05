package fusefs

import (
	"reflect"
	"testing"
)

func TestParentsToInvalidate(t *testing.T) {
	cases := []struct {
		name      string
		newParent string
		oldParent string
		want      []string
	}{
		{"create in a new dir", "dir-b", "", []string{"dir-b"}},
		{"move between dirs", "dir-b", "dir-a", []string{"dir-b", "dir-a"}},
		{"same parent", "dir-a", "dir-a", []string{"dir-a"}},
		{"unknown old parent", "dir-a", "", []string{"dir-a"}},
		{"empty ids", "", "", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parentsToInvalidate(tc.newParent, tc.oldParent)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parentsToInvalidate(%q, %q) = %v, want %v", tc.newParent, tc.oldParent, got, tc.want)
			}
		})
	}
}
