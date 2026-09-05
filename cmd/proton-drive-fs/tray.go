package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/auth"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/tray"
)

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
