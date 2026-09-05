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
		return runLogin()
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

func runLogin() int {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Username: ")
	username, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: reading username:", err)
		return 1
	}
	username = strings.TrimSpace(username)
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
		fmt.Print("Two-factor code: ")
		code, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(code), nil
	}

	ctx := context.Background()
	session, err := auth.Login(ctx, username, password, promptTOTP)
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

	api, addrKR, err := session.Client()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: restoring session:", err)
		return 1
	}
	defer api.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, root, err := drive.Open(ctx, api, addrKR)
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
