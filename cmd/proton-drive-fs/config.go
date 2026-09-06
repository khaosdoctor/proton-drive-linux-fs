package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/config"
)

// resolveConfigPath finds the -config path from args without fully parsing them: the config file
// has to be loaded before the rest of a subcommand's flags are defined, since their defaults come
// from it (see registerMountConfigFlags), which is one step before the real -config flag,
// registered alongside them below, ever gets parsed. Falls back to the default location.
func resolveConfigPath(args []string) string {
	for i, a := range args {
		switch {
		case a == "-config" || a == "--config":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "-config="):
			return strings.TrimPrefix(a, "-config=")
		case strings.HasPrefix(a, "--config="):
			return strings.TrimPrefix(a, "--config=")
		}
	}
	return config.DefaultPath()
}

// sizeValue is a flag.Value for a byte-size string like "2GiB" or "0": Set runs it through
// parseCacheSize, so a malformed -cache-size/-large-file fails fs.Parse itself, the same way a
// malformed -ttl fails fs.Duration's own parsing, instead of surfacing only once mount actually
// tries to open the cache.
type sizeValue string

func (s *sizeValue) String() string { return string(*s) }

// Get implements flag.Getter (in addition to flag.Value) as a plain string, so
// config.ApplyFlags's flag.Getter type assertion still works for cache-size/large-file exactly
// like every other flag.
func (s *sizeValue) Get() any { return string(*s) }

func (s *sizeValue) Set(v string) error {
	if _, err := parseCacheSize(v); err != nil {
		return err
	}
	*s = sizeValue(v)
	return nil
}

// mountConfigFlags holds fs.Parse's results for every config-backed mount flag (everything mount
// takes except -debug, which has no config key).
type mountConfigFlags struct {
	ttl, poll, opTimeout     *time.Duration
	cacheDir                 *string
	cacheSize, largeFile     *sizeValue
	thumbnails               *bool
	thumbnailDir             *string
	denyReaders              *string
	maxUploads, maxDownloads *int
	foreground               *bool
	logLevel                 *string
	logStderr                *bool
}

// registerMountConfigFlags defines every config-backed mount flag on fs, defaulted from cfg (the
// config file merged over Defaults()), so a flag left unset on the command line keeps the config
// file's value and one that is set overrides it — flag.FlagSet's own default handling does the
// precedence work; applyFlags (via config.ApplyFlags) only needs it afterward to report where
// each value came from. Shared by runMount and "config show", which previews the same resolution
// without mounting. Every default is validated here (durations via time.ParseDuration, sizes via
// parseCacheSize) so a malformed config file value errors out immediately in both callers, rather
// than deferring the failure to whatever later in runMount happens to parse that field again.
func registerMountConfigFlags(fs *flag.FlagSet, cfg config.Config) (*mountConfigFlags, error) {
	ttlDefault, err := time.ParseDuration(cfg.TTL)
	if err != nil {
		return nil, fmt.Errorf("config ttl: %w", err)
	}
	pollDefault, err := time.ParseDuration(cfg.Poll)
	if err != nil {
		return nil, fmt.Errorf("config poll: %w", err)
	}
	opTimeoutDefault, err := time.ParseDuration(cfg.OpTimeout)
	if err != nil {
		return nil, fmt.Errorf("config op_timeout: %w", err)
	}
	if _, err := parseCacheSize(cfg.CacheSize); err != nil {
		return nil, fmt.Errorf("config cache_size: %w", err)
	}
	if _, err := parseCacheSize(cfg.LargeFile); err != nil {
		return nil, fmt.Errorf("config large_file: %w", err)
	}

	cacheSize := sizeValue(cfg.CacheSize)
	largeFile := sizeValue(cfg.LargeFile)
	fs.Var(&cacheSize, "cache-size", "on-disk cache size limit shared by blocks and listings (e.g. 512MiB, 2GiB); <=0 disables both")
	fs.Var(&largeFile, "large-file", "files larger than this bypass the on-disk block cache; 0 disables")

	return &mountConfigFlags{
		ttl:          fs.Duration("ttl", ttlDefault, "directory listing cache TTL"),
		poll:         fs.Duration("poll", pollDefault, "remote change polling interval"),
		opTimeout:    fs.Duration("op-timeout", opTimeoutDefault, "deadline for one filesystem operation's network calls; a stuck operation returns an error after this instead of hanging"),
		cacheDir:     fs.String("cache-dir", cfg.CacheDir, "on-disk cache directory for blocks and persisted directory listings"),
		cacheSize:    &cacheSize,
		largeFile:    &largeFile,
		thumbnails:   fs.Bool("thumbnails", cfg.Thumbnails, "write Proton's stored previews into the freedesktop thumbnail cache"),
		thumbnailDir: fs.String("thumbnail-dir", cfg.ThumbnailDir, "freedesktop thumbnail cache directory"),
		denyReaders:  fs.String("deny-readers", strings.Join(cfg.DenyReaders, ","), "comma-separated process names refused a read of a file above -large-file; empty allows all"),
		maxUploads:   fs.Int("max-uploads", cfg.MaxUploads, "how many files upload at once; the rest wait in line"),
		maxDownloads: fs.Int("max-downloads", cfg.MaxDownloads, "how many file blocks download at once"),
		foreground:   fs.Bool("foreground", cfg.Foreground, "stay attached to the terminal instead of detaching into the background; used by the systemd unit"),
		logLevel:     fs.String("log-level", cfg.LogLevel, "log verbosity: debug, info, warn or error"),
		logStderr:    fs.Bool("log-stderr", cfg.LogStderr, "force logging to stderr instead of the systemd journal; useful with -foreground"),
	}, nil
}

