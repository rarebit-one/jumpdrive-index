package mcpauth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/mcpauth"
	"github.com/rarebit-one/void-which-binds-go/device"
	"github.com/rarebit-one/void-which-binds-go/enrolment"
	"github.com/rarebit-one/void-which-binds-go/identity"
	"github.com/rarebit-one/void-which-binds-go/rp"
	"github.com/rarebit-one/void-which-binds-go/testvectors"
)

// vbNow is the fixed clock the membership tests run at; ops are issued before it.
var vbNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

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
	v := mcpauth.NewVoidbind(mcpauth.PinnedUsersFromFile(trustPath), rp.NewMemMembership())

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
	v := mcpauth.NewVoidbind(mcpauth.PinnedUsersFromFile(trustPath), rp.NewMemMembership())

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
	v := mcpauth.NewVoidbind(mcpauth.PinnedUsersFromFile(writeTrust(t, otherUser)), rp.NewMemMembership())

	if got := v.EffectiveBearer(reqWithCredential(t, store)); got != "" {
		t.Errorf("un-pinned user: bearer = %q, want empty", got)
	}
}

// --- membership (ADR-0007) --------------------------------------------------

// fleet is one pinned identity with two devices: genesis admits A, and A (a
// member) admits B. Ops are issued before vbNow so every prev is issued no later
// than the op that cites it.
type fleet struct {
	userPriv     ed25519.PrivateKey
	userID       string
	aPriv, bPriv ed25519.PrivateKey
	aID, bID     string
	addA, addB   string
}

func newFleet(t *testing.T) fleet {
	t.Helper()
	f := fleet{}
	_, f.userPriv = mustKey(t)
	f.userID = identity.FormatPublicKey(f.userPriv.Public().(ed25519.PublicKey))
	var aPub, bPub ed25519.PublicKey
	aPub, f.aPriv = mustKey(t)
	bPub, f.bPriv = mustKey(t)
	f.aID, f.bID = identity.FormatPublicKey(aPub), identity.FormatPublicKey(bPub)
	t0 := vbNow.Add(-time.Hour)
	f.addA = mustOp(t, f.userPriv, f.userID, enrolment.OpAdd, f.aID, nil, t0, enrolment.CertLifetime)
	f.addB = mustOp(t, f.aPriv, f.userID, enrolment.OpAdd, f.bID,
		[]string{enrolment.OpHash(f.addA)}, t0.Add(time.Minute), enrolment.CertLifetime)
	return f
}

// removeB is A's member-signed remove of B, citing B's admission. With a fleet
// high-water of two devices one member signature is the quorum (ADR-0008).
func (f fleet) removeB(t *testing.T) string {
	t.Helper()
	return mustOp(t, f.aPriv, f.userID, enrolment.OpRemove, f.bID,
		[]string{enrolment.OpHash(f.addB)}, vbNow.Add(-30*time.Minute), 0)
}

func (f fleet) trust() mcpauth.TrustSource {
	trust := rp.NewMemTrust()
	trust.Pin(f.userID, f.userPriv.Public().(ed25519.PublicKey))
	return func() (rp.MemTrust, error) { return trust, nil }
}

func (f fleet) authorizer(m rp.Membership) *mcpauth.Voidbind {
	return mcpauth.NewVoidbind(f.trust(), m, mcpauth.WithClock(func() time.Time { return vbNow }))
}

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return pub, priv
}

func mustOp(t *testing.T, signer ed25519.PrivateKey, usr string, kind enrolment.OpKind, dev string, prev []string, iat time.Time, lifetime time.Duration) string {
	t.Helper()
	tok, err := enrolment.SignOp(signer, usr, kind, dev, "", prev, iat, lifetime)
	if err != nil {
		t.Fatalf("SignOp %s: %v", kind, err)
	}
	return tok
}

// deviceReq builds a request with `Authorization: Device <cred>`, presenting ops
// in the membership header.
func deviceReq(t *testing.T, cred string, ops ...string) *http.Request {
	t.Helper()
	r := newReq(t)
	r.Header.Set("Authorization", "Device "+cred)
	if len(ops) > 0 {
		r.Header.Set(rp.MembershipHeader, rp.FormatMembershipHeader(ops))
	}
	return r
}

