package drive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMoveLinkReqJSON asserts every field Proton's move endpoint requires -- including
// NameSignatureEmail, the one go-proton-api's own MoveLinkReq never sends -- is present and
// non-empty in the marshaled request body.
func TestMoveLinkReqJSON(t *testing.T) {
	req := moveLinkReq{
		Name:                    "enc-name",
		Hash:                    "name-hash",
		ParentLinkID:            "parent-link-id",
		OriginalHash:            "old-name-hash",
		NodePassphrase:          "enc-passphrase",
		NodePassphraseSignature: "passphrase-sig",
		SignatureAddress:        "user@proton.me",
		NameSignatureEmail:      "user@proton.me",
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{
		"Name", "Hash", "ParentLinkID", "OriginalHash",
		"NodePassphrase", "NodePassphraseSignature", "SignatureAddress", "NameSignatureEmail",
	} {
		if got[key] == "" {
			t.Errorf("moveLinkReq JSON missing or empty required key %q: %s", key, data)
		}
	}
}

// TestPutJSON checks the headers putJSON sends and that an API error body ({"Code":...,
// "Error":...}) surfaces as an error carrying both the code and the message.
func TestPutJSON(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"Code":2000,"Error":"This value should not be blank."}`))
	}))
	defer server.Close()

	c := &Client{
		apiURL:      server.URL,
		tokenSource: func() (string, string) { return "test-uid", "test-access" },
	}

	err := c.putJSON(context.Background(), "/drive/shares/s1/links/l1/move", moveLinkReq{Name: "x"})
	if err == nil {
		t.Fatal("putJSON: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "This value should not be blank.") {
		t.Errorf("error %q does not contain the API message", err)
	}
	if !strings.Contains(err.Error(), "2000") {
		t.Errorf("error %q does not contain the API code", err)
	}

	if got := gotHeaders.Get("x-pm-uid"); got != "test-uid" {
		t.Errorf("x-pm-uid = %q, want %q", got, "test-uid")
	}
	if got := gotHeaders.Get("Authorization"); got != "Bearer test-access" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-access")
	}
	if got := gotHeaders.Get("x-pm-appversion"); got == "" {
		t.Error("x-pm-appversion header not set")
	}
	if got := gotHeaders.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}
