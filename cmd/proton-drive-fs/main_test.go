package main

import (
	"slices"
	"testing"
)

func TestParseDenyReaders(t *testing.T) {
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
		if got := parseDenyReaders(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("parseDenyReaders(%q) = %v, want %v", tt.in, got, tt.want)
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
