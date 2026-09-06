// Package tray runs the status icon: a StatusNotifierItem with a menu that mounts,
// unmounts, pauses polling, opens the folder and the logs, and logs in or out by running
// the same binary again.
package tray

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/about"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/api"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/logx"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/state"
)

// maxCurrentLines bounds how many in-progress transfers the menu lists directly.
const maxCurrentLines = 3

// maxRecentItems bounds how many finished transfers the Recent submenu lists.
const maxRecentItems = 10

// apiDialTimeout bounds how long the tray waits for the local API's unix socket to answer
// before falling back to the status.json file or the pause file directly.
const apiDialTimeout = 500 * time.Millisecond

// readStatus prefers the local API, which answers with a live snapshot straight from the running
// daemon, and falls back to the status.json file (stale after state.StaleAfter) when no daemon is
// listening on the socket, e.g. an older build without the API.
func readStatus() (state.Status, bool) {
	if client, err := api.NewClient(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), apiDialTimeout)
		st, apiErr := client.Status(ctx)
		cancel()
		if apiErr == nil {
			return st, true
		}
	}

	st, ok := state.ReadStatus()
	return st, ok && st.Fresh()
}

//go:generate go run ./gen

// State is what the icon shows.
type State int

const (
	StateLoggedOut State = iota
	StatePaused
	StateUnmounted
	StateSyncing
	StateOnline
)

// pickState maps the facts to an icon state. Being logged out hides everything else, a
// pause marker outranks activity, and transfers only count while the mount's status
// snapshot is fresh enough to trust.
func pickState(loggedIn, mounted, paused bool, transfers int64, fresh bool) State {
	if !loggedIn {
		return StateLoggedOut
	}
	if paused {
		return StatePaused
	}
	if !mounted {
		return StateUnmounted
	}
	if fresh && transfers > 0 {
		return StateSyncing
	}
	return StateOnline
}

func icon(s State) []byte {
	switch s {
	case StateOnline:
		return iconOnline
	case StateSyncing:
		return iconSyncing
	case StatePaused:
		return iconPaused
	}

	// Logged out and merely unmounted both show the hollow glyph; the status line says which.
	return iconIdle
}

// Options is what the tray needs from the command layer: which mountpoint it manages, where
// the fallback log file is, this build's version and commit, and how to check whether the mount
// and the session are live.
type Options struct {
	Mountpoint string
	LogPath    string
	Version    string
	Commit     string
	Mounted    func() bool
	LoggedIn   func() bool
}

// snapshot is one round of status detection.
type snapshot struct {
	loggedIn  bool
	mounted   bool
	paused    bool
	transfers int64
	fresh     bool

	// Upload progress from the status file, shown as "syncing done/total".
	uploadsQueued int64
	uploadsDone   int64
	uploadsFailed int64

	// current and recent mirror state.Status's transfer lists, for the tooltip and the menu.
	current []state.CurrentTransfer
	recent  []state.RecentTransfer

	// ownVersion is this tray's build; daemonVersion is the running daemon's, from the status
	// file. A mismatch means a rebuild left an older daemon running.
	ownVersion    string
	daemonVersion string
}

func (sn snapshot) state() State {
	return pickState(sn.loggedIn, sn.mounted, sn.paused, sn.transfers, sn.fresh)
}

// uploading reports whether files are still waiting on the uploader.
func (sn snapshot) uploading() bool {
	return sn.uploadsQueued > sn.uploadsDone+sn.uploadsFailed
}

// daemonMismatch reports whether the daemon serving the mount is a different build than this
// tray, trustworthy only while the status snapshot is fresh.
func (sn snapshot) daemonMismatch() bool {
	return sn.fresh && sn.daemonVersion != "" && sn.ownVersion != "" && sn.daemonVersion != sn.ownVersion
}

func (sn snapshot) statusLine(mountpoint string) string {
	if !sn.loggedIn {
		return "Not logged in"
	}
	if !sn.mounted {
		return "Not mounted"
	}

	line := "Mounted at " + mountpoint
	switch {
	case sn.paused:
		line += " (paused)"
	case sn.fresh && sn.uploading():
		line += fmt.Sprintf(", syncing %d/%d", sn.uploadsDone, sn.uploadsQueued)
	case sn.fresh && sn.transfers > 0:
		line += " (syncing)"
	}

	if sn.fresh && sn.uploadsFailed > 0 {
		line += fmt.Sprintf(", %d failed", sn.uploadsFailed)
	}

	if sn.daemonMismatch() {
		line += fmt.Sprintf(" (daemon %s, restart needed)", sn.daemonVersion)
	}

	return line
}

