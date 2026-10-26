// Package config loads and validates configuration at boot. Following
// jumpdrive-broker's discipline: Load takes a getenv func (for testability),
// accumulates every problem and returns errors.Join (so a fresh deploy is fixed
// once, not one restart at a time), and is DEFAULT-DENY — an unrecognised enum
// value is an error, never a silent fallback, because "an operator believes a
// thing is armed when it is not" is the failure to prevent.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/secret"
)

// Mode selects the run phase. Migrations run as a separate phase, never at boot
// (racing writers + opaque health timeouts).
type Mode string

// Mode values.
const (
	ModeServe   Mode = "serve"
	ModeMigrate Mode = "migrate"
)

// Backend selects the storage adapter.
type Backend string

// Backend values.
const (
	BackendSQLite   Backend = "sqlite"   // homelab / Starchart
	BackendPostgres Backend = "postgres" // hosted
)

// IdentityMode selects the access model.
type IdentityMode string

// IdentityMode values.
const (
	IdentityStarchart IdentityMode = "starchart" // self-contained principals/spaces/lenses
	IdentityJumpdrive IdentityMode = "jumpdrive" // delegate to Jumpdrive
)

// JumpdriveVerify selects how the jumpdrive identity backend trusts a
// credential (JDX_JUMPDRIVE_VERIFY).
type JumpdriveVerify string

// JumpdriveVerify values.
const (
	// VerifyJWKS verifies the ES256 credential locally against the control
	// plane's JWKS (the production end state; no callback on the hot path).
	VerifyJWKS JumpdriveVerify = "jwks"
	// VerifyAuthorize asks the control plane's /authorize (resolver of record).
	VerifyAuthorize JumpdriveVerify = "authorize"
	// VerifyDual runs both, enforces the intersection and logs mismatches.
	VerifyDual JumpdriveVerify = "dual"
)

// jumpdriveIndexPath is the control-plane route the index's JDX_JUMPDRIVE_URL
// points at (<console>/internal/index); the issuer defaults to what precedes it.
const jumpdriveIndexPath = "/internal/index"

// Config is the whole validated configuration.
type Config struct {
	Mode         Mode
	Backend      Backend
	IdentityMode IdentityMode

	// DSN is the Postgres URL (Backend=postgres) or the SQLite path (Backend=sqlite).
	DSN string

	// PrincipalsFile is a JSON file describing the starchart access model
	// (principals + restricted deny-types). Used when IdentityMode=starchart.
	PrincipalsFile string
	// LegacyStarchart (JDX_LEGACY_STARCHART=1) is the temporary cutover escape
	// hatch that lets a Postgres serve run the starchart identity model. Without
	// it that combination is refused: starchart's static shared principals are
	// the homelab (SQLite) identity model, and the hosted index takes per-caller
	// identity (jumpdrive-web docs/index-principal-adr.md, Decision 5.6). It is
	// deleted with the composite model at the end of the rollout.
	LegacyStarchart bool

	// JumpdriveURL is the control plane's index route, <console>/internal/index
	// (the model appends /authorize and /jwks.json). Used when
	// IdentityMode=jumpdrive.
	JumpdriveURL string
	// JumpdriveSecret is the index→control-plane service bearer for /authorize,
	// equal to the control plane's JUMPDRIVE_INDEX_AUTHORIZE_SECRET. REQUIRED
	// when IdentityMode=jumpdrive (in every verify mode, so an operator can flip
	// to authorize or dual during an incident without a second secret change).
	JumpdriveSecret secret.Value
	// JumpdriveVerify is jwks (default) | authorize | dual.
	JumpdriveVerify JumpdriveVerify
	// JumpdriveIssuer is the `iss` a credential must carry: the control plane's
	// base URL (its JUMPDRIVE_BASE_URL). Defaults to JumpdriveURL minus a
	// trailing /internal/index; required when that does not apply.
	JumpdriveIssuer string

	// HTTPAddr is where the service listens. Loopback is the safe default.
	HTTPAddr string
	// AuthEnabled gates the bearer-token requirement. Per heyarr ADR-0011, serving
	// unauthenticated on a routable (non-loopback) address is a hard refusal.
	AuthEnabled bool

	// MCPAuthMode selects how /mcp callers authenticate: "bearer" (the default —
	// identical to before this field existed) resolves a shared bearer to a
	// principal; "voidbind" requires `Authorization: Device <op>~<possession>`
	// verified offline against MCPVoidbindTrustFile and the membership op log in
	// MCPVoidbindMembershipDir, and the authenticated user
	// id becomes the effective bearer (a voidbind principal sets its Token to its
	// user public key).
	MCPAuthMode string
	// MCPVoidbindTrustFile is the pinned-users file (one rendered ed25519 public
	// key per line), re-read per request so un-pinning revokes immediately.
	// REQUIRED when MCPAuthMode is "voidbind".
	MCPVoidbindTrustFile string
	// MCPVoidbindMembershipDir is the durable membership op log (one append-only
	// file per pinned identity). It is what makes a device REMOVAL stick across
	// restarts, so it must be writable, persistent state — opened at startup,
	// which fails if it cannot be. Defaults to "jumpdrive-index-membership"
	// relative to the working directory (like the SQLite default path); set it
	// under the service's state directory in a real deploy.
	MCPVoidbindMembershipDir string

	// Thresholds are the resolve vector bands (validated: AutoMerge > Review).
	Thresholds domain.Thresholds

	// HeyarrURL is the base URL of a heyarr MCP endpoint (…/api/v1/mcp) the index
	// links to by reference; HeyarrToken is its bearer. Both are OPTIONAL — an
	// unset URL simply disables heyarr reconciliation. A token set with no URL is
	// a misconfiguration (a credential armed with nowhere to go) and is rejected.
	HeyarrURL   string
	HeyarrToken secret.Value
}

