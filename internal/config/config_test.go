package config_test

import (
	"strings"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/config"
)

// env builds a getenv from a map, so a test drives Load without touching the
// process environment.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestHeyarrOptional(t *testing.T) {
	// No heyarr config at all is fine — reconciliation is simply disabled.
	c, err := config.Load(env(map[string]string{}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HeyarrURL != "" || !c.HeyarrToken.IsZero() {
		t.Errorf("heyarr should default to unset, got url=%q token-set=%v", c.HeyarrURL, !c.HeyarrToken.IsZero())
	}
}

func TestHeyarrURLAndToken(t *testing.T) {
	c, err := config.Load(env(map[string]string{
		"JDX_HEYARR_URL":   "https://heyarr.example/api/v1/mcp",
		"JDX_HEYARR_TOKEN": "s3cr3t",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HeyarrURL != "https://heyarr.example/api/v1/mcp" {
		t.Errorf("HeyarrURL = %q", c.HeyarrURL)
	}
	if c.HeyarrToken.Reveal() != "s3cr3t" {
		t.Error("HeyarrToken not loaded")
	}
	// The token must not surface in any default formatting path.
	if strings.Contains(c.HeyarrToken.String(), "s3cr3t") {
		t.Error("token leaked through String()")
	}
}

func TestHeyarrTokenWithoutURLIsRejected(t *testing.T) {
	_, err := config.Load(env(map[string]string{"JDX_HEYARR_TOKEN": "orphan"}))
	if err == nil {
		t.Fatal("a token with no URL should be rejected (a credential with nowhere to go)")
	}
	if !strings.Contains(err.Error(), "JDX_HEYARR_TOKEN") {
		t.Errorf("error should name JDX_HEYARR_TOKEN, got %v", err)
	}
}

func TestVoidbindMembershipDir(t *testing.T) {
	base := map[string]string{
		"JDX_MCP_AUTH_MODE":           "voidbind",
		"JDX_MCP_VOIDBIND_TRUST_FILE": "/etc/starchart/pinned-users",
	}
	c, err := config.Load(env(base))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MCPVoidbindMembershipDir != "jumpdrive-index-membership" {
		t.Errorf("default MCPVoidbindMembershipDir = %q", c.MCPVoidbindMembershipDir)
	}
	base["JDX_MCP_VOIDBIND_MEMBERSHIP_DIR"] = "/var/lib/starchart/voidbind-membership"
	if c, err = config.Load(env(base)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MCPVoidbindMembershipDir != "/var/lib/starchart/voidbind-membership" {
		t.Errorf("MCPVoidbindMembershipDir = %q", c.MCPVoidbindMembershipDir)
	}
}

// jumpdriveEnv is a complete jumpdrive-identity serve config; tests delete or
// override keys from it.
func jumpdriveEnv(edits map[string]string) map[string]string {
	m := map[string]string{
		"JDX_IDENTITY":         "jumpdrive",
		"JDX_JUMPDRIVE_URL":    "https://console.jumpdrive.test/internal/index",
		"JDX_JUMPDRIVE_SECRET": "svc-secret",
	}
	for k, v := range edits {
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return m
}

func TestJumpdriveDefaults(t *testing.T) {
	c, err := config.Load(env(jumpdriveEnv(nil)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.JumpdriveVerify != config.VerifyJWKS {
		t.Errorf("JumpdriveVerify = %q, want the jwks default", c.JumpdriveVerify)
	}
	// The issuer is the control plane's base URL: the index route minus
	// /internal/index.
	if c.JumpdriveIssuer != "https://console.jumpdrive.test" {
		t.Errorf("JumpdriveIssuer = %q, want it derived from JDX_JUMPDRIVE_URL", c.JumpdriveIssuer)
	}
	if c.JumpdriveSecret.Reveal() != "svc-secret" || strings.Contains(c.JumpdriveSecret.String(), "svc-secret") {
		t.Error("JumpdriveSecret not loaded, or leaked through String()")
	}
}

func TestJumpdriveVerifyModes(t *testing.T) {
	for _, mode := range []string{"jwks", "authorize", "dual", "DUAL"} {
		c, err := config.Load(env(jumpdriveEnv(map[string]string{"JDX_JUMPDRIVE_VERIFY": mode})))
		if err != nil {
			t.Errorf("JDX_JUMPDRIVE_VERIFY=%s: %v", mode, err)
			continue
		}
		if string(c.JumpdriveVerify) != strings.ToLower(mode) {
			t.Errorf("JDX_JUMPDRIVE_VERIFY=%s loaded as %q", mode, c.JumpdriveVerify)
		}
	}
	_, err := config.Load(env(jumpdriveEnv(map[string]string{"JDX_JUMPDRIVE_VERIFY": "both"})))
	if err == nil || !strings.Contains(err.Error(), "JDX_JUMPDRIVE_VERIFY") {
		t.Errorf("unknown verify mode: err=%v, want a JDX_JUMPDRIVE_VERIFY error", err)
	}
}

func TestJumpdriveRequiresSecretAndURL(t *testing.T) {
	_, err := config.Load(env(jumpdriveEnv(map[string]string{"JDX_JUMPDRIVE_SECRET": ""})))
	if err == nil || !strings.Contains(err.Error(), "JDX_JUMPDRIVE_SECRET") {
		t.Errorf("jumpdrive without a secret: err=%v, want a JDX_JUMPDRIVE_SECRET error", err)
	}
	// Required in every verify mode, not just authorize/dual.
	_, err = config.Load(env(jumpdriveEnv(map[string]string{"JDX_JUMPDRIVE_SECRET": "", "JDX_JUMPDRIVE_VERIFY": "jwks"})))
	if err == nil {
		t.Error("jwks mode without a secret was accepted")
	}
	_, err = config.Load(env(jumpdriveEnv(map[string]string{"JDX_JUMPDRIVE_URL": ""})))
	if err == nil || !strings.Contains(err.Error(), "JDX_JUMPDRIVE_URL") {
		t.Errorf("jumpdrive without a URL: err=%v, want a JDX_JUMPDRIVE_URL error", err)
	}
	_, err = config.Load(env(jumpdriveEnv(map[string]string{"JDX_JUMPDRIVE_URL": "console.jumpdrive.test/internal/index"})))
	if err == nil || !strings.Contains(err.Error(), "JDX_JUMPDRIVE_URL") {
		t.Errorf("schemeless URL: err=%v, want a JDX_JUMPDRIVE_URL error", err)
	}
	// Migrate never authenticates anyone, so it needs neither.
	if _, err := config.Load(env(jumpdriveEnv(map[string]string{
		"JDX_MODE": "migrate", "JDX_JUMPDRIVE_SECRET": "", "JDX_JUMPDRIVE_URL": "",
	}))); err != nil {
		t.Errorf("migrate with jumpdrive identity: %v", err)
	}
}

func TestJumpdriveIssuer(t *testing.T) {
	// Explicit wins over derived and is kept verbatim: iss is compared exactly,
	// so a trailing slash the control plane issues must survive.
	c, err := config.Load(env(jumpdriveEnv(map[string]string{"JDX_JUMPDRIVE_ISSUER": "https://jumpdrive.app/"})))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.JumpdriveIssuer != "https://jumpdrive.app/" {
		t.Errorf("JumpdriveIssuer = %q", c.JumpdriveIssuer)
	}
	// A URL that is not the standard route cannot imply an issuer.
	odd := map[string]string{"JDX_JUMPDRIVE_URL": "https://proxy.test/index"}
	for _, mode := range []string{"jwks", "dual"} {
		odd["JDX_JUMPDRIVE_VERIFY"] = mode
		_, err := config.Load(env(jumpdriveEnv(odd)))
		if err == nil || !strings.Contains(err.Error(), "JDX_JUMPDRIVE_ISSUER") {
			t.Errorf("%s with no derivable issuer: err=%v, want a JDX_JUMPDRIVE_ISSUER error", mode, err)
		}
	}
	// authorize never checks iss, so it doesn't need one.
	odd["JDX_JUMPDRIVE_VERIFY"] = "authorize"
	if _, err := config.Load(env(jumpdriveEnv(odd))); err != nil {
		t.Errorf("authorize with no issuer: %v", err)
	}
}
