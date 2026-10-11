package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
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

// TestSavedDoesNotReadKeyring covers the tray's periodic logged-in check: with the key password in
// the keyring (no KeyPassFile), Saved must answer from the session file alone. A keyring that fails
// every call stands in for a locked one that would prompt.
func TestSavedDoesNotReadKeyring(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	keyring.MockInitWithError(errors.New("keyring touched"))
	t.Cleanup(keyring.MockInit)

	if Saved() {
		t.Fatal("Saved() = true with no session file")
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

	if !Saved() {
		t.Fatal("Saved() = false with a session file whose key password lives in the keyring")
	}
}
