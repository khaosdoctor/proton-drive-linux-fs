package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/ProtonMail/gluon/async"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
	proton "github.com/henrybear327/go-proton-api"
)

const apiURL = "https://drive.proton.me/api"

// ponytail: Proton rejects unknown app versions; "Other" is the fallback value to try if this one stops working.
const appVersion = "macos-drive@1.0.0-alpha.1+rclone"

// Session holds the credentials needed to restore a Proton Drive client without logging in again.
type Session struct {
	Username     string
	UID          string
	AccessToken  string
	RefreshToken string
	// ponytail: key password stored on disk 0600 like rclone; OS keyring is Phase 4
	KeyPass []byte
}

func sessionDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "proton-drive-fs"), nil
}

func sessionPath() (string, error) {
	dir, err := sessionDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "session.json"), nil
}

func newManager(opts ...proton.Option) *proton.Manager {
	return proton.New(append([]proton.Option{
		proton.WithHostURL(apiURL),
		proton.WithAppVersion(appVersion),
	}, opts...)...)
}

// Login authenticates against Proton and returns a Session ready to be saved.
// totp is called only if the account requires a second factor. When Proton demands
// human verification the error is a *HumanVerificationRequired.
func Login(ctx context.Context, username string, password []byte, totp func() (string, error)) (*Session, error) {
	return login(ctx, newManager(), username, password, totp)
}

// LoginWithHV logs in carrying the human verification token obtained after a
// *HumanVerificationRequired error. method is the verification method the token
// came from ("captcha", "email" or "sms").
func LoginWithHV(ctx context.Context, username string, password []byte, method, hvToken string, totp func() (string, error)) (*Session, error) {
	m := newManager(proton.WithTransport(hvTransport{
		base:   http.DefaultTransport,
		method: method,
		token:  hvToken,
	}))

	return login(ctx, m, username, password, totp)
}

func login(ctx context.Context, m *proton.Manager, username string, password []byte, totp func() (string, error)) (*Session, error) {
	c, a, err := m.NewClientWithLogin(ctx, username, password)
	if err != nil {
		if hv := asHumanVerification(err); hv != nil {
			return nil, hv
		}
		return nil, err
	}
	defer c.Close()

	if a.PasswordMode == proton.TwoPasswordMode {
		return nil, errors.New("two-password mode not supported")
	}

	if a.TwoFA.Enabled&proton.HasTOTP != 0 {
		if totp == nil {
			return nil, errors.New("account requires a TOTP code")
		}

		code, err := totp()
		if err != nil {
			return nil, err
		}

		if err := c.Auth2FA(ctx, proton.Auth2FAReq{TwoFactorCode: code}); err != nil {
			return nil, err
		}
	}

	user, err := c.GetUser(ctx)
	if err != nil {
		return nil, err
	}

	salts, err := c.GetSalts(ctx)
	if err != nil {
		return nil, err
	}

	keyPass, err := salts.SaltForKey(password, user.Keys.Primary().ID)
	if err != nil {
		return nil, err
	}

	return &Session{
		Username:     username,
		UID:          a.UID,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		KeyPass:      keyPass,
	}, nil
}

// Save persists the session to disk (0600, in a 0700 directory).
func (s *Session) Save() error {
	dir, err := sessionDir()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	path, err := sessionPath()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o600)
}

// Load reads a previously saved session from disk.
func Load() (*Session, error) {
	path, err := sessionPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}

	return &s, nil
}

// Client restores an authenticated Proton client from the session and unlocks the
// combined address keyring. Refreshed tokens are persisted automatically.
func (s *Session) Client() (*proton.Client, *crypto.KeyRing, error) {
	m := newManager()
	c := m.NewClient(s.UID, s.AccessToken, s.RefreshToken)

	c.AddAuthHandler(func(a proton.Auth) {
		s.AccessToken = a.AccessToken
		s.RefreshToken = a.RefreshToken
		_ = s.Save() // ponytail: best-effort persist of refreshed tokens
	})

	ctx := context.Background()

	user, err := c.GetUser(ctx)
	if err != nil {
		c.Close()
		return nil, nil, err
	}

	addrs, err := c.GetAddresses(ctx)
	if err != nil {
		c.Close()
		return nil, nil, err
	}

	_, addrKRs, err := proton.Unlock(user, addrs, s.KeyPass, async.NoopPanicHandler{})
	if err != nil {
		c.Close()
		return nil, nil, err
	}

	addrKR, err := crypto.NewKeyRing(nil)
	if err != nil {
		c.Close()
		return nil, nil, err
	}

	for _, kr := range addrKRs {
		for _, key := range kr.GetKeys() {
			if err := addrKR.AddKey(key); err != nil {
				c.Close()
				return nil, nil, err
			}
		}
	}

	return c, addrKR, nil
}

// Logout revokes the session on Proton's side and removes the local session file.
func (s *Session) Logout(ctx context.Context) error {
	m := newManager()
	c := m.NewClient(s.UID, s.AccessToken, s.RefreshToken)
	defer c.Close()

	authErr := c.AuthDelete(ctx)

	path, err := sessionPath()
	if err != nil {
		return err
	}

	if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && authErr == nil {
		return rmErr
	}

	return authErr
}