// opReq is a Device request for admitting op `cred`, possession proven by holder.
func opReq(t *testing.T, cred string, holder ed25519.PrivateKey, ops ...string) *http.Request {
	t.Helper()
	proof, err := enrolment.SignPossession(holder, cred, vbNow, 0)
	if err != nil {
		t.Fatalf("SignPossession: %v", err)
	}
	return deviceReq(t, cred+enrolment.CredentialSeparator+proof, ops...)
}

// countingMembership counts how often Record reaches the log.
type countingMembership struct {
	rp.Membership
	mu      sync.Mutex
	records int
}

func (c *countingMembership) Record(usr string, ops []string) error {
	c.mu.Lock()
	c.records++
	c.mu.Unlock()
	return c.Membership.Record(usr, ops)
}

func (c *countingMembership) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.records
}

// TestVoidbindRemovedDeviceRefusedAcrossRestart: a device a member removed is
// refused although its op is in date and its possession proof valid; the remove
// is persisted to the file log, so a restarted authorizer (a fresh log over the
// same directory) still refuses it when nobody presents the remove again.
func TestVoidbindRemovedDeviceRefusedAcrossRestart(t *testing.T) {
	f := newFleet(t)
	dir := t.TempDir()
	log, err := mcpauth.OpenMembership(dir)
	if err != nil {
		t.Fatal(err)
	}
	v := f.authorizer(log)

	if got := v.EffectiveBearer(opReq(t, f.addB, f.bPriv, f.addA)); got != f.userID {
		t.Fatalf("B before removal: bearer = %q, want %q", got, f.userID)
	}
	// A, a member, presents its remove of B.
	if got := v.EffectiveBearer(opReq(t, f.addA, f.aPriv, f.addB, f.removeB(t))); got != f.userID {
		t.Fatalf("A presenting the remove: bearer = %q", got)
	}
	if got := v.EffectiveBearer(opReq(t, f.addB, f.bPriv, f.addA)); got != "" {
		t.Fatalf("removed B: bearer = %q, want empty", got)
	}

	reopened, err := mcpauth.OpenMembership(dir)
	if err != nil {
		t.Fatal(err)
	}
	v2 := f.authorizer(reopened)
	if got := v2.EffectiveBearer(opReq(t, f.addB, f.bPriv, f.addA)); got != "" {
		t.Fatalf("after restart, removed B: bearer = %q, want empty", got)
	}
	if got := v2.EffectiveBearer(opReq(t, f.addA, f.aPriv)); got != f.userID {
		t.Fatalf("after restart, A: bearer = %q, want %q", got, f.userID)
	}
}

// TestVoidbindReplayWithoutPossessionRecordsNothing: a member's public admitting
// op replayed with a proof the replayer cannot validly make is refused, and the
// genuine ops it presents never reach the log (possession before commit).
func TestVoidbindReplayWithoutPossessionRecordsNothing(t *testing.T) {
	f := newFleet(t)
	_, attacker := mustKey(t)
	expired, err := enrolment.SignPossession(f.bPriv, f.addB, vbNow.Add(-time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	boundElsewhere, err := enrolment.SignPossession(f.bPriv, f.addA, vbNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	sep := enrolment.CredentialSeparator
	cases := map[string]func() *http.Request{
		"proof by another key":   func() *http.Request { return opReq(t, f.addB, attacker, f.addA, f.removeB(t)) },
		"expired proof":          func() *http.Request { return deviceReq(t, f.addB+sep+expired, f.addA) },
		"proof bound to another": func() *http.Request { return deviceReq(t, f.addB+sep+boundElsewhere, f.addA) },
		"garbage proof":          func() *http.Request { return deviceReq(t, f.addB+sep+"not.a-proof", f.addA) },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			log := &countingMembership{Membership: rp.NewMemMembership()}
			if got := f.authorizer(log).EffectiveBearer(mk()); got != "" {
				t.Fatalf("bearer = %q, want empty", got)
			}
			if n := log.count(); n != 0 {
				t.Errorf("Record reached %d time(s) without possession", n)
			}
			if ops, _ := log.Ops(f.userID); len(ops) != 0 {
				t.Errorf("recorded %d op(s) without possession", len(ops))
			}
		})
	}

	// Control: the same request with a genuine proof records addA and addB.
	log := &countingMembership{Membership: rp.NewMemMembership()}
	if got := f.authorizer(log).EffectiveBearer(opReq(t, f.addB, f.bPriv, f.addA)); got != f.userID {
		t.Fatalf("genuine B: bearer = %q", got)
	}
	if ops, _ := log.Ops(f.userID); len(ops) != 2 {
		t.Errorf("genuine request recorded %d op(s), want 2", len(ops))
	}
}

// TestVoidbindFailsClosed covers the log and header edges: no log, a log that
// cannot be written, and more presented ops than rp.MaxPresentedOps.
func TestVoidbindFailsClosed(t *testing.T) {
	f := newFleet(t)
	if got := f.authorizer(nil).EffectiveBearer(opReq(t, f.addA, f.aPriv)); got != "" {
		t.Errorf("nil membership: bearer = %q, want empty", got)
	}
	if got := f.authorizer(failingRecord{rp.NewMemMembership()}).EffectiveBearer(opReq(t, f.addB, f.bPriv, f.addA)); got != "" {
		t.Errorf("unwritable log: bearer = %q, want empty", got)
	}
	ops := make([]string, rp.MaxPresentedOps+1)
	for i := range ops {
		ops[i] = f.addA
	}
	if got := f.authorizer(rp.NewMemMembership()).EffectiveBearer(opReq(t, f.addB, f.bPriv, ops...)); got != "" {
		t.Errorf("over-cap ops: bearer = %q, want empty", got)
	}
}

type failingRecord struct{ rp.Membership }

func (failingRecord) Record(string, []string) error { return errors.New("boom") }

// TestOpenMembershipFailsOnUnusableDir: a path that cannot hold the log (here, a
// regular file where the directory should be) is an error at open time.
func TestOpenMembershipFailsOnUnusableDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mcpauth.OpenMembership(file); err == nil {
		t.Error("OpenMembership over a regular file: want error")
	}
	if _, err := mcpauth.OpenMembership(""); err == nil {
		t.Error("OpenMembership(\"\"): want error")
	}
}

