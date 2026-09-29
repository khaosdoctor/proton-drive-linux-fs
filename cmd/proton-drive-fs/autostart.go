package main

import (
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// unitTemplate is the systemd user unit, with @BINDIR@ standing in for the binary's
// directory. Packages ship it with @BINDIR@ replaced at build time.
//
//go:embed proton-drive-fs.service
var unitTemplate string

// Where distro packages install the unit and the binary it runs.
const (
	systemUnitPath = "/usr/lib/systemd/user/proton-drive-fs.service"
	systemBinPath  = "/usr/bin/proton-drive-fs"
)

// runAutostart installs and enables the systemd user unit so the mount starts with the
// graphical session, or at boot with -headless. -remove undoes it.
func runAutostart(args []string) int {
	const usage = "usage: proton-drive-fs autostart [-headless | -remove]"

	fs := flag.NewFlagSet("autostart", flag.ContinueOnError)
	headless := fs.Bool("headless", false, "start at boot instead of with the graphical session, for machines without a desktop")
	remove := fs.Bool("remove", false, "stop and disable the unit, and delete the user copy")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if fs.NArg() > 0 || (*headless && *remove) {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}

	if *remove {
		return removeAutostart()
	}

	if err := installUnit(*headless); err != nil {
		fmt.Fprintln(os.Stderr, "error: installing the systemd unit:", err)
		return 1
	}

	// reenable (not enable) moves the symlink when switching between headless and graphical.
	for _, step := range [][]string{{"daemon-reload"}, {"reenable", "proton-drive-fs"}, {"restart", "proton-drive-fs"}} {
		if err := systemctl(step...); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	}

	if *headless {
		if out, err := exec.Command("loginctl", "enable-linger").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: loginctl enable-linger failed, so the mount starts at login instead of boot: %v\n%s", err, out)
		}
	}

	fmt.Println("autostart enabled")
	return 0
}

// removeAutostart stops and disables the unit and deletes the user copy. A packaged unit in
// /usr/lib stays on disk, disabled. Lingering stays on, since other units may rely on it.
func removeAutostart() int {
	// disable fails when no unit is installed; the user copy may still be there, so go on.
	if err := systemctl("disable", "--now", "proton-drive-fs"); err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}

	path, err := userUnitPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "error: removing the systemd unit:", err)
		return 1
	}

	if err := systemctl("daemon-reload"); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	fmt.Println("autostart removed")
	return 0
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

func userUnitPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "systemd", "user", "proton-drive-fs.service"), nil
}

// installUnit writes the user unit to $XDG_CONFIG_HOME/systemd/user, running the current
// binary. Run from a packaged install in graphical mode, it uses the packaged unit instead so
// package upgrades keep it current, removing any user copy that would shadow it.
func installUnit(headless bool) error {
	path, err := userUnitPath()
	if err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}

	if _, err := os.Stat(systemUnitPath); err == nil && exe == systemBinPath && !headless {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(renderUnit(exe, headless)), 0o644)
}

// renderUnit points the unit at exe and, for headless machines, hooks it to default.target
// since graphical-session.target never starts there.
func renderUnit(exe string, headless bool) string {
	unit := strings.ReplaceAll(unitTemplate, "@BINDIR@/proton-drive-fs", exe)
	if !headless {
		return unit
	}

	unit = strings.Replace(unit, "PartOf=graphical-session.target\n", "", 1)
	return strings.Replace(unit, "WantedBy=graphical-session.target", "WantedBy=default.target", 1)
}