// tooltipFor is the tray icon's tooltip: the first active transfer's action, path and percent
// complete, plus how many more are running, or the status line when nothing is moving.
func tooltipFor(sn snapshot, mountpoint string) string {
	if len(sn.current) == 0 {
		return sn.statusLine(mountpoint)
	}

	line := transferLine(sn.current[0])
	if more := len(sn.current) - 1; more > 0 {
		line += fmt.Sprintf(" and %d more", more)
	}
	return line
}

// transferLine formats one in-progress transfer as "Uploading path 40%", or without the percent
// when its total size isn't known.
func transferLine(t state.CurrentTransfer) string {
	verb := "Uploading"
	if t.Action == "download" {
		verb = "Downloading"
	}

	if t.Total <= 0 {
		return verb + " " + t.Path
	}

	pct := t.Bytes * 100 / t.Total
	return fmt.Sprintf("%s %s %d%%", verb, t.Path, pct)
}

// recentLine formats one finished transfer for the Recent submenu: a checkmark and its size on
// success, a cross and the error on failure.
func recentLine(rt state.RecentTransfer) string {
	if rt.Status == "failed" {
		return fmt.Sprintf("✗ %s: %s", rt.Path, rt.Err)
	}
	return fmt.Sprintf("✓ %s (%s)", rt.Path, logx.FormatSize(rt.Bytes))
}

// refreshInterval is how often poll re-checks status: every second while a transfer is active,
// so the tooltip's percentage keeps up, otherwise every two.
func refreshInterval(sn snapshot) time.Duration {
	if len(sn.current) > 0 {
		return time.Second
	}
	return 2 * time.Second
}

type app struct {
	opts    Options
	refresh chan struct{}
	shown   State

	status               *systray.MenuItem
	current              [maxCurrentLines]*systray.MenuItem
	recentSub            *systray.MenuItem
	recentItems          [maxRecentItems]*systray.MenuItem
	mount, unmount       *systray.MenuItem
	restart              *systray.MenuItem
	pause, resume        *systray.MenuItem
	openFolder, openLogs *systray.MenuItem
	openDebugLogs        *systray.MenuItem
	login, logout        *systray.MenuItem
	about, openDashboard *systray.MenuItem
	hintUntil            atomic.Int64

	// recentMu guards recentPaths, the path each Recent submenu slot currently shows, so a click
	// opens whatever folder that slot is showing right now, not whatever it showed when the menu
	// was built.
	recentMu    sync.Mutex
	recentPaths [maxRecentItems]string
}

// Run shows the icon and blocks until Quit is chosen.
func Run(opts Options) {
	a := &app{opts: opts, refresh: make(chan struct{}, 1), shown: -1}
	systray.Run(a.onReady, func() {})
}

