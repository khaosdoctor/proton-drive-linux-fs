package auth

import (
	"bytes"
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

// parseHVMessage reads the postMessage payload of Proton's CAPTCHA page. Its token is
// already the full "<hv token>:<solution>" string the retry has to send back:
//
//	function sendToken(responseRaw) { var response = captchaToken + ':' + responseRaw; ...
//	postMessageToParent({"type": "pm_captcha", "token": response}) }
//
// The same page also reports its height and an expired solution, which carry no token.
func parseHVMessage(raw []byte) string {
	var m struct {
		Type  string
		Token string
	}

	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}

	if m.Type != "pm_captcha" {
		return ""
	}

	return m.Token
}

// verifyURL points at Proton's verify app, which solves the CAPTCHA without any help
// from us when it runs as a normal browser tab.
func verifyURL(token string) string {
	return verifyOrigin + "/?" + url.Values{
		"methods": {"captcha"},
		"token":   {token},
	}.Encode()
}

var hvPage = template.Must(template.New("hv").Parse(`<!doctype html>
<title>Proton human verification</title>
<body style="font-family: sans-serif; margin: 2rem; max-width: 40rem">
<p id="status">Solve the CAPTCHA below, then go back to the terminal.</p>
<iframe id="frame" src="/captcha" style="width: 100%; height: 30rem; border: 0"></iframe>
<p>Nothing showing up? <a href="{{.VerifyURL}}" target="_blank" rel="noreferrer">Solve it at verify.proton.me</a>, then <button id="external">tell the terminal you are done</button>.</p>
<script>
function report(path, body) {
	fetch(path, { method: "POST", body: body }).then(function (r) {
		if (r.status === 200) {
			document.getElementById("status").textContent = "Done. Go back to the terminal.";
		}
	});
}
window.addEventListener("message", function (e) {
	if (e.origin !== window.location.origin) {
		return;
	}
	report("/done", JSON.stringify(e.data));
});
document.getElementById("external").addEventListener("click", function () {
	report("/external", "");
});
</script>
</body>
`))

// SolveCaptcha serves the CAPTCHA on 127.0.0.1 and returns the token to retry the
// login with.
//
// Proton's pages cannot be framed by a local page: verify.proton.me and the CAPTCHA
// endpoint both answer with "frame-ancestors https://mail.proton.me
// https://calendar.proton.me https://drive.proton.me". So the local page proxies the
// CAPTCHA HTML through 127.0.0.1, which puts the frame on our own origin and lets its
// "pm_captcha" message reach us; the page's own requests still work because the API
// reflects any Origin back in access-control-allow-origin.
//
// The page also offers verify.proton.me in a normal tab as a way out. Run top-level,
// Proton's verify app posts the solution to core/v4/verification/captcha/<token>
// itself (WebClients Verify.tsx, submitExternalCaptcha), which leaves the bare human
// verification token redeemable by the retry.
func SolveCaptcha(ctx context.Context, hvToken string, openBrowser bool) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}

	m := newManager()
	done := make(chan string, 1)

	finish := func(w http.ResponseWriter, token string) {
		if token == "" {
			// Height and expiry messages land here too; only a token means success.
			w.WriteHeader(http.StatusNoContent)
			return
		}

		select {
		case done <- token:
		default:
		}

		w.WriteHeader(http.StatusOK)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = hvPage.Execute(w, map[string]string{"VerifyURL": verifyURL(hvToken)})
	})

	mux.HandleFunc("/captcha", func(w http.ResponseWriter, r *http.Request) {
		html, err := m.GetCaptcha(r.Context(), hvToken)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		// The base href keeps any relative asset in Proton's HTML pointing at the API.
		html = bytes.Replace(html, []byte("<head>"), []byte(`<head><base href="`+apiURL+`/core/v4/">`), 1)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(html)
	})

	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		finish(w, parseHVMessage(body))
	})

	mux.HandleFunc("/external", func(w http.ResponseWriter, r *http.Request) {
		finish(w, hvToken)
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
	case token := <-done:
		return token, nil
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
