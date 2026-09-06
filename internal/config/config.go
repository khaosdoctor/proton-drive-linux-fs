// Package config loads proton-drive-fs's TOML configuration file and its built-in defaults.
// cmd/proton-drive-fs layers command-line flags on top with ApplyFlags: defaults < config file <
// flags explicitly set on the command line.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Config is proton-drive-fs's full configuration: every mount, login and tray setting that can
// come from a TOML file. Durations and byte sizes are strings ("30s", "2GiB"), parsed the same way
// their flag counterparts are, so a config file and a command-line flag accept identical syntax.
type Config struct {
	// Mountpoint is the default mountpoint mount and tray use when none is given on the command
	// line (mount's positional argument, or tray's -mountpoint flag).
	Mountpoint string `toml:"mountpoint"`

	TTL          string   `toml:"ttl"`
	Poll         string   `toml:"poll"`
	OpTimeout    string   `toml:"op_timeout"`
	CacheDir     string   `toml:"cache_dir"`
	CacheSize    string   `toml:"cache_size"`
	LargeFile    string   `toml:"large_file"`
	Thumbnails   bool     `toml:"thumbnails"`
	ThumbnailDir string   `toml:"thumbnail_dir"`
	DenyReaders  []string `toml:"deny_readers"`
	MaxUploads   int      `toml:"max_uploads"`
	MaxDownloads int      `toml:"max_downloads"`
	LogLevel     string   `toml:"log_level"`
	LogStderr    bool     `toml:"log_stderr"`
	Foreground   bool     `toml:"foreground"`

	HVMethod  string `toml:"hv_method"`
	NoBrowser bool   `toml:"no_browser"`

	// fileKeys holds the TOML keys Load found present in the file, so a caller ("config show")
	// can tell a value that came from the file from one that is just Defaults() under the same
	// name. Never set directly; read it with FileKeys.
	fileKeys map[string]bool
}

// FileKeys reports which TOML keys were present in the file Load read. Empty, never nil, when no
// file was read (a missing file is not an error, see Load) or cfg was built without Load.
func (c Config) FileKeys() map[string]bool {
	if c.fileKeys == nil {
		return map[string]bool{}
	}
	return c.fileKeys
}

// defaultDenyReaders are the dedicated thumbnailer and indexer binaries refused a read of a large
// file. Only processes that walk a folder on their own are listed: an application the user
// launches to open a file, a slicer for example, must keep working.
var defaultDenyReaders = []string{
	"tracker-miner-fs",
	"tracker-extract",
	"localsearch",
	"baloo_file",
	"baloo_file_extractor",
	"tumblerd",
	"ffmpegthumbnailer",
	"totem-video-thumbnailer",
	"gdk-pixbuf-thumbnailer",
	"gnome-desktop-thumbnailer",
	"evince-thumbnailer",
}

// Defaults returns the built-in configuration: what proton-drive-fs uses when neither a config
// file nor a flag overrides a value. It is the single source of default values; flag definitions
// and the config file both start here so the two can never drift apart.
func Defaults() Config {
	return Config{
		TTL:          "30s",
		Poll:         "10s",
		OpTimeout:    "60s",
		CacheDir:     defaultCacheDir(),
		CacheSize:    "2GiB",
		LargeFile:    "300MiB",
		Thumbnails:   true,
		ThumbnailDir: defaultThumbnailDir(),
		DenyReaders:  append([]string(nil), defaultDenyReaders...),
		MaxUploads:   5,
		MaxDownloads: 8,
		LogLevel:     "info",
		fileKeys:     map[string]bool{},
	}
}

// defaultCacheDir returns the default on-disk cache root for blocks and persisted directory
// listings, or "" if the user cache directory can't be determined (cache_dir/-cache-dir can still
// override it). It deliberately does not include a "blocks" suffix the way the old block-only
// cache's default did: that suffix is now BlockCache's own blocks/ subdirectory, and a fresh
// install of this cache root happens to line up with where an old default install's blocks
// already are on disk (.../proton-drive-fs/blocks), so upgrading needs no migration for anyone who
// never set -cache-dir explicitly. See BlockCache's migrateLayout for the case that does.
func defaultCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "proton-drive-fs")
}

// defaultThumbnailDir returns the freedesktop thumbnail cache directory, where file managers look
// for previews: $XDG_CACHE_HOME/thumbnails, falling back to ~/.cache/thumbnails.
func defaultThumbnailDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "thumbnails")
}

// DefaultPath returns the default config file location, $XDG_CONFIG_HOME/proton-drive-fs/config.toml,
// falling back to ~/.config/proton-drive-fs/config.toml.
func DefaultPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "proton-drive-fs", "config.toml")
}

// Load reads and parses the TOML file at path, overlaying it onto Defaults(). A missing file is
// not an error: Load returns the defaults. A malformed file returns an error that names the line
// and column go-toml found the problem at.
func Load(path string) (Config, error) {
	cfg := Defaults()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}

	// Decoded twice: once into a bare map to record which keys the file actually set (FileKeys,
	// used by "config show" to report a value's source), once into cfg for the typed values.
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return cfg, decodeErr(path, err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, decodeErr(path, err)
	}

	keys := make(map[string]bool, len(raw))
	for k := range raw {
		keys[k] = true
	}
	cfg.fileKeys = keys

	return cfg, nil
}

// decodeErr wraps a go-toml decode error with path and, when go-toml can locate the problem
// (it cannot for every error, e.g. a field type mismatch found after parsing), the line and
// column it found the problem at.
func decodeErr(path string, err error) error {
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, col := de.Position()
		return fmt.Errorf("parsing %s at line %d, column %d: %w", path, row, col, err)
	}
	return fmt.Errorf("parsing %s: %w", path, err)
}
