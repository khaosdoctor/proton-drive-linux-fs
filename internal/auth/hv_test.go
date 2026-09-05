package auth

import (
	"slices"
	"testing"
)

func TestParseHVDetails(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantToken   string
		wantMethods []string
	}{
		{
			name:        "captcha and email",
			raw:         `{"HumanVerificationToken":"abc123","HumanVerificationMethods":["captcha","email","sms"],"Title":"Verify"}`,
			wantToken:   "abc123",
			wantMethods: []string{"captcha", "email", "sms"},
		},
		{
			name:      "token without methods",
			raw:       `{"HumanVerificationToken":"abc123"}`,
			wantToken: "abc123",
		},
		{
			name: "no token",
			raw:  `{"HumanVerificationMethods":["captcha"]}`,
		},
		{
			name: "unrelated details",
			raw:  `{"Foo":"bar"}`,
		},
		{
			name: "details is null",
			raw:  `null`,
		},
		{
			name: "details is not an object",
			raw:  `"nope"`,
		},
		{
			name: "broken json",
			raw:  `{`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHVDetails([]byte(tt.raw))

			if tt.wantToken == "" {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}

			if got == nil {
				t.Fatal("want a result, got nil")
			}
			if got.Token != tt.wantToken {
				t.Errorf("token: want %q, got %q", tt.wantToken, got.Token)
			}
			if !slices.Equal(got.Methods, tt.wantMethods) {
				t.Errorf("methods: want %v, got %v", tt.wantMethods, got.Methods)
			}
		})
	}
}

func TestParseHVMessage(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "solved captcha",
			raw:  `{"type":"pm_captcha","token":"hv-token:0123abc"}`,
			want: "hv-token:0123abc",
		},
		{name: "height report", raw: `{"type":"pm_height","height":320}`},
		{name: "expired solution", raw: `{"type":"pm_captcha_expired","token":"hv-token:old"}`},
		{name: "no token", raw: `{"type":"pm_captcha"}`},
		{name: "not a proton message", raw: `{"token":"hv-token:0123abc"}`},
		{name: "broken json", raw: `not json`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseHVMessage([]byte(tt.raw)); got != tt.want {
				t.Errorf("want %q, got %q", tt.want, got)
			}
		})
	}
}

func TestVerifyURL(t *testing.T) {
	got := verifyURL("tok 1&x")
	want := "https://verify.proton.me/?methods=captcha&token=tok+1%26x"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestFormatCodeToken(t *testing.T) {
	tests := []struct {
		destination string
		code        string
		want        string
	}{
		{destination: "me@example.com", code: "123456", want: "me@example.com:123456"},
		{destination: " me@example.com ", code: "123 456", want: "me@example.com:123456"},
		{destination: "+46 70 123 45 67", code: "123456", want: "+46701234567:123456"},
	}

	for _, tt := range tests {
		got := FormatCodeToken(tt.destination, tt.code)
		if got != tt.want {
			t.Errorf("FormatCodeToken(%q, %q): want %q, got %q", tt.destination, tt.code, tt.want, got)
		}
	}
}
