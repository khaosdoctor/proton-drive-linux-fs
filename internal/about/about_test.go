package about

import (
	"strings"
	"testing"
)

func TestLicenses(t *testing.T) {
	entries, err := Licenses()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one embedded license")
	}

	for _, e := range entries {
		if e.Module == "" || e.File == "" {
			t.Fatalf("incomplete entry: %+v", e)
		}
		if _, err := LicenseText(e); err != nil {
			t.Fatalf("reading license for %s: %v", e.Module, err)
		}
	}
}

func TestText(t *testing.T) {
	text := Text("1.2.3", "abc1234")

	for _, want := range []string{"proton-drive-fs", "by Lucas Santos", "1.2.3", "abc1234", "github.com/khaosdoctor/proton-drive-linux-fs"} {
		if !strings.Contains(text, want) {
			t.Errorf("Text() missing %q", want)
		}
	}
}
