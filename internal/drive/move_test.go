package drive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMoveLinkReqJSON asserts every field Proton's move endpoint requires -- exactly WebClients'
// MoveLink shape (Name, Hash, ParentLinkID, NodePassphrase, NameSignatureEmail), no more and no
// less -- is present and non-empty in the marshaled request body. go-proton-api's own
// MoveLinkReq also carries OriginalHash/SignatureAddress; neither belongs in this payload, and an
// empty OriginalHash was exactly what the live "This value should not be blank" (Code=2000)
// error was about.
func TestMoveLinkReqJSON(t *testing.T) {
	req := moveLinkReq{
		Name:               "enc-name",
		Hash:               "name-hash",
		ParentLinkID:       "parent-link-id",
		NodePassphrase:     "enc-passphrase",
		NameSignatureEmail: "user@proton.me",
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	required := []string{"Name", "Hash", "ParentLinkID", "NodePassphrase", "NameSignatureEmail"}
	for _, key := range required {
		if got[key] == "" {
			t.Errorf("moveLinkReq JSON missing or empty required key %q: %s", key, data)
		}
	}

	for _, stale := range []string{"OriginalHash", "SignatureAddress", "NodePassphraseSignature", "SignatureEmail"} {
		if _, ok := got[stale]; ok {
			t.Errorf("moveLinkReq JSON carries %q, which isn't part of WebClients' MoveLink payload: %s", stale, data)
		}
	}

	if len(got) != len(required) {
		t.Errorf("moveLinkReq JSON has %d keys, want exactly %v: %s", len(got), required, data)
	}
}

// TestPayloadFieldLengths checks the error-decoration helper: it names every top-level JSON key
// with its string length (never the value), in alphabetical order, on one line -- so a blank
// field is nameable straight from the journal without ever logging real payload content.
func TestPayloadFieldLengths(t *testing.T) {
	req := moveLinkReq{
		Name:               "enc-name",
		Hash:               "name-hash",
		ParentLinkID:       "",
		NodePassphrase:     "enc-passphrase",
		NameSignatureEmail: "user@proton.me",
	}

	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := payloadFieldLengths(payload)
	want := "Hash=9 Name=8 NameSignatureEmail=14 NodePassphrase=14 ParentLinkID=0"
	if got != want {
		t.Errorf("payloadFieldLengths = %q, want %q", got, want)
	}
	if strings.Contains(got, "enc-name") || strings.Contains(got, "user@proton.me") {
		t.Errorf("payloadFieldLengths leaked a value: %q", got)
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
	if !strings.Contains(err.Error(), "payload keys: ") || !strings.Contains(err.Error(), "Name=1") {
		t.Errorf("error %q does not name the payload field lengths", err)
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
