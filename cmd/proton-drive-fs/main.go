// Command proton-drive-fs mounts Proton Drive as a local FUSE filesystem.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/auth"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/fusefs"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/thumbs"
)

var version = "dev"

func usage() {
	fmt.Fprintln(os.Stderr, "usage: proton-drive-fs <login|mount|unmount|tray|logout|version> [args]")
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}

	switch args[0] {
	case "login":
		return runLogin(args[1:])
	case "mount":
		return runMount(args[1:])
	case "unmount":
		return runUnmount(args[1:])
	case "tray":
		return runTray(args[1:])
	case "logout":
		return runLogout()
	case "version":
		return runVersion()
	default:
		usage()
		return 2
	}
}

func prompt(reader *bufio.Reader, label string) (string, error) {
	fmt.Print(label)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(line), nil
}

func runLogin(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	noBrowser := fs.Bool("no-browser", false, "do not open a browser for human verification")
	hvMethod := fs.String("hv-method", "", "force a human verification method: captcha, email or sms")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	reader := bufio.NewReader(os.Stdin)

	username, err := prompt(reader, "Username: ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: reading username:", err)
		return 1
	}
	if username == "" {
		fmt.Fprintln(os.Stderr, "error: username is required")
		return 2
	}

	fmt.Print("Password: ")
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: reading password:", err)
		return 1
	}

	promptTOTP := func() (string, error) {
		return prompt(reader, "Two-factor code: ")
	}

	ctx := context.Background()
	session, err := auth.Login(ctx, username, password, promptTOTP)

	var hv *auth.HumanVerificationRequired
	if errors.As(err, &hv) {
		session, err = verifyHuman(ctx, reader, hv, username, password, *hvMethod, !*noBrowser, promptTOTP)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error: login failed:", err)
		return 1
	}

	usedKeyring, err := session.Save()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: saving session:", err)
		return 1
	}
	if !usedKeyring {
		path, pathErr := auth.SessionPath()
		if pathErr != nil {
			path = "the session file"
		}
		fmt.Fprintf(os.Stderr, "warning: no OS keyring available (Secret Service/libsecret); key password stored in %s with mode 0600\n", path)
	}

	fmt.Printf("logged in as %s\n", username)
	return 0
}

// pickHVMethod returns the forced method when Proton offered it, otherwise a code by
// email or sms, and the CAPTCHA last because it needs a detour through a browser.
func pickHVMethod(offered []string, forced string) (string, error) {
	if forced != "" {
		if !slices.Contains(offered, forced) {
			return "", fmt.Errorf("proton did not offer verification method %q (offered: %s)", forced, strings.Join(offered, ", "))
		}
		return forced, nil
	}

	for _, method := range []string{"email", "sms", "captcha"} {
		if slices.Contains(offered, method) {
			return method, nil
		}
	}

	return "", fmt.Errorf("no supported verification method offered: %s", strings.Join(offered, ", "))
}

// verifyHuman walks the user through the verification Proton asked for and retries
// the login with the resulting token.
func verifyHuman(ctx context.Context, reader *bufio.Reader, hv *auth.HumanVerificationRequired, username string, password []byte, forced string, openBrowser bool, promptTOTP func() (string, error)) (*auth.Session, error) {
	fmt.Println("Proton requires human verification. Methods offered:", strings.Join(hv.Methods, ", "))

	method, err := pickHVMethod(hv.Methods, forced)
	if err != nil {
		return nil, err
	}
	fmt.Println("Verifying with:", method)

	if method == "captcha" {
		url := auth.CaptchaURL(hv.Token)

		fmt.Println("Proton requires a CAPTCHA. Solve it at:")
		fmt.Println(url)

		if openBrowser {
			_ = exec.Command("xdg-open", url).Start()
		}

		if _, err := prompt(reader, "Press Enter once the CAPTCHA is solved: "); err != nil {
			return nil, err
		}

		return auth.LoginWithHV(ctx, username, password, "captcha", hv.Token, promptTOTP)
	}

	label := "Email address for the verification code: "
	if method == "sms" {
		label = "Phone number for the verification code: "
	}

	destination, err := prompt(reader, label)
	if err != nil {
		return nil, err
	}
	if destination == "" {
		destination = username
	}

	if err := auth.RequestVerificationCode(ctx, username, method, destination); err != nil {
		return nil, err
	}

	code, err := prompt(reader, "Verification code: ")
	if err != nil {
		return nil, err
	}

	return auth.LoginWithHV(ctx, username, password, method, auth.FormatCodeToken(destination, code), promptTOTP)
}

