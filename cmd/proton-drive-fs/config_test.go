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
