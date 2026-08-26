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
	"strings"

	"github.com/rarebit-one/jumpdrive-index/internal/domain"
)

// Mode selects the run phase. Migrations run as a separate phase, never at boot
// (racing writers + opaque health timeouts).
type Mode string

const (
	ModeServe   Mode = "serve"
	ModeMigrate Mode = "migrate"
)

// Backend selects the storage adapter.
type Backend string

const (
	BackendSQLite   Backend = "sqlite"   // homelab / Starchart
	BackendPostgres Backend = "postgres" // hosted
)

// IdentityMode selects the access model.
type IdentityMode string

const (
	IdentityStarchart IdentityMode = "starchart" // self-contained principals/spaces/lenses
	IdentityJumpdrive IdentityMode = "jumpdrive" // delegate to Jumpdrive
)

// Config is the whole validated configuration.
type Config struct {
	Mode         Mode
	Backend      Backend
	IdentityMode IdentityMode

	// DSN is the Postgres URL (Backend=postgres) or the SQLite path (Backend=sqlite).
	DSN string

	// HTTPAddr is where the service listens. Loopback is the safe default.
	HTTPAddr string
	// AuthEnabled gates the bearer-token requirement. Per heyarr ADR-0011, serving
	// unauthenticated on a routable (non-loopback) address is a hard refusal.
	AuthEnabled bool

	// Thresholds are the resolve vector bands (validated: AutoMerge > Review).
	Thresholds domain.Thresholds
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
		DSN:      getenv("JDX_DSN"),
		HTTPAddr: get("JDX_HTTP_ADDR", "127.0.0.1:8090"),
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

	if err := c.Thresholds.Validate(); err != nil {
		errs = append(errs, err)
	}

	// Mode-aware required credentials: a serve on Postgres needs a DSN; migrate
	// needs one too. SQLite defaults its path, so a blank DSN is allowed there.
	if c.Backend == BackendPostgres && c.DSN == "" {
		errs = append(errs, errors.New("JDX_DSN: required when JDX_BACKEND=postgres"))
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
