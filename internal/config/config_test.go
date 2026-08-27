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
