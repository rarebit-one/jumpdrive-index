package mcpauth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/mcpauth"
	"github.com/rarebit-one/voidbind-go/device"
	"github.com/rarebit-one/voidbind-go/enrolment"
	"github.com/rarebit-one/voidbind-go/identity"
)

// enrolled builds a device store enrolled under a fresh user identity and returns
// the store plus the user's rendered public key (the pin line a trust file needs).
func enrolled(t *testing.T) (*device.Store, string) {
	t.Helper()
	userPub, userPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("user key: %v", err)
	}
	store, err := device.NewStore(device.StoreOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	dev, err := store.Generate("caller", false)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cert, err := enrolment.SignCert(userPriv, dev.PublicKey, "", time.Now(), 0)
	if err != nil {
		t.Fatalf("SignCert: %v", err)
	}
	if _, err := store.Enrol(cert); err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	return store, identity.FormatPublicKey(userPub)
}

// reqWithCredential builds a request carrying a fresh Device credential from store.
func reqWithCredential(t *testing.T, store *device.Store) *http.Request {
	t.Helper()
	cred, err := store.Credential(time.Now(), 0)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	r := newReq(t)
	r.Header.Set("Authorization", "Device "+cred)
	return r
}

func newReq(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "http://x/mcp", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return r
}

// writeTrust writes a pinned-users file with the given lines and returns its path.
func writeTrust(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "trust.txt")
	data := ""
	for _, l := range lines {
		data += l + "\n"
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatalf("write trust: %v", err)
	}
	return p
}

// TestVoidbindAuthenticatesToUserID proves a valid Device credential reduces to
// the pinned user id (the effective bearer), while a missing credential yields "".
func TestVoidbindAuthenticatesToUserID(t *testing.T) {
	store, userID := enrolled(t)
	trustPath := writeTrust(t, userID)
	v := mcpauth.NewVoidbind(mcpauth.PinnedUsersFromFile(trustPath))

	if got := v.EffectiveBearer(reqWithCredential(t, store)); got != userID {
		t.Errorf("effective bearer = %q, want the pinned user id %q", got, userID)
	}
	if got := v.EffectiveBearer(newReq(t)); got != "" {
		t.Errorf("no credential: effective bearer = %q, want empty (fail-closed)", got)
	}
}

// TestVoidbindRevokesOnUnpin proves the trust file is re-read per request: once the
// user's line is gone, the same valid credential no longer authenticates.
func TestVoidbindRevokesOnUnpin(t *testing.T) {
	store, userID := enrolled(t)
	trustPath := writeTrust(t, userID)
	v := mcpauth.NewVoidbind(mcpauth.PinnedUsersFromFile(trustPath))

	if got := v.EffectiveBearer(reqWithCredential(t, store)); got != userID {
		t.Fatalf("pre-revoke: bearer = %q, want %q", got, userID)
	}
	// Un-pin the user (empty the trust file) — takes effect on the next request.
	if err := os.WriteFile(trustPath, []byte("# revoked\n"), 0o600); err != nil {
		t.Fatalf("rewrite trust: %v", err)
	}
	if got := v.EffectiveBearer(reqWithCredential(t, store)); got != "" {
		t.Errorf("post-revoke: bearer = %q, want empty", got)
	}
}

// TestVoidbindRefusesUnpinnedUser proves a valid credential from an UN-pinned user
// (a trust file that pins someone else) is refused.
func TestVoidbindRefusesUnpinnedUser(t *testing.T) {
	store, _ := enrolled(t)
	_, otherUser := enrolled(t) // a different, unrelated user is the only pin
	v := mcpauth.NewVoidbind(mcpauth.PinnedUsersFromFile(writeTrust(t, otherUser)))

	if got := v.EffectiveBearer(reqWithCredential(t, store)); got != "" {
		t.Errorf("un-pinned user: bearer = %q, want empty", got)
	}
}
