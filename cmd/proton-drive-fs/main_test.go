package main

import "testing"

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
