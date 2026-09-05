package tray

import (
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestVisibilityFor(t *testing.T) {
	tests := []struct {
		name     string
		loggedIn bool
		mounted  bool
		want     menuVisibility
	}{
		{
			"logged out, not mounted",
			false,
			false,
			menuVisibility{Mount: false, Unmount: false, Pause: false, OpenFolder: false, Login: true, Logout: false},
		},
		{
			"logged out, mounted",
			false,
			true,
			menuVisibility{Mount: false, Unmount: true, Pause: true, OpenFolder: true, Login: true, Logout: false},
		},
		{
			"logged in, not mounted",
			true,
			false,
			menuVisibility{Mount: true, Unmount: false, Pause: false, OpenFolder: false, Login: false, Logout: true},
		},
		{
			"logged in, mounted",
			true,
			true,
			menuVisibility{Mount: false, Unmount: true, Pause: true, OpenFolder: true, Login: false, Logout: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := visibilityFor(tt.loggedIn, tt.mounted)
			if got != tt.want {
				t.Errorf("visibilityFor(%v, %v) = %+v, want %+v", tt.loggedIn, tt.mounted, got, tt.want)
			}
		})
	}
}

func TestPickState(t *testing.T) {
	tests := []struct {
		name      string
		loggedIn  bool
		mounted   bool
		paused    bool
		transfers int64
		fresh     bool
		want      State
	}{
		{"no session", false, true, false, 3, true, StateLoggedOut},
		{"paused outranks transfers", true, true, true, 3, true, StatePaused},
		{"logged in but not mounted", true, false, false, 0, false, StateUnmounted},
		{"transfers in flight", true, true, false, 1, true, StateSyncing},
		{"stale snapshot is not syncing", true, true, false, 1, false, StateOnline},
		{"mounted and idle", true, true, false, 0, true, StateOnline},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pickState(tt.loggedIn, tt.mounted, tt.paused, tt.transfers, tt.fresh)
			if got != tt.want {
				t.Errorf("pickState() = %v, want %v", got, tt.want)
			}
		})
	}
}

// fakeLookPath finds only the terminals in found, and reports the order it was asked in.
func fakeLookPath(found []string, asked *[]string) func(string) (string, error) {
	return func(name string) (string, error) {
		*asked = append(*asked, name)
		if !slices.Contains(found, name) {
			return "", exec.ErrNotFound
		}
		if filepath.IsAbs(name) {
			return name, nil
		}
		return "/usr/bin/" + name, nil
	}
}

func TestTerminalCommandOrder(t *testing.T) {
	var asked []string
	argv := terminalCommand(fakeLookPath([]string{"konsole", "foot"}, &asked), func(string) string { return "" }, []string{"/bin/exe", "login"})

	want := []string{"/usr/bin/foot", "-e", "/bin/exe", "login"}
	if !slices.Equal(argv, want) {
		t.Errorf("terminalCommand() = %v, want %v", argv, want)
	}
	if !slices.Equal(asked, []string{"x-terminal-emulator", "kitty", "alacritty", "foot"}) {
		t.Errorf("asked in order %v", asked)
	}
}

func TestTerminalCommandPrefersEnv(t *testing.T) {
	var asked []string
	getenv := func(name string) string {
		if name == "TERMINAL" {
			return "/opt/bin/wezterm"
		}
		return ""
	}

	argv := terminalCommand(fakeLookPath([]string{"/opt/bin/wezterm", "xterm"}, &asked), getenv, []string{"/bin/exe", "login"})

	want := []string{"/opt/bin/wezterm", "-e", "/bin/exe", "login"}
	if !slices.Equal(argv, want) {
		t.Errorf("terminalCommand() = %v, want %v", argv, want)
	}
	if len(asked) != 1 || asked[0] != "/opt/bin/wezterm" {
		t.Errorf("$TERMINAL should be tried first, asked %v", asked)
	}
}

func TestTerminalCommandGnomeSeparator(t *testing.T) {
	var asked []string
	argv := terminalCommand(fakeLookPath([]string{"gnome-terminal"}, &asked), func(string) string { return "" }, []string{"/bin/exe", "login"})

	want := []string{"/usr/bin/gnome-terminal", "--", "/bin/exe", "login"}
	if !slices.Equal(argv, want) {
		t.Errorf("terminalCommand() = %v, want %v", argv, want)
	}
}

func TestTerminalCommandNoneFound(t *testing.T) {
	var asked []string
	argv := terminalCommand(fakeLookPath(nil, &asked), func(string) string { return "" }, []string{"/bin/exe", "login"})
	if argv != nil {
		t.Errorf("terminalCommand() = %v, want nil", argv)
	}
	if len(asked) != len(terminals) {
		t.Errorf("every terminal should be tried, asked %v", asked)
	}
}

func TestTerminalCommandDoesNotMutateTerminals(t *testing.T) {
	before := slices.Clone(terminals)
	var asked []string
	terminalCommand(fakeLookPath(nil, &asked), func(string) string { return "kitty" }, []string{"x"})
	if !slices.Equal(terminals, before) {
		t.Errorf("terminals mutated: %v", terminals)
	}
}

func TestStatusLine(t *testing.T) {
	tests := []struct {
		name string
		sn   snapshot
		want string
	}{
		{"logged out", snapshot{}, "Not logged in"},
		{"not mounted", snapshot{loggedIn: true}, "Not mounted"},
		{"mounted", snapshot{loggedIn: true, mounted: true}, "Mounted at /m"},
		{"paused", snapshot{loggedIn: true, mounted: true, paused: true}, "Mounted at /m (paused)"},
		{"syncing", snapshot{loggedIn: true, mounted: true, transfers: 2, fresh: true}, "Mounted at /m (syncing)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.sn.statusLine("/m"); got != tt.want {
				t.Errorf("statusLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIconsEmbedded(t *testing.T) {
	for _, s := range []State{StateOnline, StateSyncing, StatePaused, StateUnmounted, StateLoggedOut} {
		if len(icon(s)) == 0 {
			t.Errorf("state %v has no icon", s)
		}
	}
}

func TestMountpointRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if got := LoadMountpoint(); got != "" {
		t.Fatalf("LoadMountpoint() = %q, want empty", got)
	}
	if err := SaveMountpoint("/home/u/Drive"); err != nil {
		t.Fatal(err)
	}
	if got := LoadMountpoint(); got != "/home/u/Drive" {
		t.Fatalf("LoadMountpoint() = %q, want /home/u/Drive", got)
	}
}
