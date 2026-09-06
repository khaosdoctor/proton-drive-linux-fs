package config

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	d := Defaults()

	if d.TTL != "30s" || d.Poll != "10s" || d.OpTimeout != "60s" {
		t.Fatalf("unexpected duration defaults: %+v", d)
	}
	if d.CacheSize != "2GiB" {
		t.Fatalf("CacheSize = %q, want 2GiB", d.CacheSize)
	}
	if d.LargeFile != "300MiB" {
		t.Fatalf("LargeFile = %q, want 300MiB", d.LargeFile)
	}
	if !d.Thumbnails {
		t.Fatal("Thumbnails should default to true")
	}
	if d.MaxUploads != 5 || d.MaxDownloads != 8 {
		t.Fatalf("unexpected upload/download defaults: %+v", d)
	}
	if d.LogLevel != "info" {
		t.Fatalf("LogLevel = %q, want info", d.LogLevel)
	}
	if len(d.DenyReaders) == 0 {
		t.Fatal("DenyReaders should default to a non-empty list")
	}
	if len(d.FileKeys()) != 0 {
		t.Fatalf("FileKeys() = %v, want empty for a Config built without Load", d.FileKeys())
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatal(err)
	}

	want := Defaults()
	want.fileKeys = cfg.fileKeys // both empty maps; avoid nil-vs-empty noise below
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("cfg = %+v, want defaults %+v", cfg, want)
	}
}

func TestLoadFileOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := `
ttl = "5s"
max_uploads = 42
thumbnails = false
deny_readers = ["custom-thumbnailer"]
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.TTL != "5s" {
		t.Errorf("TTL = %q, want 5s", cfg.TTL)
	}
	if cfg.MaxUploads != 42 {
		t.Errorf("MaxUploads = %d, want 42", cfg.MaxUploads)
	}
	if cfg.Thumbnails {
		t.Error("Thumbnails should be false, overridden by the file")
	}
	if !reflect.DeepEqual(cfg.DenyReaders, []string{"custom-thumbnailer"}) {
		t.Errorf("DenyReaders = %v, want [custom-thumbnailer]", cfg.DenyReaders)
	}

	// Untouched keys keep their built-in default.
	if cfg.Poll != "10s" {
		t.Errorf("Poll = %q, want the default 10s", cfg.Poll)
	}

	for _, key := range []string{"ttl", "max_uploads", "thumbnails", "deny_readers"} {
		if !cfg.FileKeys()[key] {
			t.Errorf("FileKeys()[%q] = false, want true", key)
		}
	}
	if cfg.FileKeys()["poll"] {
		t.Error("FileKeys()[\"poll\"] should be false: the file never set it")
	}
}

func TestLoadMalformedFileReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("ttl = \n\nthis is not valid toml [[[\n"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected a parse error for malformed TOML")
	}
	if !strings.Contains(err.Error(), "line") {
		t.Errorf("error = %q, want it to name the line the problem is at", err.Error())
	}
}

func TestApplyFlagsOverridesOnlyExplicit(t *testing.T) {
	cfg := Defaults()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Duration("ttl", 30*time.Second, "")
	fs.String("cache-dir", cfg.CacheDir, "")
	fs.Bool("thumbnails", cfg.Thumbnails, "")

	if err := fs.Parse([]string{"-ttl", "5s"}); err != nil {
		t.Fatal(err)
	}

	explicit := ApplyFlags(fs, &cfg)

	if cfg.TTL != "5s" {
		t.Errorf("TTL = %q, want 5s", cfg.TTL)
	}
	if !explicit["ttl"] {
		t.Error(`explicit["ttl"] = false, want true: -ttl was passed on the command line`)
	}
	if explicit["cache_dir"] {
		t.Error(`explicit["cache_dir"] = true, want false: -cache-dir was never passed`)
	}
	if cfg.CacheDir != Defaults().CacheDir {
		t.Errorf("CacheDir changed even though -cache-dir was never set: %q", cfg.CacheDir)
	}
}

func TestInitOutputRoundTripsThroughLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml")

	if err := Init(path, false); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("loading config init's own output failed: %v", err)
	}

	want := Defaults()
	want.fileKeys = cfg.fileKeys
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("cfg = %+v, want the defaults unchanged (every line in the init file is commented out): %+v", cfg, want)
	}
	if len(cfg.FileKeys()) != 0 {
		t.Errorf("FileKeys() = %v, want empty: every line in the init file is commented out", cfg.FileKeys())
	}

	if err := Init(path, false); err == nil {
		t.Fatal("expected Init to refuse to overwrite an existing file without -force")
	}
	if err := Init(path, true); err != nil {
		t.Fatalf("Init with force=true should overwrite: %v", err)
	}
}
