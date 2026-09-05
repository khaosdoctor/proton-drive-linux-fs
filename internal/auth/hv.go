package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	proton "github.com/henrybear327/go-proton-api"
)

const verifyOrigin = "https://verify.proton.me"

// HumanVerificationRequired is returned by Login when Proton refuses the credentials
// until the user passes a CAPTCHA or an emailed code (API error 9001).
type HumanVerificationRequired struct {
	Token   string
	Methods []string
}

func (e *HumanVerificationRequired) Error() string {
	return fmt.Sprintf("human verification required (methods: %s)", strings.Join(e.Methods, ", "))
}

func asHumanVerification(err error) *HumanVerificationRequired {
	var apiErr *proton.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != proton.HumanVerificationRequired {
		return nil
	}

	// Details is decoded into an untyped value, so round-trip it through JSON to read it.
	raw, marshalErr := json.Marshal(apiErr.Details)
	if marshalErr != nil {
		return nil
	}

	return parseHVDetails(raw)
}

// parseHVDetails reads the Details object of a 9001 error body.
func parseHVDetails(raw []byte) *HumanVerificationRequired {
	var d struct {
		HumanVerificationToken   string
		HumanVerificationMethods []string
	}

	if err := json.Unmarshal(raw, &d); err != nil {
		return nil
	}

	if d.HumanVerificationToken == "" {
		return nil
	}

	return &HumanVerificationRequired{Token: d.HumanVerificationToken, Methods: d.HumanVerificationMethods}
}

// CaptchaURL points at Proton's verify page for a human verification token. It has to
// be opened as a normal browser tab: Proton's verify app, run top-level, submits the
// solved CAPTCHA to core/v4/verification/captcha/<token> itself (WebClients
// Verify.tsx, submitExternalCaptcha), which makes the same token redeemable by the
// login retry. A local page can't do this in an iframe: Proton serves the page with
// "frame-ancestors https://mail.proton.me https://calendar.proton.me
// https://drive.proton.me", which forbids embedding it anywhere but Proton's own
// origins.
func CaptchaURL(token string) string {
	return verifyOrigin + "/?" + url.Values{
		"methods": {"captcha"},
		"token":   {token},
	}.Encode()
}

// RequestVerificationCode asks Proton to send a verification code to an email address
// or phone number. method is "email" or "sms".
func RequestVerificationCode(ctx context.Context, username, method, destination string) error {
	req := proton.SendVerificationCodeReq{
		Username:    username,
		Type:        proton.EmailTokenType,
		Destination: proton.TokenDestination{Address: destination},
	}

	if method == "sms" {
		req.Type = proton.SMSTokenType
		req.Destination = proton.TokenDestination{Phone: destination}
	}

	return newManager().SendVerificationCode(ctx, req)
}

// FormatCodeToken builds the header token value Proton expects for email and sms codes.
func FormatCodeToken(destination, code string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, destination+":"+code)
}

type hvTransport struct {
	base   http.RoundTripper
	method string
	token  string
}

func (t hvTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("x-pm-human-verification-token-type", t.method)
	r.Header.Set("x-pm-human-verification-token", t.token)

	return t.base.RoundTrip(r)
}
