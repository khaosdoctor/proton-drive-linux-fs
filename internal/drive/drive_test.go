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

// TestAttrsKnown checks that AttrsKnown reflects whether ResolveAttrs has successfully decrypted
// the XAttr: false for a freshly listed node (still using encrypted size), true after resolution.
func TestAttrsKnown(t *testing.T) {
	n := &Node{Link: proton.Link{LinkID: "l1"}, client: &Client{}, size: 999}

	if n.AttrsKnown() {
		t.Fatal("freshly created node should not have attrs known")
	}

	// Simulate a successful resolution.
	n.attrMu.Lock()
	n.attrsKnown = true
	n.size = 100
	n.attrMu.Unlock()

	if !n.AttrsKnown() {
		t.Fatal("node should have attrs known after resolution")
	}
	if got := n.Size(); got != 100 {
		t.Errorf("Size() = %d, want 100", got)
	}
}
