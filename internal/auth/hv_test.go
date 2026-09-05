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
		name       string
		raw        string
		wantToken  string
		wantMethod string
	}{
		{
			name:       "verify app success",
			raw:        `{"type":"HUMAN_VERIFICATION_SUCCESS","payload":{"token":"tok-1","type":"email"}}`,
			wantToken:  "tok-1",
			wantMethod: "email",
		},
		{
			name:       "verify app success without method",
			raw:        `{"type":"HUMAN_VERIFICATION_SUCCESS","payload":{"token":"tok-1"}}`,
			wantToken:  "tok-1",
			wantMethod: "captcha",
		},
		{
			name:       "raw captcha frame",
			raw:        `{"type":"pm_captcha","token":"tok-2"}`,
			wantToken:  "tok-2",
			wantMethod: "captcha",
		},
		{
			name: "captcha frame height",
			raw:  `{"type":"pm_height","height":320}`,
		},
		{
			name: "verify app resize",
			raw:  `{"type":"RESIZE","payload":{"height":320}}`,
		},
		{
			name: "success without token",
			raw:  `{"type":"HUMAN_VERIFICATION_SUCCESS","payload":{"type":"captcha"}}`,
		},
		{
			name: "token from an unknown message type",
			raw:  `{"type":"WHATEVER","token":"tok-3"}`,
		},
		{
			name: "broken json",
			raw:  `not json`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, method := parseHVMessage([]byte(tt.raw))
			if token != tt.wantToken {
				t.Errorf("token: want %q, got %q", tt.wantToken, token)
			}
			if method != tt.wantMethod {
				t.Errorf("method: want %q, got %q", tt.wantMethod, method)
			}
		})
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