// defaultCacheDir returns the default on-disk block cache directory, or "" if the user
// cache directory can't be determined (the -cache-dir flag can still override it).
func defaultCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "proton-drive-fs", "blocks")
}

// defaultThumbnailDir returns the freedesktop thumbnail cache directory, where file managers
// look for previews: $XDG_CACHE_HOME/thumbnails, falling back to ~/.cache/thumbnails.
func defaultThumbnailDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "thumbnails")
}

// defaultDenyReaders are the dedicated thumbnailer and indexer binaries refused a read of a
// large file. Only processes that walk a folder on their own are listed: an application the
// user launches to open a file, a slicer for example, must keep working.
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

// parseDenyReaders splits a comma-separated -deny-readers value, dropping blank entries. An
// empty value disables the denylist.
func parseDenyReaders(s string) []string {
	var names []string
	for _, name := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			names = append(names, trimmed)
		}
	}

	return names
}

// parseCacheSize parses a byte size like "512MiB", "2GiB", "100M", or a bare byte count.
// A result <= 0 disables the cache.
func parseCacheSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}

	units := []struct {
		suffix string
		mult   int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30},
	}

	for _, u := range units {
		if !strings.HasSuffix(s, u.suffix) {
			continue
		}
		val, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid size %q: %w", s, err)
		}
		return int64(val * float64(u.mult)), nil
	}

	val, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	return val, nil
}

func runMount(args []string) int {
	fs := flag.NewFlagSet("mount", flag.ContinueOnError)
	debug := fs.Bool("debug", false, "enable FUSE debug logging")
	ttl := fs.Duration("ttl", 30*time.Second, "directory listing cache TTL")
	poll := fs.Duration("poll", 10*time.Second, "remote change polling interval")
	opTimeout := fs.Duration("op-timeout", 60*time.Second, "deadline for one filesystem operation's network calls; a stuck operation returns an error after this instead of hanging")
	cacheDir := fs.String("cache-dir", defaultCacheDir(), "on-disk block cache directory")
	cacheSize := fs.String("cache-size", "1GiB", "on-disk block cache size limit (e.g. 512MiB, 2GiB); <=0 disables it")
	largeFile := fs.String("large-file", "300MiB", "files larger than this bypass the on-disk block cache; 0 disables")
	thumbnails := fs.Bool("thumbnails", true, "write Proton's stored previews into the freedesktop thumbnail cache")
	thumbnailDir := fs.String("thumbnail-dir", defaultThumbnailDir(), "freedesktop thumbnail cache directory")
	denyReaders := fs.String("deny-readers", strings.Join(defaultDenyReaders, ","), "comma-separated process names refused a read of a file above -large-file; empty allows all")
	foreground := fs.Bool("foreground", false, "stay attached to the terminal and log to stderr; used by the systemd unit")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: proton-drive-fs mount <mountpoint> [-debug] [-ttl 30s] [-poll 10s] [-op-timeout 60s] [-cache-dir path] [-cache-size 1GiB] [-large-file 300MiB] [-thumbnails] [-thumbnail-dir path] [-deny-readers names] [-foreground]")
		return 2
	}
	mountpoint := fs.Arg(0)

	stat, err := os.Stat(mountpoint)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "error: checking mountpoint: %v\n", err)
		return 1
	}
	if errors.Is(err, os.ErrNotExist) {
		fmt.Printf("mountpoint %s does not exist, creating it\n", mountpoint)
		if err := os.MkdirAll(mountpoint, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "error: creating mountpoint: %v\n", err)
			return 1
		}
	}
	if err == nil && !stat.IsDir() {
		fmt.Fprintf(os.Stderr, "error: mountpoint %s is not a directory\n", mountpoint)
		return 1
	}

	if !*foreground {
		return mountDetached(args, mountpoint)
	}

	cacheLimit, err := parseCacheSize(*cacheSize)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: invalid -cache-size:", err)
		return 2
	}

	largeFileLimit, err := parseCacheSize(*largeFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: invalid -large-file:", err)
		return 2
	}

	session, err := auth.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: no saved session, run \"proton-drive-fs login\" first:", err)
		return 1
	}

	api, keys, err := session.Client()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: restoring session:", err)
		return 1
	}
	defer api.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, root, err := drive.Open(ctx, api, keys)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: opening drive:", err)
		return 1
	}

	blockCache, err := drive.OpenBlockCache(*cacheDir, cacheLimit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: opening block cache:", err)
		return 1
	}
	client.SetBlockCache(blockCache, largeFileLimit)

	var thumbStore *thumbs.Store
	if *thumbnails && *thumbnailDir != "" {
		thumbStore, err = thumbs.New(*thumbnailDir, mountpoint)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: resolving thumbnail directory:", err)
			return 1
		}
	}

	fmt.Printf("mounting %s; unmount with: proton-drive-fs unmount %s\n", mountpoint, mountpoint)

	opts := fusefs.Options{
		Debug:        *debug,
		TTL:          *ttl,
		PollInterval: *poll,
		OpTimeout:    *opTimeout,
		Thumbnails:   thumbStore,
		DenyReaders:  parseDenyReaders(*denyReaders),
	}

	if err := fusefs.Mount(ctx, mountpoint, client, root, opts); err != nil {
		fmt.Fprintln(os.Stderr, "error: mount failed:", err)
		return 1
	}

	return 0
}

