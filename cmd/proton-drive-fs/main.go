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
)

var version = "dev"

func usage() {
	fmt.Fprintln(os.Stderr, "usage: proton-drive-fs <login|mount|unmount|logout|version> [args]")
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
	cacheDir := fs.String("cache-dir", defaultCacheDir(), "on-disk block cache directory")
	cacheSize := fs.String("cache-size", "1GiB", "on-disk block cache size limit (e.g. 512MiB, 2GiB); <=0 disables it")
	foreground := fs.Bool("foreground", false, "stay attached to the terminal and log to stderr; used by the systemd unit")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: proton-drive-fs mount <mountpoint> [-debug] [-ttl 30s] [-poll 10s] [-cache-dir path] [-cache-size 1GiB] [-foreground]")
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
	client.SetBlockCache(blockCache)

	fmt.Printf("mounting %s; unmount with: proton-drive-fs unmount %s\n", mountpoint, mountpoint)

	if err := fusefs.Mount(ctx, mountpoint, client, root, fusefs.Options{Debug: *debug, TTL: *ttl, PollInterval: *poll}); err != nil {
		fmt.Fprintln(os.Stderr, "error: mount failed:", err)
		return 1
	}

	return 0
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

	logPath, err := mountLogPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: preparing log file:", err)
		return 1
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: opening log file:", err)
		return 1
	}
	defer logFile.Close()

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: opening /dev/null:", err)
		return 1
	}
	defer devNull.Close()

	childArgs := make([]string, 0, len(args)+2)
	childArgs = append(childArgs, "mount")
	childArgs = append(childArgs, args...)
	childArgs = append(childArgs, "-foreground")

	cmd := exec.Command(exe, childArgs...)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
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
			fmt.Printf("mounted %s (pid %d, log %s); unmount with: proton-drive-fs unmount %s\n", mountpoint, cmd.Process.Pid, logPath, mountpoint)
			return 0
		}

		select {
		case <-exited:
			fmt.Fprintf(os.Stderr, "error: mount process exited; see %s\n", logPath)
			printLastLines(logPath, 20)
			return 1
		default:
		}

		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "error: timed out waiting for mount; see %s\n", logPath)
			printLastLines(logPath, 20)
			return 1
		}

		time.Sleep(200 * time.Millisecond)
	}
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
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: proton-drive-fs unmount <mountpoint>")
		return 2
	}
	mountpoint := args[0]

	bin := "fusermount3"
	if _, err := exec.LookPath(bin); err != nil {
		bin = "fusermount"
	}

	cmd := exec.Command(bin, "-u", mountpoint)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error: unmount failed:", err)
		return 1
	}

	return 0
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
