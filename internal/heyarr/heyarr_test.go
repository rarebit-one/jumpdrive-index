package heyarr_test

import (
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/heyarr"
	"github.com/rarebit-one/jumpdrive-index/internal/secret"
)

func TestNewRejectsEmptyBaseURL(t *testing.T) {
	if _, err := heyarr.New(heyarr.Options{BaseURL: "  "}); err == nil {
		t.Error("empty BaseURL should be rejected")
	}
}

func TestTokenRedaction(t *testing.T) {
	// A secret must never render its bytes in a formatted string.
	s := secret.Value("super-secret-token")
	if got := s.String(); got == "super-secret-token" {
		t.Error("secret leaked through String()")
	}
	if s.Reveal() != "super-secret-token" {
		t.Error("Reveal must return the real value")
	}
}