func detachedArgs(args []string) []string {
	childArgs := make([]string, 0, len(args)+2)
	childArgs = append(childArgs, "mount", "-foreground")
	childArgs = append(childArgs, args...)
	return childArgs
}

// mountDetached re-execs the current binary in the background with -foreground appended, waits
// for the mount to appear in /proc/self/mounts, and reports the result without blocking the
// caller's terminal.
func mountDetached(args []string, mountpoint string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: locating own executable:", err)
		return 1
	}

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: opening /dev/null:", err)
		return 1
	}
	defer func() { _ = devNull.Close() }()

	name := exe
	childArgs := detachedArgs(args)
	output := devNull
	logPath := ""

	// systemd-cat puts the daemon's output in the journal, where it is rotated and can be
	// followed per unit. Without it, keep appending to the log file.
	catPath, catErr := exec.LookPath("systemd-cat")
	if catErr == nil {
		name = catPath
		childArgs = append([]string{"-t", journalTag, "--level-prefix=false", exe}, childArgs...)
	}
	if catErr != nil {
		logPath, err = mountLogPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: preparing log file:", err)
			return 1
		}

		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: opening log file:", err)
			return 1
		}
		defer func() { _ = logFile.Close() }()
		output = logFile
	}

	cmd := exec.Command(name, childArgs...)
	cmd.Stdin = devNull
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "error: starting mount process:", err)
		return 1
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	absMountpoint, err := filepath.Abs(mountpoint)
	if err != nil {
		absMountpoint = mountpoint
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		mounts, err := os.ReadFile("/proc/self/mounts")
		if err == nil && mountedAt(string(mounts), absMountpoint) {
			if logPath == "" {
				fmt.Printf("mounted %s (pid %d, logs: journalctl --user -t %s); unmount with: proton-drive-fs unmount %s\n", mountpoint, cmd.Process.Pid, journalTag, mountpoint)
				return 0
			}
			fmt.Printf("mounted %s (pid %d, log %s); unmount with: proton-drive-fs unmount %s\n", mountpoint, cmd.Process.Pid, logPath, mountpoint)
			return 0
		}

		select {
		case <-exited:
			reportMountFailure("mount process exited", logPath)
			return 1
		default:
		}

		if time.Now().After(deadline) {
			reportMountFailure("timed out waiting for mount", logPath)
			return 1
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// journalTag is the syslog identifier the detached mount logs under.
const journalTag = "proton-drive-fs"

// reportMountFailure points at wherever the failed child's output went: the journal, or the
// log file with its last lines inlined.
func reportMountFailure(what string, logPath string) {
	if logPath == "" {
		fmt.Fprintf(os.Stderr, "error: %s; see: journalctl --user -t %s -n 50\n", what, journalTag)
		return
	}

	fmt.Fprintf(os.Stderr, "error: %s; see %s\n", what, logPath)
	printLastLines(logPath, 20)
}

// mountLogPath returns the daemon log path under $XDG_STATE_HOME/proton-drive-fs, falling back
// to ~/.local/state/proton-drive-fs, creating the directory (0700) if needed.
func mountLogPath() (string, error) {
	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		stateDir = filepath.Join(home, ".local", "state")
	}

	dir := filepath.Join(stateDir, "proton-drive-fs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	return filepath.Join(dir, "mount.log"), nil
}

// mountedAt reports whether procMounts (the contents of /proc/self/mounts) has an entry whose
// mountpoint field is mountpoint and whose fstype is fuse.proton-drive-fs. The kernel escapes
// space, tab, newline and backslash in the mountpoint field as octal (e.g. " " -> "\040"), so
// mountpoint is escaped the same way before comparing.
func mountedAt(procMounts string, mountpoint string) bool {
	escaped := escapeMountField(mountpoint)
	for _, line := range strings.Split(procMounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if fields[1] == escaped && fields[2] == "fuse.proton-drive-fs" {
			return true
		}
	}
	return false
}

func escapeMountField(s string) string {
	replacer := strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`)
	return replacer.Replace(s)
}

// printLastLines prints up to the last n lines of the file at path to stderr.
func printLastLines(path string, n int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
}

func runUnmount(args []string) int {
	fs := flag.NewFlagSet("unmount", flag.ContinueOnError)
	force := fs.Bool("force", false, "lazily unmount and abort the kernel FUSE connection; for a mount wedged by a dead or deadlocked daemon")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: proton-drive-fs unmount [-force] <mountpoint>")
		return 2
	}
	mountpoint := fs.Arg(0)

	if *force {
		return runUnmountForce(mountpoint)
	}

	cmd := exec.Command(fusermountBinary(), "-u", mountpoint)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error: unmount failed:", err)
		return 1
	}

	return 0
}

// fusermountBinary returns the fusermount helper to use: fusermount3 when it's on PATH (what
// libfuse3, and go-fuse, expect), else the older fusermount name.
func fusermountBinary() string {
	if _, err := exec.LookPath("fusermount3"); err == nil {
		return "fusermount3"
	}
	return "fusermount"
}

// runUnmountForce recovers a wedged mount: it lazily unmounts so blocked callers stop waiting on
// the mountpoint, then aborts the kernel-side FUSE connection so requests already stuck in the
// kernel (e.g. a file manager's threads) get errors instead of hanging forever. Either step
// succeeding counts as recovery, since the daemon may already be dead and one side moot.
func runUnmountForce(mountpoint string) int {
	absMountpoint, err := filepath.Abs(mountpoint)
	if err != nil {
		absMountpoint = mountpoint
	}

	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: reading /proc/self/mountinfo:", err)
	}
	minor, found := fuseConnectionID(string(mountinfo), absMountpoint)

	out, lazyErr := exec.Command(fusermountBinary(), "-u", "-z", mountpoint).CombinedOutput()
	if lazyErr != nil {
		fmt.Fprintf(os.Stderr, "error: lazy unmount %s: %v: %s\n", mountpoint, lazyErr, out)
	}

	aborted := false
	if found {
		aborted = abortFuseConnection(minor)
	}

	if lazyErr == nil || aborted {
		return 0
	}

	fmt.Fprintln(os.Stderr, "error: could not unmount or abort", mountpoint)
	return 1
}

// fuseConnectionID scans mountinfo (the contents of /proc/self/mountinfo) for the line whose
// mount point field (field 5, octal-escaped like /proc/self/mounts) is mountpoint and whose
// fstype starts with "fuse". It returns the minor number from that line's major:minor (field 3):
// the kernel names each FUSE connection's /sys/fs/fuse/connections/<id> directory by minor number.
func fuseConnectionID(mountinfo string, mountpoint string) (int, bool) {
	escaped := escapeMountField(mountpoint)
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[4] != escaped {
			continue
		}

		sep := slices.Index(fields, "-")
		if sep < 0 || sep+1 >= len(fields) || !strings.HasPrefix(fields[sep+1], "fuse") {
			continue
		}

		majorMinor := strings.SplitN(fields[2], ":", 2)
		if len(majorMinor) != 2 {
			continue
		}
		minor, err := strconv.Atoi(majorMinor[1])
		if err != nil {
			continue
		}

		return minor, true
	}

	return 0, false
}

// abortFuseConnection writes to /sys/fs/fuse/connections/<minor>/abort, which tells the kernel to
// fail every pending and future request on that FUSE connection instead of blocking forever. It
// reports whether the abort file exists and was written successfully.
func abortFuseConnection(minor int) bool {
	dir := fmt.Sprintf("/sys/fs/fuse/connections/%d", minor)
	abortPath := filepath.Join(dir, "abort")
	if _, err := os.Stat(abortPath); err != nil {
		return false
	}

	waiting := "?"
	if data, err := os.ReadFile(filepath.Join(dir, "waiting")); err == nil {
		waiting = strings.TrimSpace(string(data))
	}
	fmt.Printf("aborting FUSE connection %d (%s requests waiting)\n", minor, waiting)

	if err := os.WriteFile(abortPath, []byte("1"), 0o200); err != nil {
		fmt.Fprintln(os.Stderr, "error: aborting FUSE connection:", err)
		return false
	}

	return true
}

func runLogout() int {
	session, err := auth.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: no saved session:", err)
		return 1
	}

	if err := session.Logout(context.Background()); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "error: logout failed:", err)
		return 1
	}

	fmt.Println("logged out")
	return 0
}

func runVersion() int {
	fmt.Println(version)
	return 0
}