// Load reads and validates configuration from getenv (falls back to os.Getenv
// when nil). Env keys are prefixed JDX_.
func Load(getenv func(string) string) (*Config, error) {
	if getenv == nil {
		return nil, errors.New("config: nil getenv")
	}
	get := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}

	var errs []error
	c := &Config{
		DSN:                      getenv("JDX_DSN"),
		PrincipalsFile:           getenv("JDX_PRINCIPALS_FILE"),
		JumpdriveURL:             strings.TrimRight(getenv("JDX_JUMPDRIVE_URL"), "/"),
		JumpdriveSecret:          secret.Value(getenv("JDX_JUMPDRIVE_SECRET")),
		JumpdriveIssuer:          getenv("JDX_JUMPDRIVE_ISSUER"), // verbatim: iss is compared exactly
		HTTPAddr:                 get("JDX_HTTP_ADDR", "127.0.0.1:8090"),
		MCPAuthMode:              strings.ToLower(get("JDX_MCP_AUTH_MODE", "bearer")),
		MCPVoidbindTrustFile:     getenv("JDX_MCP_VOIDBIND_TRUST_FILE"),
		MCPVoidbindMembershipDir: get("JDX_MCP_VOIDBIND_MEMBERSHIP_DIR", "jumpdrive-index-membership"),
		HeyarrURL:                getenv("JDX_HEYARR_URL"),
		HeyarrToken:              secret.Value(getenv("JDX_HEYARR_TOKEN")),
		Thresholds: domain.Thresholds{
			AutoMerge: 0.94,
			Review:    0.86,
		},
	}

	// Enums — default-deny (a blank OR unknown value is an error, never a fallback).
	switch m := Mode(get("JDX_MODE", "serve")); m {
	case ModeServe, ModeMigrate:
		c.Mode = m
	default:
		errs = append(errs, fmt.Errorf("JDX_MODE: unrecognised %q (want serve|migrate)", m))
	}
	switch b := Backend(get("JDX_BACKEND", "sqlite")); b {
	case BackendSQLite, BackendPostgres:
		c.Backend = b
	default:
		errs = append(errs, fmt.Errorf("JDX_BACKEND: unrecognised %q (want sqlite|postgres)", b))
	}
	switch id := IdentityMode(get("JDX_IDENTITY", "starchart")); id {
	case IdentityStarchart, IdentityJumpdrive:
		c.IdentityMode = id
	default:
		errs = append(errs, fmt.Errorf("JDX_IDENTITY: unrecognised %q (want starchart|jumpdrive)", id))
	}

	// AuthEnabled: strict 1/0/true/false, no silent "off" on a typo.
	switch v := strings.ToLower(get("JDX_AUTH", "true")); v {
	case "1", "true":
		c.AuthEnabled = true
	case "0", "false":
		c.AuthEnabled = false
	default:
		errs = append(errs, fmt.Errorf("JDX_AUTH: unrecognised %q (want 1|0|true|false)", v))
	}

	// LegacyStarchart: strict like JDX_AUTH, defaulting off.
	switch v := strings.ToLower(get("JDX_LEGACY_STARCHART", "false")); v {
	case "1", "true":
		c.LegacyStarchart = true
	case "0", "false":
	default:
		errs = append(errs, fmt.Errorf("JDX_LEGACY_STARCHART: unrecognised %q (want 1|0|true|false)", v))
	}

	// MCPAuthMode: a closed set, default-deny on anything else; voidbind needs a
	// pinned-users file to have anyone to admit.
	switch c.MCPAuthMode {
	case "bearer":
	case "voidbind":
		if c.MCPVoidbindTrustFile == "" {
			errs = append(errs, errors.New("JDX_MCP_VOIDBIND_TRUST_FILE: required when JDX_MCP_AUTH_MODE=voidbind"))
		}
	default:
		errs = append(errs, fmt.Errorf("JDX_MCP_AUTH_MODE: unrecognised %q (want bearer|voidbind)", c.MCPAuthMode))
	}

	if err := c.Thresholds.Validate(); err != nil {
		errs = append(errs, err)
	}

	// Mode-aware required credentials: a serve on Postgres needs a DSN; migrate
	// needs one too. SQLite defaults its path, so a blank DSN is allowed there.
	if c.Backend == BackendPostgres && c.DSN == "" {
		errs = append(errs, errors.New("JDX_DSN: required when JDX_BACKEND=postgres"))
	}
	// Identity-aware: the jumpdrive backend needs the control plane's index
	// route, its service secret, a verify mode and (for jwks/dual) an issuer.
	switch v := JumpdriveVerify(strings.ToLower(get("JDX_JUMPDRIVE_VERIFY", string(VerifyJWKS)))); v {
	case VerifyJWKS, VerifyAuthorize, VerifyDual:
		c.JumpdriveVerify = v
	default:
		errs = append(errs, fmt.Errorf("JDX_JUMPDRIVE_VERIFY: unrecognised %q (want jwks|authorize|dual)", v))
	}
	if c.Mode == ModeServe && c.IdentityMode == IdentityJumpdrive {
		errs = append(errs, c.validateJumpdrive()...)
	}
	// Starchart is retired for hosted serving: a Postgres serve with the static
	// shared principal is refused unless the cutover escape hatch is set. Migrate
	// builds no access model, so it is exempt (its job declares no identity).
	if c.Mode == ModeServe && c.Backend == BackendPostgres && c.IdentityMode == IdentityStarchart && !c.LegacyStarchart {
		errs = append(errs, errors.New(
			"JDX_IDENTITY: starchart is the homelab (sqlite) identity model; refusing it with JDX_BACKEND=postgres "+
				"(use JDX_IDENTITY=jumpdrive, or set JDX_LEGACY_STARCHART=1 for the cutover window)"))
	}
	// A heyarr token armed with no endpoint is a misconfiguration, not a silent
	// no-op — the credential would never be sent, so fail loud (default-deny).
	if !c.HeyarrToken.IsZero() && c.HeyarrURL == "" {
		errs = append(errs, errors.New("JDX_HEYARR_TOKEN: set with no JDX_HEYARR_URL (a credential with nowhere to go)"))
	}

	// ADR-0011: refuse to serve unauthenticated on a routable address.
	if c.Mode == ModeServe && !c.AuthEnabled && isRoutable(c.HTTPAddr) {
		errs = append(errs, fmt.Errorf(
			"refusing to serve unauthenticated on routable address %q: enable JDX_AUTH or bind loopback", c.HTTPAddr))
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func (c *Config) validateJumpdrive() []error {
	var errs []error
	if c.JumpdriveURL == "" {
		errs = append(errs, errors.New("JDX_JUMPDRIVE_URL: required when JDX_IDENTITY=jumpdrive"))
	} else if u, err := url.Parse(c.JumpdriveURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		errs = append(errs, fmt.Errorf("JDX_JUMPDRIVE_URL: %q is not an http(s) URL", c.JumpdriveURL))
	}
	if c.JumpdriveSecret.IsZero() {
		errs = append(errs, errors.New("JDX_JUMPDRIVE_SECRET: required when JDX_IDENTITY=jumpdrive (the control plane's JUMPDRIVE_INDEX_AUTHORIZE_SECRET)"))
	}
	if c.JumpdriveIssuer == "" && strings.HasSuffix(c.JumpdriveURL, jumpdriveIndexPath) {
		c.JumpdriveIssuer = strings.TrimSuffix(c.JumpdriveURL, jumpdriveIndexPath)
	}
	if c.JumpdriveIssuer == "" && c.JumpdriveVerify != VerifyAuthorize {
		errs = append(errs, fmt.Errorf(
			"JDX_JUMPDRIVE_ISSUER: required with JDX_JUMPDRIVE_VERIFY=%s when JDX_JUMPDRIVE_URL does not end in %s", c.JumpdriveVerify, jumpdriveIndexPath))
	}
	return errs
}

// isRoutable reports whether addr binds something other than loopback. A bare
// port or an empty host ("" / ":8090") binds all interfaces and is routable; an
// explicit 127.0.0.1 / ::1 / localhost host is not.
func isRoutable(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]") // strip IPv6 brackets
	switch host {
	case "", "0.0.0.0", "::":
		return true
	case "127.0.0.1", "::1", "localhost":
		return false
	default:
		return !strings.HasPrefix(host, "127.")
	}
}
