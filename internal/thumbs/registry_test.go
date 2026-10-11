package thumbs

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoadRegistryParsesThumbnailerFile(t *testing.T) {
	dir := t.TempDir()
	content := "[Thumbnailer Entry]\nTryExec=/bin/true\nExec=/bin/true %i %s %o\nMimeType=text/x-test;application/x-test;\n"
	if err := os.WriteFile(filepath.Join(dir, "test.thumbnailer"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Registry{byMIME: make(map[string]string)}
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

	r := &Registry{byMIME: make(map[string]string)}
	r.loadDir(dir)

	if len(r.byMIME) != 0 {
		t.Errorf("got %d entries, want 0", len(r.byMIME))
	}
}

func TestProcessNames(t *testing.T) {
	r := &Registry{byMIME: map[string]string{
		"text/x-scad":        "/usr/local/bin/scad-thumbnailer-script %i %s %o",
		"application/x-scad": "/usr/local/bin/scad-thumbnailer-script %i %s %o",
		"model/stl":          "/usr/local/bin/stl-thumbnailer-script %i %s %o",
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

// A thumbnailer that forks a helper and then hangs must not leave the helper running once the
// timeout kills it: in a long-running daemon those orphans pile up.
func TestGenerateTimeoutKillsForkedChildren(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "thumbnailer.sh")
	content := "sleep 60 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(script, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &Registry{byMIME: map[string]string{"image/png": "/bin/sh " + script + " %i %o"}}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := r.Generate(ctx, ".png", filepath.Join(dir, "in.png"), 256); err == nil {
		t.Fatal("Generate succeeded; want the timeout to kill the thumbnailer")
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("thumbnailer never forked its child: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("pid file %q: %v", data, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	// The orphan is reaped by init or a subreaper, so give it a moment to disappear.
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("forked child %d still running after the thumbnailer timed out", pid)
		}
		time.Sleep(20 * time.Millisecond)
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
