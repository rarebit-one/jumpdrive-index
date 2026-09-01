package fabricauth_test

import (
	"testing"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/fabricauth"
)

// TestEmptyDirDisablesVoidbind proves the opt-in branch: no device dir means
// Voidbind is not configured, so Client returns (nil, nil) and the caller keeps
// its bearer/default client. This is the default path the existing deploy takes.
func TestEmptyDirDisablesVoidbind(t *testing.T) {
	c, err := fabricauth.Client("", 30*time.Second)
	if err != nil {
		t.Fatalf("empty dir: unexpected error %v", err)
	}
	if c != nil {
		t.Errorf("empty dir: got a client, want nil (Voidbind off)")
	}
}

// TestProvisionedDirYieldsClient proves that a device dir yields a real client
// (the Device-credential transport). The credential wire behaviour itself is
// proven in voidbind-go/deviceclient against the actual rp verifier; here we only
// assert the wiring produces a usable, timeout-configured client.
func TestProvisionedDirYieldsClient(t *testing.T) {
	c, err := fabricauth.Client(t.TempDir(), 42*time.Second)
	if err != nil {
		t.Fatalf("provisioned dir: %v", err)
	}
	if c == nil {
		t.Fatal("provisioned dir: got nil client, want a Device-auth client")
	}
	if c.Timeout != 42*time.Second {
		t.Errorf("client timeout = %v, want 42s", c.Timeout)
	}
}
