package drive

import (
	"errors"
	"testing"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
	proton "github.com/henrybear327/go-proton-api"
)

// TestKeyringRetriesAfterTransientFailure checks a failed unlock is never cached: the node must
// retry on the next call instead of staying poisoned until its listing expires.
func TestKeyringRetriesAfterTransientFailure(t *testing.T) {
	n := &Node{Link: proton.Link{LinkID: "l1"}, client: &Client{}}

	calls := 0
	want := &crypto.KeyRing{}
	orig := getKeyRing
	getKeyRing = func(link proton.Link, parentKR, addrKR *crypto.KeyRing) (*crypto.KeyRing, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("transient failure")
		}
		return want, nil
	}
	defer func() { getKeyRing = orig }()

	if _, err := n.Keyring(); err == nil {
		t.Fatal("first call should return the transient failure")
	}

	kr, err := n.Keyring()
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if kr != want {
		t.Errorf("kr = %v, want %v", kr, want)
	}
	if calls != 2 {
		t.Errorf("getKeyRing called %d times, want 2: the failure must not be cached", calls)
	}
}
