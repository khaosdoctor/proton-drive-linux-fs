package main

import (
	"flag"
	"testing"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/config"
)

// TestRegisterMountConfigFlagsRejectsMalformedCacheSize checks a malformed cache_size in the
// config file errors out at flag-registration time - the point both runMount and "config show"
// share - instead of only surfacing once mount itself gets around to parsing it.
func TestRegisterMountConfigFlagsRejectsMalformedCacheSize(t *testing.T) {
	cfg := config.Defaults()
	cfg.CacheSize = "not-a-size"

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	if _, err := registerMountConfigFlags(fs, cfg); err == nil {
		t.Fatal("expected an error for a malformed cache_size")
	}
}

func TestRegisterMountConfigFlagsRejectsMalformedLargeFile(t *testing.T) {
	cfg := config.Defaults()
	cfg.LargeFile = "not-a-size"

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	if _, err := registerMountConfigFlags(fs, cfg); err == nil {
		t.Fatal("expected an error for a malformed large_file")
	}
}

// TestRegisterMountConfigFlagsAcceptsValidSizes checks a well-formed config does not regress.
func TestRegisterMountConfigFlagsAcceptsValidSizes(t *testing.T) {
	cfg := config.Defaults()
	cfg.CacheSize = "512MiB"
	cfg.LargeFile = "0"

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	mf, err := registerMountConfigFlags(fs, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if mf.cacheSize.String() != "512MiB" || mf.largeFile.String() != "0" {
		t.Errorf("cacheSize=%q largeFile=%q, want 512MiB/0", mf.cacheSize.String(), mf.largeFile.String())
	}
}

// TestCacheSizeFlagRejectsMalformedCLIValue checks fs.Parse itself fails on a bad -cache-size, the
// same way it already fails on a bad -ttl, instead of deferring the failure to mount's own
// post-parse parseCacheSize call.
func TestCacheSizeFlagRejectsMalformedCLIValue(t *testing.T) {
	cfg := config.Defaults()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	if _, err := registerMountConfigFlags(fs, cfg); err != nil {
		t.Fatal(err)
	}

	if err := fs.Parse([]string{"-cache-size", "garbage"}); err == nil {
		t.Fatal("expected fs.Parse to reject a malformed -cache-size")
	}
}

// TestResolveMountpoint covers every combination resolveMountpoint has to arbitrate between: an
// argument, a config file value, both, and neither. There is no built-in default (see
// config.Defaults), so "neither" must fail rather than fall back to an invented path.
func TestResolveMountpoint(t *testing.T) {
	tests := []struct {
		name    string
		arg     string
		cfgMP   string
		want    string
		wantErr bool
	}{
		{"arg only", "/from/arg", "", "/from/arg", false},
		{"config only", "", "/from/config", "/from/config", false},
		{"both, arg wins", "/from/arg", "/from/config", "/from/arg", false},
		{"neither is an error", "", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Mountpoint = tt.cfgMP

			got, err := resolveMountpoint(tt.arg, cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveMountpoint(%q, cfg with Mountpoint=%q) = %q, nil; want an error", tt.arg, tt.cfgMP, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveMountpoint(%q, cfg with Mountpoint=%q) unexpected error: %v", tt.arg, tt.cfgMP, err)
			}
			if got != tt.want {
				t.Errorf("resolveMountpoint(%q, cfg with Mountpoint=%q) = %q, want %q", tt.arg, tt.cfgMP, got, tt.want)
			}
		})
	}
}

// TestNoMountpointError checks the exact wording and that the config path is interpolated into
// it, since a caller (mount, unmount, status) matches nothing else against this string.
func TestNoMountpointError(t *testing.T) {
	got := noMountpointError("/home/u/.config/proton-drive-fs/config.toml")
	want := "error: no mountpoint given; pass one as an argument or set mountpoint in /home/u/.config/proton-drive-fs/config.toml"
	if got != want {
		t.Errorf("noMountpointError() = %q, want %q", got, want)
	}
}