func (a *app) onReady() {
	systray.SetIcon(iconIdle)
	systray.SetTooltip("Proton Drive FS")

	a.status = systray.AddMenuItem("Checking", "")
	a.status.Disable()

	for i := range a.current {
		item := systray.AddMenuItem("", "")
		item.Disable()
		item.Hide()
		a.current[i] = item
	}

	a.recentSub = systray.AddMenuItem("Recent", "The last few finished transfers")
	a.recentSub.Hide()
	for i := range a.recentItems {
		item := a.recentSub.AddSubMenuItem("", "")
		item.Hide()
		a.recentItems[i] = item
		onClick(item, a.openRecentAt(i))
	}
	systray.AddSeparator()

	a.mount = systray.AddMenuItem("Mount", "Mount Proton Drive at "+a.opts.Mountpoint)
	a.unmount = systray.AddMenuItem("Unmount", "Unmount "+a.opts.Mountpoint)
	a.restart = systray.AddMenuItem("Restart mount", "Unmount and remount, to pick up a rebuild")
	a.pause = systray.AddMenuItem("Pause syncing", "Stop polling Proton for remote changes")
	a.resume = systray.AddMenuItem("Resume syncing", "Poll Proton for remote changes again")
	a.openFolder = systray.AddMenuItem("Open folder", "Open "+a.opts.Mountpoint+" in the file manager")
	a.openLogs = systray.AddMenuItem("Open logs", "Show the mount log")
	a.openDebugLogs = systray.AddMenuItem("Open debug logs", "Show the mount log at debug verbosity")
	systray.AddSeparator()

	a.login = systray.AddMenuItem("Log in", "Log in to Proton in a terminal")
	a.logout = systray.AddMenuItem("Log out", "Revoke the saved session")
	systray.AddSeparator()

	a.about = systray.AddMenuItem("About proton-drive-fs", "Version, links and third-party licenses")
	if _, err := exec.LookPath("proton-drive-fs-gui"); err == nil {
		a.openDashboard = systray.AddMenuItem("Open dashboard", "Open the proton-drive-fs-gui dashboard")
		onClick(a.openDashboard, func() { start([]string{"proton-drive-fs-gui"}) })
	}
	quit := systray.AddMenuItem("Quit", "Close the tray icon; the mount keeps running")

	onClick(a.mount, func() { a.runSelf("mount", a.opts.Mountpoint) })
	onClick(a.unmount, func() { a.runSelf("unmount", a.opts.Mountpoint) })
	onClick(a.restart, a.restartMount)
	onClick(a.pause, func() { a.setPaused(true) })
	onClick(a.resume, func() { a.setPaused(false) })
	onClick(a.openFolder, a.showFolder)
	onClick(a.openLogs, a.showLogs)
	onClick(a.openDebugLogs, a.showDebugLogs)
	onClick(a.login, a.startLogin)
	onClick(a.logout, func() { a.runSelf("logout") })
	onClick(a.about, a.showAbout)
	onClick(quit, systray.Quit)

	go a.poll()
}

// showAbout displays the About dialog; a failure (no zenity and no xdg-open, say) only gets a
// log line since there is nowhere else in a tray click to report it.
func (a *app) showAbout() {
	if err := about.Show(a.opts.Version, a.opts.Commit); err != nil {
		slog.Warn("showing about dialog failed", "err", err)
	}
}

// openRecentAt returns the click handler for Recent submenu slot i: it opens whatever folder
// that slot currently shows, read fresh at click time since slots are reused as the list changes.
func (a *app) openRecentAt(i int) func() {
	return func() {
		a.recentMu.Lock()
		p := a.recentPaths[i]
		a.recentMu.Unlock()
		if p == "" {
			return
		}

		dir := filepath.Dir(filepath.Join(a.opts.Mountpoint, p))
		if err := exec.Command("xdg-open", dir).Start(); err != nil {
			slog.Warn("xdg-open failed", "path", dir, "err", err)
		}
	}
}

func onClick(item *systray.MenuItem, fn func()) {
	go func() {
		for range item.ClickedCh {
			fn()
		}
	}()
}

// poll refreshes the icon and menu every two seconds, or every one while a transfer is active
// (see refreshInterval), and re-checks the session (which can reach the OS keyring) at most
// every five.
func (a *app) poll() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var (
		loggedIn  bool
		lastCheck time.Time
	)

	for {
		if time.Since(lastCheck) >= 5*time.Second {
			loggedIn = a.opts.LoggedIn()
			lastCheck = time.Now()
		}

		st, fresh := readStatus()
		sn := snapshot{
			loggedIn:      loggedIn,
			mounted:       a.opts.Mounted(),
			paused:        state.Paused(),
			transfers:     st.Transfers,
			fresh:         fresh,
			uploadsQueued: st.UploadsQueued,
			uploadsDone:   st.UploadsDone,
			uploadsFailed: st.UploadsFailed,
			current:       st.Current,
			recent:        st.Recent,
			ownVersion:    a.opts.Version,
			daemonVersion: st.Version,
		}
		a.apply(sn)
		ticker.Reset(refreshInterval(sn))

		select {
		case <-ticker.C:
		case <-a.refresh:
			lastCheck = time.Time{}
		}
	}
}