// TestVoidbindTypedAndUntypedDeviceScheme replays void-which-binds-go's golden
// Device-scheme vectors byte-for-byte: the legacy untyped credential and the
// typed one minted since ADR-0009 phase 2 (v0.16) both authenticate to the
// vector's user id. A freshly minted credential is typed and verifies too.
func TestVoidbindTypedAndUntypedDeviceScheme(t *testing.T) {
	for name, raw := range map[string][]byte{
		"untyped": testvectors.DeviceSchemeVector(),
		"typed":   testvectors.DeviceSchemeTypedVector(),
	} {
		t.Run(name, func(t *testing.T) {
			var vec struct {
				Header   string `json:"authorization_header"`
				Cert     string `json:"cert"`
				UserKey  string `json:"user_public_key"`
				WantUser string `json:"expected_principal_user_id"`
				Now      int64  `json:"possession_now_unix"`
			}
			if err := json.Unmarshal(raw, &vec); err != nil {
				t.Fatal(err)
			}
			if typed := certHasTyp(t, vec.Cert); typed != (name == "typed") {
				t.Fatalf("vector %s: cert typ present = %v", name, typed)
			}
			v := mcpauth.NewVoidbind(
				mcpauth.PinnedUsersFromFile(writeTrust(t, vec.UserKey)),
				rp.NewMemMembership(),
				mcpauth.WithClock(func() time.Time { return time.Unix(vec.Now, 0).UTC() }),
			)
			r := newReq(t)
			r.Header.Set("Authorization", strings.TrimPrefix(vec.Header, "Authorization: "))
			if got := v.EffectiveBearer(r); got != vec.WantUser {
				t.Errorf("bearer = %q, want %q", got, vec.WantUser)
			}
		})
	}

	store, userID := enrolled(t)
	cred, err := store.Credential(time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	cert, _, _ := strings.Cut(cred, enrolment.CredentialSeparator)
	if !certHasTyp(t, cert) {
		t.Fatal("a freshly minted cert should be typed")
	}
	v := mcpauth.NewVoidbind(mcpauth.PinnedUsersFromFile(writeTrust(t, userID)), rp.NewMemMembership())
	if got := v.EffectiveBearer(reqWithCredential(t, store)); got != userID {
		t.Errorf("fresh typed credential: bearer = %q, want %q", got, userID)
	}
}

func certHasTyp(t *testing.T, token string) bool {
	t.Helper()
	body, _, _ := strings.Cut(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("decode cert body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("cert body: %v", err)
	}
	_, ok := m["typ"]
	return ok
}
