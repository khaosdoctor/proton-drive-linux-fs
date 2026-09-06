package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/config"
	"github.com/khaosdoctor/proton-drive-linux-fs/internal/state"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"debug", slog.LevelDebug, false},
		{"info", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"DEBUG", slog.LevelDebug, false},
		{" info ", slog.LevelInfo, false},
		{"trace", 0, true},
		{"", 0, true},
	}

	for _, tt := range tests {
		got, err := parseLogLevel(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseLogLevel(%q) = nil error, want an error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseLogLevel(%q) unexpected error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestSplitDenyReaders(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"tumblerd", []string{"tumblerd"}},
		{" tumblerd , localsearch ,, baloo_file ", []string{"tumblerd", "localsearch", "baloo_file"}},
	}

	for _, tt := range tests {
		if got := config.SplitDenyReaders(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("SplitDenyReaders(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestDetachedArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "simple mount with mountpoint",
			in:   []string{"-debug", "/mnt/x"},
			want: []string{"mount", "-foreground", "-debug", "/mnt/x"},
		},
		{
			name: "multiple flags with mountpoint",
			in:   []string{"-debug", "-ttl", "30s", "/mnt/x"},
			want: []string{"mount", "-foreground", "-debug", "-ttl", "30s", "/mnt/x"},
		},
		{
			name: "just mountpoint",
			in:   []string{"/mnt/x"},
			want: []string{"mount", "-foreground", "/mnt/x"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detachedArgs(tt.in)

			if len(got) != len(tt.want) {
				t.Fatalf("want len %d, got len %d", len(tt.want), len(got))
			}

			for i, v := range got {
				if v != tt.want[i] {
					t.Errorf("index %d: want %q, got %q", i, tt.want[i], v)
				}
			}

			if len(got) >= 2 && got[1] != "-foreground" {
				t.Errorf("want -foreground at index 1, got %q", got[1])
			}
		})
	}
}

func TestPickHVMethod(t *testing.T) {
	tests := []struct {
		name    string
		offered []string
		forced  string
		want    string
		wantErr bool
	}{
		{name: "prefers email", offered: []string{"captcha", "email", "sms"}, want: "email"},
		{name: "falls back to sms", offered: []string{"captcha", "sms"}, want: "sms"},
		{name: "captcha last", offered: []string{"captcha"}, want: "captcha"},
		{name: "forced method that was offered", offered: []string{"captcha", "email"}, forced: "captcha", want: "captcha"},
		{name: "forced method that was not offered", offered: []string{"email"}, forced: "captcha", wantErr: true},
		{name: "nothing supported", offered: []string{"invite", "payment"}, wantErr: true},
		{name: "nothing offered", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pickHVMethod(tt.offered, tt.forced)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %q", got)
				}
				return
			}

			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("want %q, got %q", tt.want, got)
			}
		})
	}
}

func TestMountedAt(t *testing.T) {
	tests := []struct {
		name       string
		procMounts string
		mountpoint string
		want       bool
	}{
		{
			name:       "matches fuse.proton-drive-fs entry",
			procMounts: "proton-drive-fs /home/user/ProtonDrive fuse.proton-drive-fs rw,nosuid,nodev,relatime 0 0\n",
			mountpoint: "/home/user/ProtonDrive",
			want:       true,
		},
		{
			name:       "ignores other fstypes at the same path",
			procMounts: "tmpfs /home/user/ProtonDrive tmpfs rw 0 0\n",
			mountpoint: "/home/user/ProtonDrive",
			want:       false,
		},
		{
			name:       "no matching mountpoint",
			procMounts: "proton-drive-fs /home/user/Other fuse.proton-drive-fs rw 0 0\n",
			mountpoint: "/home/user/ProtonDrive",
			want:       false,
		},
		{
			name:       "escapes spaces as \\040",
			procMounts: `proton-drive-fs /home/user/Proton\040Drive fuse.proton-drive-fs rw 0 0` + "\n",
			mountpoint: "/home/user/Proton Drive",
			want:       true,
		},
		{
			name:       "empty mounts",
			procMounts: "",
			mountpoint: "/home/user/ProtonDrive",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mountedAt(tt.procMounts, tt.mountpoint)
			if got != tt.want {
				t.Errorf("mountedAt(%q, %q) = %v, want %v", tt.procMounts, tt.mountpoint, got, tt.want)
			}
		})
	}
}

func TestDescribeRunning(t *testing.T) {
	tests := []struct {
		name string
		st   *state.Status
		this string
		want string
	}{
		{
			name: "nil status",
			st:   nil,
			this: "1.2.0",
			want: "; this binary is version 1.2.0",
		},
		{
			name: "same version",
			st:   &state.Status{PID: 4242, Version: "1.2.0"},
			this: "1.2.0",
			want: " (pid 4242, daemon version 1.2.0); this binary is version 1.2.0",
		},
		{
			name: "different version",
			st:   &state.Status{PID: 4242, Version: "1.1.0"},
			this: "1.2.0",
			want: " (pid 4242, daemon version 1.1.0); this binary is version 1.2.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := describeRunning(tt.st, tt.this); got != tt.want {
				t.Errorf("describeRunning() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFuseConnectionID(t *testing.T) {
	mountinfo := `1234 56 0:104 / /home/u/Proton\040Drive rw,nosuid,nodev,relatime shared:1 - fuse.proton-drive-fs proton-drive-fs rw,user_id=1000,group_id=1000
36 35 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
`

	tests := []struct {
		name       string
		mountpoint string
		wantID     int
		wantFound  bool
	}{
		{
			name:       "matches fuse line and escapes the space",
			mountpoint: "/home/u/Proton Drive",
			wantID:     104,
			wantFound:  true,
		},
		{
			name:       "ignores non-fuse lines",
			mountpoint: "/",
			wantFound:  false,
		},
		{
			name:       "no matching mountpoint",
			mountpoint: "/nope",
			wantFound:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotFound := fuseConnectionID(mountinfo, tt.mountpoint)
			if gotFound != tt.wantFound || (gotFound && gotID != tt.wantID) {
				t.Errorf("fuseConnectionID(_, %q) = (%d, %v), want (%d, %v)", tt.mountpoint, gotID, gotFound, tt.wantID, tt.wantFound)
			}
		})
	}
}

func TestMountHolders(t *testing.T) {
	root := t.TempDir()
	mountpoint := "/home/u/ProtonDrive"

	// pid 123 holds the mount through its cwd.
	mustSymlink(t, mountpoint+"/sub", filepath.Join(root, "123", "cwd"))
	mustWriteFile(t, filepath.Join(root, "123", "comm"), "bash\n")

	// pid 456 has an open fd pointing elsewhere: not a holder.
	mustSymlink(t, "/tmp/elsewhere", filepath.Join(root, "456", "fd", "3"))
	mustWriteFile(t, filepath.Join(root, "456", "comm"), "other\n")

	// pid 789's cwd is under a sibling directory that only shares the mountpoint's prefix;
	// the trailing-slash rule must not treat that as a match.
	mustSymlink(t, mountpoint+"2/sub", filepath.Join(root, "789", "cwd"))
	mustWriteFile(t, filepath.Join(root, "789", "comm"), "sibling\n")

	got := mountHolders(root, mountpoint)
	want := []holder{{pid: 123, comm: "bash"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mountHolders(%q, %q) = %+v, want %+v", root, mountpoint, got, want)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
