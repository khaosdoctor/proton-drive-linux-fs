package config

import (
	"flag"
	"strings"
	"time"
)

// field describes one config key: the flag name cmd/proton-drive-fs registers it under, its TOML
// key, and how to write a flag's final parsed value into a Config.
type field struct {
	flagName string
	tomlKey  string
	set      func(cfg *Config, v any)
}

// fields is the one map between every config-backed flag name (mount, login and tray combined)
// and the Config field it drives. It only needs a flag.Getter's typed value, not a *flag.FlagSet
// instance, so any FlagSet that registers a flag under one of these names — whichever subcommand
// defines it — works with ApplyFlags unmodified.
var fields = []field{
	{"mountpoint", "mountpoint", func(cfg *Config, v any) { cfg.Mountpoint = v.(string) }},
	{"ttl", "ttl", func(cfg *Config, v any) { cfg.TTL = v.(time.Duration).String() }},
	{"poll", "poll", func(cfg *Config, v any) { cfg.Poll = v.(time.Duration).String() }},
	{"op-timeout", "op_timeout", func(cfg *Config, v any) { cfg.OpTimeout = v.(time.Duration).String() }},
	{"cache-dir", "cache_dir", func(cfg *Config, v any) { cfg.CacheDir = v.(string) }},
	{"cache-size", "cache_size", func(cfg *Config, v any) { cfg.CacheSize = v.(string) }},
	{"large-file", "large_file", func(cfg *Config, v any) { cfg.LargeFile = v.(string) }},
	{"thumbnails", "thumbnails", func(cfg *Config, v any) { cfg.Thumbnails = v.(bool) }},
	{"thumbnail-dir", "thumbnail_dir", func(cfg *Config, v any) { cfg.ThumbnailDir = v.(string) }},
	{"deny-readers", "deny_readers", func(cfg *Config, v any) { cfg.DenyReaders = SplitDenyReaders(v.(string)) }},
	{"max-uploads", "max_uploads", func(cfg *Config, v any) { cfg.MaxUploads = v.(int) }},
	{"max-downloads", "max_downloads", func(cfg *Config, v any) { cfg.MaxDownloads = v.(int) }},
	{"foreground", "foreground", func(cfg *Config, v any) { cfg.Foreground = v.(bool) }},
	{"log-level", "log_level", func(cfg *Config, v any) { cfg.LogLevel = v.(string) }},
	{"log-stderr", "log_stderr", func(cfg *Config, v any) { cfg.LogStderr = v.(bool) }},
	{"hv-method", "hv_method", func(cfg *Config, v any) { cfg.HVMethod = v.(string) }},
	{"no-browser", "no_browser", func(cfg *Config, v any) { cfg.NoBrowser = v.(bool) }},
}

// SplitDenyReaders splits a comma-separated -deny-readers value, dropping blank entries. An empty
// value disables the denylist. Shared by ApplyFlags and cmd/proton-drive-fs's own flag parsing so
// a config file's deny_readers array and the -deny-readers flag mean the same thing.
func SplitDenyReaders(s string) []string {
	var names []string
	for _, name := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	return names
}

// ApplyFlags overlays cfg with every flag in fs that was explicitly set on the command line,
// detected via fs.Visit. A flag left at its default is not touched here: every caller in
// cmd/proton-drive-fs seeds a flag's default from cfg itself before parsing, so an unset flag
// already carries the config file's (or the built-in default's) value — that is what gives the
// precedence defaults < config file < flags explicitly set. ApplyFlags returns which config keys
// (their TOML name) a flag actually changed, for "config show"'s per-value source.
func ApplyFlags(fs *flag.FlagSet, cfg *Config) map[string]bool {
	explicit := make(map[string]bool)

	fs.Visit(func(f *flag.Flag) {
		for _, fld := range fields {
			if fld.flagName != f.Name {
				continue
			}

			getter, ok := f.Value.(flag.Getter)
			if !ok {
				return
			}

			fld.set(cfg, getter.Get())
			explicit[fld.tomlKey] = true
			return
		}
	})

	return explicit
}
