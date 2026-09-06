package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/about"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/api"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/auth"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/state"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/tray"
)

// commit is this build's short git commit, set via -X main.commit=... in the Makefile and
// .goreleaser.yaml, next to the existing "dev" version var in main.go.
var commit = "none"

// apiDialTimeout bounds how long "status" waits for the local API's unix socket to answer
// before falling back to the on-disk status.json snapshot.
const apiDialTimeout = 500 * time.Millisecond

// statusFromAPIOrFile prefers the local API, which answers with a live snapshot straight from
// the running daemon, and falls back to the status.json file (stale after state.StaleAfter) when
// no daemon is listening on the socket, e.g. an older build without the API.
func statusFromAPIOrFile() (state.Status, bool) {
	if st, ok := statusFromAPI(); ok {
		return st, true
	}
	st, ok := state.ReadStatus()
	return st, ok && st.Fresh()
}

func statusFromAPI() (state.Status, bool) {
	client, err := api.NewClient()
	if err != nil {
		return state.Status{}, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), apiDialTimeout)
	defer cancel()

	st, err := client.Status(ctx)
	if err != nil {
		return state.Status{}, false
	}
	return st, true
}

// runAbout prints the About dialog's text (version, commit, links and third-party licenses) to
// stdout, so the same content the tray's About dialog shows is available without a GUI.
func runAbout() int {
	fmt.Print(about.Text(version, commit))
	return 0
}

func runTray(args []string) int {
	fs := flag.NewFlagSet("tray", flag.ContinueOnError)
	mountpoint := fs.String("mountpoint", "", "mountpoint the tray manages (default: the last one used, else ~/ProtonDrive)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	mp := resolveTrayMountpoint(*mountpoint)
	if err := tray.SaveMountpoint(mp); err != nil {
		fmt.Fprintln(os.Stderr, "warning: remembering the mountpoint failed:", err)
	}

	logPath, err := mountLogPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: preparing log file:", err)
		return 1
	}

	tray.Run(tray.Options{
		Mountpoint: mp,
		LogPath:    logPath,
		Version:    version,
		Commit:     commit,
		Mounted:    func() bool { return isMounted(mp) },
		LoggedIn:   func() bool { _, err := auth.Load(); return err == nil },
	})

	return 0
}

// resolveTrayMountpoint prefers the flag, then the mountpoint the tray used last, then the
// default under the home directory.
func resolveTrayMountpoint(flagValue string) string {
	if flagValue != "" {
		return absOrSelf(flagValue)
	}
	if saved := tray.LoadMountpoint(); saved != "" {
		return saved
	}
	return tray.DefaultMountpoint()
}

func absOrSelf(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

// isMounted reports whether one of our filesystems is currently mounted at mountpoint.
func isMounted(mountpoint string) bool {
	mounts, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return false
	}
	return mountedAt(string(mounts), absOrSelf(mountpoint))
}
