package thumbs

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestLoadRegistryParsesThumbnailerFile(t *testing.T) {
	dir := t.TempDir()
	content := "[Thumbnailer Entry]\nTryExec=/bin/true\nExec=/bin/true %i %s %o\nMimeType=text/x-test;application/x-test;\n"
	if err := os.WriteFile(filepath.Join(dir, "test.thumbnailer"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Registry{byMIME: make(map[string]thumbExec)}
	r.loadDir(dir)

	for _, mt := range []string{"text/x-test", "application/x-test"} {
		if _, ok := r.byMIME[mt]; !ok {
			t.Errorf("missing %s in registry", mt)
		}
	}
}

func TestRegistrySkipsMissingTryExec(t *testing.T) {
	dir := t.TempDir()
	content := "[Thumbnailer Entry]\nTryExec=/nonexistent/binary\nExec=/nonexistent/binary %i %s %o\nMimeType=text/x-missing;\n"
	if err := os.WriteFile(filepath.Join(dir, "missing.thumbnailer"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Registry{byMIME: make(map[string]thumbExec)}
	r.loadDir(dir)

	if len(r.byMIME) != 0 {
		t.Errorf("got %d entries, want 0", len(r.byMIME))
	}
}

func TestProcessNames(t *testing.T) {
	r := &Registry{byMIME: map[string]thumbExec{
		"text/x-scad":        {exec: "/usr/local/bin/scad-thumbnailer-script %i %s %o"},
		"application/x-scad": {exec: "/usr/local/bin/scad-thumbnailer-script %i %s %o"},
		"model/stl":          {exec: "/usr/local/bin/stl-thumbnailer-script %i %s %o"},
	}}

	names := r.ProcessNames()
	sort.Strings(names)

	want := []string{"scad-thumbnailer-script", "stl-thumbnailer-script"}
	sort.Strings(want)

	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestExecBasename(t *testing.T) {
	tests := []struct {
		exec string
		want string
	}{
		{"/usr/local/bin/scad-thumbnailer-script %i %s %o", "scad-thumbnailer-script"},
		{"ffmpegthumbnailer -i %i -o %o -s %s -f", "ffmpegthumbnailer"},
		{"/usr/lib/freecad/bin/freecad-thumbnailer -s %s %i %o", "freecad-thumbnailer"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := execBasename(tc.exec); got != tc.want {
			t.Errorf("execBasename(%q) = %q, want %q", tc.exec, got, tc.want)
		}
	}
}

func TestForExtNilRegistry(t *testing.T) {
	var r *Registry
	if got := r.ForExt(".scad"); got != "" {
		t.Errorf("nil registry returned %q", got)
	}
}

func TestProcessNamesNilRegistry(t *testing.T) {
	var r *Registry
	if got := r.ProcessNames(); got != nil {
		t.Errorf("nil registry returned %v", got)
	}
}
