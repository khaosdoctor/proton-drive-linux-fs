package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
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

// parseHVMessage reads the postMessage payload the verify app broadcasts on success
// ({type: "HUMAN_VERIFICATION_SUCCESS", payload: {token, type}}), or the one the raw
// captcha frame sends ({type: "pm_captcha", token}).
func parseHVMessage(raw []byte) (string, string) {
	var m struct {
		Type    string
		Token   string
		Payload struct {
			Token string
			Type  string
		}
	}

	if err := json.Unmarshal(raw, &m); err != nil {
		return "", ""
	}

	if m.Type == "HUMAN_VERIFICATION_SUCCESS" && m.Payload.Token != "" {
		method := m.Payload.Type
		if method == "" {
			method = "captcha"
		}
		return m.Payload.Token, method
	}

	if m.Type == "pm_captcha" && m.Token != "" {
		return m.Token, "captcha"
	}

	return "", ""
}

var hvPage = template.Must(template.New("hv").Parse(`<!doctype html>
<title>Proton human verification</title>
<body style="font-family: sans-serif; margin: 2rem">
<p id="status">Complete the verification below, then go back to the terminal.</p>
<iframe id="frame" name="frame" src="{{.VerifyURL}}" style="width: 100%; height: 34rem; border: 0"></iframe>
<p><a href="{{.CaptchaURL}}" target="frame">Nothing showing up? Load Proton's plain CAPTCHA frame instead.</a></p>
<script>
var origins = ["{{.VerifyOrigin}}", "{{.APIOrigin}}"];
window.addEventListener("message", function (e) {
	if (origins.indexOf(e.origin) < 0) {
		return;
	}
	fetch("/done", { method: "POST", body: JSON.stringify(e.data) }).then(function (r) {
		if (r.status === 200) {
			document.getElementById("status").textContent = "Verified. Go back to the terminal.";
		}
	});
});
</script>
</body>
`))

// SolveCaptcha serves a local page embedding Proton's verify app and waits for it to
// hand back a human verification token.
func SolveCaptcha(ctx context.Context, token string, openBrowser bool) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}

	apiOrigin, err := url.Parse(apiURL)
	if err != nil {
		return "", err
	}
	apiOrigin.Path = ""

	// unverified: whether verify.proton.me allows a 127.0.0.1 page to frame it; the
	// fallback link loads the /core/v4/captcha frame that Proton's own apps embed.
	verifyURL := verifyOrigin + "/?" + url.Values{
		"methods": {"captcha"},
		"token":   {token},
		"embed":   {"true"},
	}.Encode()

	captchaURL := apiURL + "/core/v4/captcha?" + url.Values{
		"Token":             {token},
		"ForceWebMessaging": {"1"},
	}.Encode()

	done := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = hvPage.Execute(w, map[string]string{
			"VerifyURL":    verifyURL,
			"CaptchaURL":   captchaURL,
			"VerifyOrigin": verifyOrigin,
			"APIOrigin":    apiOrigin.String(),
		})
	})
	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		hvToken, _ := parseHVMessage(body)
		if hvToken == "" {
			// Resize and notification messages land here too; only a token matters.
			w.WriteHeader(http.StatusNoContent)
			return
		}

		select {
		case done <- hvToken:
		default:
		}

		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	pageURL := "http://" + ln.Addr().String() + "/"
	fmt.Println("Open this URL to complete the CAPTCHA:", pageURL)

	if openBrowser {
		_ = exec.Command("xdg-open", pageURL).Start()
	}

	select {
	case hvToken := <-done:
		return hvToken, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
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
