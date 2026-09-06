package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// initFields documents every config key for `config init`'s commented file, in the order they
// appear there.
var initFields = []struct {
	key     string
	comment string
	value   func(d Config) string
}{
	{"mountpoint", "Default mountpoint for mount and tray when none is given on the command line.", func(d Config) string { return strconv.Quote(d.Mountpoint) }},
	{"ttl", "How long a directory listing stays cached before it is fetched again.", func(d Config) string { return strconv.Quote(d.TTL) }},
	{"poll", "How often the event feed is polled for remote changes.", func(d Config) string { return strconv.Quote(d.Poll) }},
	{"op_timeout", "Deadline for one filesystem operation's network calls; a stuck operation returns an error after this instead of hanging.", func(d Config) string { return strconv.Quote(d.OpTimeout) }},
	{"cache_dir", "Where downloaded file blocks and persisted directory listings are stored on disk.", func(d Config) string { return strconv.Quote(d.CacheDir) }},
	{"cache_size", `Total size the on-disk cache (blocks and listings together) may use; "0" disables both.`, func(d Config) string { return strconv.Quote(d.CacheSize) }},
	{"large_file", `Files larger than this bypass the on-disk block cache; "0" disables the threshold.`, func(d Config) string { return strconv.Quote(d.LargeFile) }},
	{"thumbnails", "Write Proton's stored previews into the freedesktop thumbnail cache.", func(d Config) string { return strconv.FormatBool(d.Thumbnails) }},
	{"thumbnail_dir", "Freedesktop thumbnail cache directory.", func(d Config) string { return strconv.Quote(d.ThumbnailDir) }},
	{"deny_readers", "Process names refused a read of a file above large_file; empty allows all.", func(d Config) string { return QuoteArray(d.DenyReaders) }},
	{"max_uploads", "How many files upload at once; the rest wait in line.", func(d Config) string { return strconv.Itoa(d.MaxUploads) }},
	{"max_downloads", "How many file blocks download at once, across every open file.", func(d Config) string { return strconv.Itoa(d.MaxDownloads) }},
	{"log_level", "Log verbosity: debug, info, warn or error.", func(d Config) string { return strconv.Quote(d.LogLevel) }},
	{"log_stderr", "Force logging to stderr instead of the systemd journal.", func(d Config) string { return strconv.FormatBool(d.LogStderr) }},
	{"foreground", "Stay attached to the terminal instead of detaching into the background.", func(d Config) string { return strconv.FormatBool(d.Foreground) }},
	{"hv_method", "Force a human verification method at login: captcha, email or sms; empty tries email, sms, then captcha.", func(d Config) string { return strconv.Quote(d.HVMethod) }},
	{"no_browser", "Do not open a browser for human verification at login.", func(d Config) string { return strconv.FormatBool(d.NoBrowser) }},
}

// QuoteArray formats items as a TOML array of quoted strings, e.g. ["a", "b"].
func QuoteArray(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = strconv.Quote(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// Init writes a fully commented default config file to path: every key, its default value, and a
// one-line explanation, so a user starts from a real example instead of a blank file. It refuses
// to overwrite an existing file unless force is true.
func Init(path string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists (use -force to overwrite)", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	return os.WriteFile(path, []byte(defaultFileContents()), 0600)
}

// defaultFileContents renders every config key as a commented "# key = default" line with its
// explanation, so the file round-trips through Load unchanged (nothing is actually set) until the
// user uncomments a line.
func defaultFileContents() string {
	d := Defaults()

	var b strings.Builder
	b.WriteString("# proton-drive-fs configuration. Every key below is commented out at its default value;\n")
	b.WriteString("# uncomment and edit a line to override it. A command-line flag wins over this file.\n\n")

	for _, f := range initFields {
		b.WriteString("# " + f.comment + "\n")
		fmt.Fprintf(&b, "# %s = %s\n\n", f.key, f.value(d))
	}

	return b.String()
}