// loginConfigFlags holds fs.Parse's results for every config-backed login flag.
type loginConfigFlags struct {
	noBrowser *bool
	hvMethod  *string
}

// registerLoginConfigFlags defines every config-backed login flag on fs, defaulted from cfg. See
// registerMountConfigFlags.
func registerLoginConfigFlags(fs *flag.FlagSet, cfg config.Config) *loginConfigFlags {
	return &loginConfigFlags{
		noBrowser: fs.Bool("no-browser", cfg.NoBrowser, "do not open a browser for human verification"),
		hvMethod:  fs.String("hv-method", cfg.HVMethod, "force a human verification method: captcha, email or sms"),
	}
}

func runConfigCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: proton-drive-fs config <init|show> [args]")
		return 2
	}

	switch args[0] {
	case "init":
		return runConfigInit(args[1:])
	case "show":
		return runConfigShow(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "usage: proton-drive-fs config <init|show> [args]")
		return 2
	}
}

func runConfigInit(args []string) int {
	fs := flag.NewFlagSet("config init", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to write (default: $XDG_CONFIG_HOME/proton-drive-fs/config.toml)")
	force := fs.Bool("force", false, "overwrite an existing file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path := *configPath
	if path == "" {
		path = config.DefaultPath()
	}

	if err := config.Init(path, *force); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	fmt.Println("wrote", path)
	return 0
}

// runConfigShow prints the effective configuration: every key, its value after merging built-in
// defaults, the config file, and any flag passed to this same "config show" invocation, and where
// that value came from. It registers the full set of mount and login flags so it can preview
// exactly what those subcommands would resolve to, without mounting or logging in.
func runConfigShow(args []string) int {
	configPath := resolveConfigPath(args)
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: loading config:", err)
		return 1
	}

	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	fs.String("config", configPath, "path to config.toml")
	fs.String("mountpoint", cfg.Mountpoint, "default mountpoint for mount and tray")
	if _, err := registerMountConfigFlags(fs, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	registerLoginConfigFlags(fs, cfg)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	explicit := config.ApplyFlags(fs, &cfg)
	printConfig(cfg, explicit)
	return 0
}

// printConfig prints cfg as TOML with a trailing comment on each line naming where that value came
// from: a flag explicitly passed to this invocation, the config file, or the built-in default.
func printConfig(cfg config.Config, explicit map[string]bool) {
	fileKeys := cfg.FileKeys()

	source := func(key string) string {
		switch {
		case explicit[key]:
			return "flag"
		case fileKeys[key]:
			return "file"
		default:
			return "default"
		}
	}

	line := func(key, value string) {
		fmt.Printf("%s = %s # %s\n", key, value, source(key))
	}

	line("mountpoint", strconv.Quote(cfg.Mountpoint))
	line("ttl", strconv.Quote(cfg.TTL))
	line("poll", strconv.Quote(cfg.Poll))
	line("op_timeout", strconv.Quote(cfg.OpTimeout))
	line("cache_dir", strconv.Quote(cfg.CacheDir))
	line("cache_size", strconv.Quote(cfg.CacheSize))
	line("large_file", strconv.Quote(cfg.LargeFile))
	line("thumbnails", strconv.FormatBool(cfg.Thumbnails))
	line("thumbnail_dir", strconv.Quote(cfg.ThumbnailDir))
	line("deny_readers", config.QuoteArray(cfg.DenyReaders))
	line("max_uploads", strconv.Itoa(cfg.MaxUploads))
	line("max_downloads", strconv.Itoa(cfg.MaxDownloads))
	line("log_level", strconv.Quote(cfg.LogLevel))
	line("log_stderr", strconv.FormatBool(cfg.LogStderr))
	line("foreground", strconv.FormatBool(cfg.Foreground))
	line("hv_method", strconv.Quote(cfg.HVMethod))
	line("no_browser", strconv.FormatBool(cfg.NoBrowser))
}
