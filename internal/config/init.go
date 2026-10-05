package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	{"mountpoint", lines(
		`Where the drive is mounted. This one is required and has no default.`,
		`Set it here so "proton-drive-fs mount" works without arguments, for example "/home/you/ProtonDrive".`,
	), func(d Config) string { return strconv.Quote(d.Mountpoint) }},
	{"ttl", lines(
		`How long a folder listing is reused before it is fetched again.`,
		`A higher value makes browsing big folders faster. Changes from other devices still arrive through poll.`,
		`A lower value means more network calls.`,
		`Warning: below "5s" a folder is fetched again almost every time you open it.`,
	), func(d Config) string { return strconv.Quote(d.TTL) }},
	{"poll", lines(
		`How often Proton is asked for changes made on other devices.`,
		`A higher value means less traffic, but a file added on your phone takes up to that long to show up here.`,
		`A lower value shows changes sooner.`,
		`Warning: below "5s" Proton may start rate limiting you.`,
	), func(d Config) string { return strconv.Quote(d.Poll) }},
	{"op_timeout", lines(
		`How long one operation may wait on the network before it fails. Big uploads get extra time on top of this.`,
		`A higher value means fewer failures on a slow connection, but a dead connection freezes the app for longer.`,
		`A lower value fails faster.`,
		`Warning: below "15s" normal operations on a slow connection also start failing.`,
	), func(d Config) string { return strconv.Quote(d.OpTimeout) }},
	{"cache_dir", lines(
		`Where downloaded file blocks and folder listings are kept on disk.`,
		`This is why reopening a file or folder after a remount does not download it again.`,
	), func(d Config) string { return strconv.Quote(d.CacheDir) }},
	{"cache_size", lines(
		`How much disk space the cache may use. "0" turns the disk cache off.`,
		`A higher value lets more files reopen without downloading.`,
		`A lower value uses less disk.`,
		`Warning: below "256MiB" files you reopen often get downloaded again.`,
	), func(d Config) string { return strconv.Quote(d.CacheSize) }},
	{"large_file", lines(
		`Files above this size skip the disk cache. "0" caches files of any size.`,
		`This is so large files you download don't evict the entire cache in one go.`,
		`A higher value caches bigger files too, but each one pushes out many small ones.`,
		`A lower value caches only small files.`,
		`Warning: keep it well below cache_size.`,
	), func(d Config) string { return strconv.Quote(d.LargeFile) }},
	{"thumbnails", lines(
		`Write file previews where your file manager looks for them.`,
		`Photos and models then show thumbnails without being downloaded.`,
	), func(d Config) string { return strconv.FormatBool(d.Thumbnails) }},
	{"thumbnail_dir", lines(
		`Where those previews go.`,
		`Only change it if your file manager reads thumbnails from somewhere else.`,
	), func(d Config) string { return strconv.Quote(d.ThumbnailDir) }},
	{"deny_readers", lines(
		`Programs that are not allowed to read files bigger than large_file. An empty list allows everyone.`,
		`Indexers and thumbnailers open every file they find, which would download every big file on the drive.`,
	), func(d Config) string { return QuoteArray(d.DenyReaders) }},
	{"exclude", lines(
		`File names that are hidden from the mount and never uploaded, such as editor swap files ("*.swp") or "Thumbs.db".`,
		`Patterns are globs. Start one with "re:" to use a regular expression instead.`,
		`An app can still create a file with one of these names. It only uploads if the app renames it to a name not on this list.`,
	), func(d Config) string { return QuoteArray(d.Exclude) }},
	{"upload_delay", lines(
		`How long to wait after a file closes before uploading it, so the app can finish saving first. "0s" uploads right away.`,
		`For example, FreeCAD saves to "model.FCStd.<uuid>" and then renames it to "model.FCStd". Without the wait, the temporary name gets uploaded.`,
		`A higher value is safer for apps with slow saves, but a fresh save only exists on this machine until it uploads.`,
		`A lower value gets saves to Proton sooner.`,
		`Warning: above "1m" a crash before the upload loses that save.`,
		`Warning: below "2s" the app may still be saving when the upload starts.`,
	), func(d Config) string { return strconv.Quote(d.UploadDelay) }},
	{"upload_delays", lines(
		`A different upload_delay for files matching a pattern, written as "pattern=duration". The first pattern that matches is used.`,
		`For example, ["*.bak=1m"] for an app that rewrites its backups every few seconds, so only the last one uploads.`,
		`Files in exclude are never uploaded, whatever this says.`,
	), func(d Config) string { return QuoteArray(d.UploadDelays) }},
	{"max_uploads", lines(
		`How many files upload at the same time. 0 or less removes the limit.`,
		`A higher value copies big folders in faster.`,
		`A lower value is lighter on your network, but bulk copies take longer.`,
		`Warning: above 10 Proton may start rate limiting you. Proton's own apps use 5.`,
	), func(d Config) string { return strconv.Itoa(d.MaxUploads) }},
	{"max_downloads", lines(
		`How many file pieces download at the same time, counting every open file. 0 or less removes the limit.`,
		`A higher value opens big files faster on a fast connection.`,
		`A lower value is lighter on your network.`,
		`Warning: above 32 Proton may start rate limiting you.`,
		`Warning: below 4 big files are slow to open.`,
	), func(d Config) string { return strconv.Itoa(d.MaxDownloads) }},
	{"log_level", `Log verbosity: debug, info, warn or error.`, func(d Config) string { return strconv.Quote(d.LogLevel) }},
	{"log_stderr", `Log to the terminal instead of the systemd journal.`, func(d Config) string { return strconv.FormatBool(d.LogStderr) }},
	{"foreground", lines(
		`Keep mount attached to the terminal instead of moving to the background.`,
		`The systemd unit turns this on itself, so leave it off here.`,
	), func(d Config) string { return strconv.FormatBool(d.Foreground) }},
	{"hv_method", lines(
		`Which human verification to use when Proton asks for one at login: captcha, email or sms.`,
		`When empty, it tries email first, then sms, then captcha.`,
	), func(d Config) string { return strconv.Quote(d.HVMethod) }},
	{"no_browser", lines(
		`Do not open a browser for human verification at login.`,
		`Useful when logging in over SSH or on a machine without a desktop.`,
	), func(d Config) string { return strconv.FormatBool(d.NoBrowser) }},
}

