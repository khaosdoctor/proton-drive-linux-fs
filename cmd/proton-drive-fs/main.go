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
	"slices"
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

	if err := session.Save(); err != nil {
		fmt.Fprintln(os.Stderr, "error: saving session:", err)
		return 1
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
		captchaURL := auth.CaptchaURL(hv.Token)

		fmt.Println("Proton refuses to be embedded in a local page, so the CAPTCHA has to run in a browser tab.")
		fmt.Println("Open this URL to complete the CAPTCHA:", captchaURL)

		if openBrowser {
			_ = exec.Command("xdg-open", captchaURL).Start()
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

func runMount(args []string) int {
	fs := flag.NewFlagSet("mount", flag.ContinueOnError)
	debug := fs.Bool("debug", false, "enable FUSE debug logging")
	ttl := fs.Duration("ttl", 30*time.Second, "directory listing cache TTL")
	poll := fs.Duration("poll", 10*time.Second, "remote change polling interval")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: proton-drive-fs mount <mountpoint> [-debug] [-ttl 30s] [-poll 10s]")
		return 2
	}
	mountpoint := fs.Arg(0)

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

	fmt.Printf("mounting %s; unmount with: proton-drive-fs unmount %s\n", mountpoint, mountpoint)

	if err := fusefs.Mount(ctx, mountpoint, client, root, fusefs.Options{Debug: *debug, TTL: *ttl, PollInterval: *poll}); err != nil {
		fmt.Fprintln(os.Stderr, "error: mount failed:", err)
		return 1
	}

	return 0
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