func (a *app) apply(sn snapshot) {
	s := sn.state()

	if s != a.shown {
		systray.SetIcon(icon(s))
		a.shown = s
	}

	if time.Now().UnixNano() >= a.hintUntil.Load() {
		a.status.SetTitle(sn.statusLine(a.opts.Mountpoint))
	}
	systray.SetTooltip(tooltipFor(sn, a.opts.Mountpoint))

	v := visibilityFor(sn.loggedIn, sn.mounted)
	showIf(a.mount, v.Mount)
	showIf(a.unmount, v.Unmount)
	showIf(a.restart, v.Restart)
	showIf(a.pause, v.Pause && !sn.paused)
	showIf(a.resume, v.Pause && sn.paused)
	showIf(a.openFolder, v.OpenFolder)
	showIf(a.login, v.Login)
	showIf(a.logout, v.Logout)

	a.applyCurrent(sn.current)
	a.applyRecent(sn.recent)
}

// applyCurrent updates the up-to-3 disabled lines showing transfers in progress, hiding whatever
// slots aren't in use.
func (a *app) applyCurrent(current []state.CurrentTransfer) {
	for i, item := range a.current {
		if i >= len(current) {
			item.Hide()
			continue
		}
		item.SetTitle(transferLine(current[i]))
		item.Show()
	}
}

// applyRecent updates the Recent submenu's up-to-10 items, hiding the submenu entirely when
// there is nothing finished yet.
func (a *app) applyRecent(recent []state.RecentTransfer) {
	if len(recent) == 0 {
		a.recentSub.Hide()
		return
	}
	a.recentSub.Show()

	a.recentMu.Lock()
	defer a.recentMu.Unlock()

	for i, item := range a.recentItems {
		if i >= len(recent) {
			item.Hide()
			a.recentPaths[i] = ""
			continue
		}
		item.SetTitle(recentLine(recent[i]))
		item.Show()
		a.recentPaths[i] = recent[i].Path
	}
}

type menuVisibility struct {
	Mount, Unmount, Restart, Pause, OpenFolder, Login, Logout bool
}

func visibilityFor(loggedIn, mounted bool) menuVisibility {
	return menuVisibility{
		Mount:      loggedIn && !mounted,
		Unmount:    mounted,
		Restart:    mounted,
		Pause:      mounted,
		OpenFolder: mounted,
		Login:      !loggedIn,
		Logout:     loggedIn,
	}
}

func showIf(item *systray.MenuItem, show bool) {
	if show {
		item.Show()
		return
	}
	item.Hide()
}

// signal asks poll for an immediate refresh.
func (a *app) signal() {
	select {
	case a.refresh <- struct{}{}:
	default:
	}
}

// hint parks a message on the status line for ten seconds, for when there is nothing to
// launch and the user has to run a command themselves.
func (a *app) hint(text string) {
	a.hintUntil.Store(time.Now().Add(10 * time.Second).UnixNano())
	a.status.SetTitle(text)
}

// runSelf runs this binary again with args and waits for it. "mount" detaches on its own,
// so nothing here blocks for the lifetime of a mount. On failure it logs and also parks the
// command's first output line on the status line, since a menu click has no terminal to show
// it in.
func (a *app) runSelf(args ...string) {
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("locating own executable failed", "err", err)
		return
	}

	var output bytes.Buffer
	cmd := exec.Command(exe, args...)
	cmd.Stdout = io.MultiWriter(os.Stdout, &output)
	cmd.Stderr = io.MultiWriter(os.Stderr, &output)
	if err := cmd.Run(); err != nil {
		slog.Warn("tray command failed", "command", strings.Join(args, " "), "err", err)
		a.hint(firstLine(output.String()))
	}

	a.signal()
}

// restartMount unmounts and remounts, for when the running daemon is an older build than this
// tray: a plain "mount" would refuse because the mountpoint is still busy with the old one.
func (a *app) restartMount() {
	a.runSelf("unmount", a.opts.Mountpoint)
	a.runSelf("mount", a.opts.Mountpoint)
}

// firstLine returns s up to its first newline, or "(no output)" when there was none.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return "(no output)"
	}
	return line
}

func (a *app) setPaused(paused bool) {
	if err := setPausedViaAPIOrFile(paused); err != nil {
		slog.Warn("setting pause marker failed", "err", err)
	} else if paused {
		slog.Info("paused")
	} else {
		slog.Info("resumed")
	}
	a.signal()
}