// lines joins a key's explanation, one sentence per config file comment line.
func lines(s ...string) string {
	return strings.Join(s, "\n")
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

// LoadOrInit is Load with one extra step: when no file exists at path, it first writes the
// commented default file Init writes, so a fresh install always has a config.toml to edit instead
// of an empty config directory. Every command that reads configuration goes through this. A write
// failure (a read-only home, an unwritable XDG_CONFIG_HOME) is deliberately ignored: the built-in
// defaults still load and the command still runs.
func LoadOrInit(path string) (Config, error) {
	if path != "" {
		_ = Init(path, false)
		_, _, _ = Upgrade(path)
	}
	return Load(path)
}

// commentedKey matches a commented-out "# key = value" line as Init writes it, capturing the key.
var commentedKey = regexp.MustCompile(`^#[ \t]*([a-z_]+)[ \t]*=`)

// Upgrade keeps the file at path in step with initFields, so a file written by an older version
// follows the keys added or removed since. A documented key the file neither sets nor lists as a
// commented default is appended in the form Init writes; a commented default for a key initFields
// no longer has is dropped, together with the explanation line above it. A key the user set is
// never changed or removed, even one that is no longer documented. The package upgrade hooks
// restart the daemon, and that restart goes through LoadOrInit, so an upgrade brings every user's
// file up to date without the installer touching it.
func Upgrade(path string) (added, removed []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	known := make(map[string]bool, len(initFields))
	for _, f := range initFields {
		known[f.key] = true
	}

	lines := strings.SplitAfter(string(data), "\n")
	kept := make([]string, 0, len(lines))
	skipBlank := false
	for _, line := range lines {
		if skipBlank && strings.TrimSpace(line) == "" {
			skipBlank = false
			continue
		}
		skipBlank = false

		m := commentedKey.FindStringSubmatch(line)
		if m == nil || known[m[1]] {
			kept = append(kept, line)
			continue
		}

		removed = append(removed, m[1])
		// Drop its explanation too: the comment lines right above it, up to the blank line before.
		for n := len(kept); n > 0 && strings.HasPrefix(kept[n-1], "#") && !commentedKey.MatchString(kept[n-1]); n = len(kept) {
			kept = kept[:n-1]
		}
		skipBlank = true
	}
	text := strings.Join(kept, "")

	d := Defaults()
	var b strings.Builder
	for _, f := range initFields {
		present := regexp.MustCompile(`(?m)^[ \t]*#?[ \t]*` + regexp.QuoteMeta(f.key) + `[ \t]*=`)
		if present.MatchString(text) {
			continue
		}
		added = append(added, f.key)
		writeBlock(&b, f.key, f.comment, f.value(d))
	}

	if len(added) == 0 && len(removed) == 0 {
		return nil, nil, nil
	}

	// Keep the one-blank-line spacing between blocks that Init writes.
	if len(added) > 0 && text != "" {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if !strings.HasSuffix(text, "\n\n") {
			text += "\n"
		}
	}
	text += b.String()

	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	// Write beside the file and rename over it, so a crash mid-write never leaves half a config.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), info.Mode().Perm()); err != nil {
		return nil, nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, nil, err
	}
	return added, removed, nil
}

// defaultFileContents renders every config key as a commented "# key = default" line with its
// explanation, so the file round-trips through Load unchanged (nothing is actually set) until the
// user uncomments a line.
func defaultFileContents() string {
	d := Defaults()

	var b strings.Builder
	b.WriteString("# proton-drive-fs configuration. Every key below is commented out at its default value;\n")
	b.WriteString("# uncomment and edit a line to override it. A command-line flag overrides this file.\n\n")

	for _, f := range initFields {
		writeBlock(&b, f.key, f.comment, f.value(d))
	}

	return b.String()
}

// writeBlock writes one key's block: each line of comment as its own "# " line, a "# ----"
// separator, then the key commented out at value, then a blank line.
func writeBlock(b *strings.Builder, key, comment, value string) {
	for _, line := range strings.Split(comment, "\n") {
		b.WriteString("# " + line + "\n")
	}
	b.WriteString("# ----\n")
	fmt.Fprintf(b, "# %s = %s\n\n", key, value)
}
