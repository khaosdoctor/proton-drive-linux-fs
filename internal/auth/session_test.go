package auth

import (
	"encoding/json"
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
