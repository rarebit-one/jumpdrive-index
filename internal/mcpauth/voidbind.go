// Package mcpauth authenticates callers of the /mcp endpoint. It offers two
// interchangeable authorizers that both reduce a request to an EFFECTIVE BEARER —
// the token string the MCP service resolves to a principal:
//
//   - bearer mode (the default, Bearer): the raw Authorization bearer, unchanged
//     from before this package existed.
//   - voidbind mode (Device): the caller presents `Authorization: Device
//     <op>~<possession>` (void-which-binds-go's scheme) plus, optionally, the
//     membership ops it knows in the rp.MembershipHeader. It is verified OFFLINE
//     against a pinned-users trust root re-read per request and a durable
//     membership op log (ADR-0007 of void-which-binds-go); on success the
//     authenticated USER id (ed25519:<hex>) becomes the effective bearer.
//
// Making the verified user id the effective bearer is what keeps the whole
// bearer-threaded service layer untouched: a voidbind principal is configured
// with its user public key as its Token, so the existing token→principal lookup
// resolves it — but only after the cryptographic Device credential has been
// verified, so the public key alone (no possession proof) authenticates nobody.
//
// Capability grants (rp.GrantHeader / rp.LegacyGrantHeader) are deliberately
// NOT consulted: Starchart's own principal model carries all per-caller
// authorization (spaces, approver rights), so a grant has nothing to narrow or
// elevate here.
package mcpauth

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rarebit-one/void-which-binds-go/enrolment"
	"github.com/rarebit-one/void-which-binds-go/identity"
	"github.com/rarebit-one/void-which-binds-go/rp"
)

// Authorizer reduces a request to the effective bearer the MCP service will
// resolve to a principal. An empty string means "no valid caller" — the handlers
// treat it as unauthenticated (fail-closed).
type Authorizer interface {
	EffectiveBearer(r *http.Request) string
}

// BearerFunc adapts a plain extractor (the legacy Bearer path) to Authorizer.
type BearerFunc func(r *http.Request) string

// EffectiveBearer implements Authorizer.
func (f BearerFunc) EffectiveBearer(r *http.Request) string { return f(r) }

// VoidbindScheme is the Authorization scheme name (case-insensitive on the wire).
const VoidbindScheme = "Device"

// TrustSource yields the current pinned trust root, invoked on EVERY request so
// that un-pinning a user (removing its line from the trust file) revokes it
// immediately. A nil map or an error fails CLOSED.
type TrustSource func() (rp.MemTrust, error)

// Voidbind is the Device-credential authorizer, mirroring farcaster's
// httpapi.Voidbind and unifi-mcp-go's authn.Voidbind, but reducing to the
// authenticated user id rather than a capability decision — Starchart's own
// principal model carries the per-caller authorization (spaces / approver rights).
//
// Membership (ADR-0007): a credential is the device's admitting op, and the
// device must be a CURRENT member of the pinned identity, evaluated over the
// ops recorded in the durable log plus the ops the request presents. A device
// a member has removed is refused even though its op is still in date, and
// because the remove is recorded in the log it stays refused across restarts.
//
// Ordering (void-which-binds-go#70): the library's VerifyWithPossession checks
// the credential in memory, then the possession proof, and only then commits
// the presented ops. A replayed public credential without the device key
// therefore teaches the log nothing.
type Voidbind struct {
	trust      TrustSource
	membership rp.Membership
	now        func() time.Time
}

// VoidbindOption configures a Voidbind authorizer.
type VoidbindOption func(*Voidbind)

// WithClock overrides the wall clock (tests drive cert/possession TTL windows).
func WithClock(now func() time.Time) VoidbindOption {
	return func(v *Voidbind) {
		if now != nil {
			v.now = now
		}
	}
}

// NewVoidbind builds the authorizer over a trust source and a membership op log
// (OpenMembership in production). A nil membership fails every request closed.
func NewVoidbind(trust TrustSource, membership rp.Membership, opts ...VoidbindOption) *Voidbind {
	v := &Voidbind{trust: trust, membership: membership, now: time.Now}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// EffectiveBearer verifies the Device credential and returns the authenticated
// user id (ed25519:<hex>) to use as the caller's principal token, or "" for any
// failure — missing header, wrong scheme, un-pinned user, removed or unadmitted
// device, bad signature, expired op, bad possession proof, too many presented
// ops, an unreadable or unwritable op log. Which check failed is never
// disclosed (that is reconnaissance); the caller sees only "no valid caller".
func (v *Voidbind) EffectiveBearer(r *http.Request) string {
	cred, ok := deviceCredential(r.Header.Get("Authorization"))
	if !ok {
		return ""
	}
	// Rebuild the trust root now, so an un-pin between requests is honoured
	// immediately. A nil map or any error fails closed.
	trust, err := v.trust()
	if err != nil || trust == nil {
		return ""
	}
	credential, proof, ok := strings.Cut(cred, enrolment.CredentialSeparator)
	if !ok || credential == "" || proof == "" {
		return ""
	}
	// The membership ops the device presents beside its credential; more than
	// rp.MaxPresentedOps is refused before anything is verified.
	presented, err := rp.ParseMembershipHeader(r.Header.Get(rp.MembershipHeader))
	if err != nil {
		return ""
	}
	// Check in memory → possession → Commit. A failed proof (or a log that
	// cannot be written) returns an error and persists nothing.
	auth, err := (rp.Verifier{Trust: trust, Membership: v.membership}).
		VerifyWithPossession(credential, proof, presented, v.now())
	if err != nil {
		return ""
	}
	return auth.UserID
}

// OpenMembership opens (creating, 0700) the durable membership op log under dir
// and proves it is writable now, so a misconfigured path fails at startup
// rather than refusing every caller at request time.
func OpenMembership(dir string) (*rp.FileMembership, error) {
	m, err := rp.NewFileMembership(dir)
	if err != nil {
		return nil, fmt.Errorf("mcpauth membership: %w", err)
	}
	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return nil, fmt.Errorf("mcpauth membership: %s is not writable: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	if err := os.Remove(name); err != nil {
		return nil, fmt.Errorf("mcpauth membership: cleaning probe in %s: %w", dir, err)
	}
	return m, nil
}

// PinnedUsersFromFile returns a TrustSource that RE-READS a file of pinned user
// ids on every call — so removing a line revokes that user immediately. Each
// non-empty, non-`#` line is one rendered ed25519 public key ("ed25519:<hex>").
// A MISSING file yields an EMPTY trust (fail-closed: nobody pinned, nobody
// admitted), never an error that reads as "let them in".
func PinnedUsersFromFile(path string) TrustSource {
	return func() (rp.MemTrust, error) {
		trust := rp.NewMemTrust()
		data, err := os.ReadFile(path) //nolint:gosec // G304: trusted operator config path
		if errors.Is(err, os.ErrNotExist) {
			return trust, nil // empty trust → refuses everyone
		}
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			pub, err := identity.ParsePublicKey(line)
			if err != nil {
				return nil, fmt.Errorf("mcpauth trust: %q is not a valid pinned key: %w", line, err)
			}
			trust.Pin(line, pub)
		}
		return trust, nil
	}
}

// deviceCredential extracts the value of an `Authorization: Device <credential>`
// header, matching the scheme case-insensitively. Returns ("", false) otherwise.
func deviceCredential(header string) (string, bool) {
	const prefix = VoidbindScheme + " " // "Device "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	val := strings.TrimSpace(header[len(prefix):])
	return val, val != ""
}
