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