// setPausedViaAPIOrFile prefers the local API, so a pause set from the tray reaches whichever
// daemon process is actually running even after a remount, and falls back to writing the pause
// file directly (today's behavior) when no daemon answers on the socket.
func setPausedViaAPIOrFile(paused bool) error {
	if client, err := api.NewClient(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), apiDialTimeout)
		apiErr := client.SetPaused(ctx, paused)
		cancel()
		if apiErr == nil {
			return nil
		}
	}
	return state.SetPaused(paused)
}

func (a *app) showFolder() {
	if err := exec.Command("xdg-open", a.opts.Mountpoint).Start(); err != nil {
		slog.Warn("xdg-open failed", "path", a.opts.Mountpoint, "err", err)
	}
}

// showLogs follows the journal when the mount logs there, and otherwise opens the log file.
func (a *app) showLogs() {
	journal := []string{"journalctl", "--user", "-t", "proton-drive-fs", "-f"}

	if logx.JournaldAvailable() {
		argv := terminalCommand(exec.LookPath, os.Getenv, journal)
		if argv == nil {
			a.hint("Run: " + strings.Join(journal, " "))
			return
		}
		start(argv)
		return
	}

	if err := exec.Command("xdg-open", a.opts.LogPath).Start(); err != nil {
		slog.Warn("xdg-open failed", "path", a.opts.LogPath, "err", err)
	}
}

// showDebugLogs is like showLogs but at debug verbosity, for the technical detail the plain
// "Open logs" item leaves out.
func (a *app) showDebugLogs() {
	journal := []string{"journalctl", "--user", "-t", "proton-drive-fs", "-p", "debug", "-f"}

	if !logx.JournaldAvailable() {
		a.hint("Debug logs need the systemd journal")
		return
	}

	argv := terminalCommand(exec.LookPath, os.Getenv, journal)
	if argv == nil {
		a.hint("Run: " + strings.Join(journal, " "))
		return
	}
	start(argv)
}

// startLogin opens a terminal for the login prompts, which need stdin.
func (a *app) startLogin() {
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("locating own executable failed", "err", err)
		return
	}

	argv := terminalCommand(exec.LookPath, os.Getenv, []string{exe, "login"})
	if argv == nil {
		a.hint("Run: proton-drive-fs login")
		return
	}

	start(argv)
}

func start(argv []string) {
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		slog.Warn("starting command failed", "command", strings.Join(argv, " "), "err", err)
		return
	}
	go func() { _ = cmd.Wait() }()
}

// terminals are tried in this order after $TERMINAL.
var terminals = []string{"x-terminal-emulator", "kitty", "alacritty", "foot", "gnome-terminal", "konsole", "xterm"}

// terminalCommand returns the argv that runs cmdArgs inside the first terminal emulator
// found on PATH, or nil when there is none.
func terminalCommand(lookPath func(string) (string, error), getenv func(string) string, cmdArgs []string) []string {
	candidates := terminals
	if preferred := getenv("TERMINAL"); preferred != "" {
		candidates = append([]string{preferred}, terminals...)
	}

	for _, term := range candidates {
		path, err := lookPath(term)
		if err != nil {
			continue
		}

		// gnome-terminal dropped -e; everything else here still takes it.
		separator := "-e"
		if filepath.Base(term) == "gnome-terminal" {
			separator = "--"
		}

		return append([]string{path, separator}, cmdArgs...)
	}

	return nil
}

// DefaultMountpoint is where the tray mounts when neither the flag nor the remembered
// config says otherwise.
func DefaultMountpoint() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "ProtonDrive"
	}
	return filepath.Join(home, "ProtonDrive")
}

func configPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "proton-drive-fs", "tray.json"), nil
}

type config struct {
	Mountpoint string `json:"mountpoint"`
}

// LoadMountpoint returns the mountpoint the tray used last, or "" when there is none.
func LoadMountpoint() string {
	path, err := configPath()
	if err != nil {
		return ""
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	var c config
	if err := json.Unmarshal(data, &c); err != nil {
		return ""
	}
	return c.Mountpoint
}

// SaveMountpoint remembers mountpoint so the menu keeps working after a restart.
func SaveMountpoint(mountpoint string) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	data, err := json.Marshal(config{Mountpoint: mountpoint})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
