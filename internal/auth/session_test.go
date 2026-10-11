package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionJSONRoundTripKeyringCase(t *testing.T) {
	s := Session{
		Username:     "user@proton.me",
		UID:          "uid-1",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		KeyPass:      []byte("secret key pass"),
	}

	data, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if strings.Contains(string(data), "secret key pass") {
		t.Fatalf("key password leaked into JSON when the keyring holds it: %s", data)
	}

	var got Session
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Username != s.Username || got.UID != s.UID || got.AccessToken != s.AccessToken || got.RefreshToken != s.RefreshToken {
		t.Fatalf("round trip changed non-key fields: got %+v, want %+v", got, s)
	}
	if len(got.KeyPassFile) != 0 {
		t.Fatalf("KeyPassFile should stay empty when the keyring holds the key password, got %q", got.KeyPassFile)
	}
}

func TestSessionJSONRoundTripFallbackCase(t *testing.T) {
	s := Session{
		Username:     "user@proton.me",
		UID:          "uid-1",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		KeyPassFile:  []byte("secret key pass"),
	}

	data, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Session
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if string(got.KeyPassFile) != string(s.KeyPassFile) {
		t.Fatalf("KeyPassFile did not round trip: got %q, want %q", got.KeyPassFile, s.KeyPassFile)
	}

	// Load() copies KeyPassFile back into KeyPass; mirror that here without touching the keyring.
	got.KeyPass = got.KeyPassFile
	if string(got.KeyPass) != "secret key pass" {
		t.Fatalf("KeyPass not restored from KeyPassFile: got %q", got.KeyPass)
	}
}

// TestLoggedInCheckCachesKeyringRead covers the tray's periodic logged-in check: a session file
// whose key password can't be loaded is logged out, like a full Load says, and the keyring is read
// once, not again on every call while the file is unchanged.
func TestLoggedInCheckCachesKeyringRead(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reads := 0
	orig := keyringGet
	t.Cleanup(func() { keyringGet = orig })
	keyringGet = func(string, string) (string, error) {
		reads++
		return "", errors.New("no such entry")
	}

	check := LoggedInCheck()
	if check() {
		t.Fatal("logged in with no session file")
	}
	if reads != 0 {
		t.Fatalf("keyring read %d times with no session file, want 0", reads)
	}

	dir, err := sessionDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{"Username":"user@proton.me","UID":"uid-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if check() {
			t.Fatal("logged in with a session file whose key password can't be loaded")
		}
	}
	if reads != 1 {
		t.Fatalf("keyring read %d times over two checks of an unchanged file, want 1", reads)
	}
}
