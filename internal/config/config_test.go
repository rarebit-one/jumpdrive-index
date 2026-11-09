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

func TestEmbedProviderDefaults(t *testing.T) {
	// No embed config → provider is "".
	c, err := config.Load(env(map[string]string{}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.EmbedProvider != "" {
		t.Errorf("EmbedProvider should default to empty, got %q", c.EmbedProvider)
	}
}

func TestEmbedProviderUnknownIsRejected(t *testing.T) {
	_, err := config.Load(env(map[string]string{"JDX_EMBED_PROVIDER": "unknown-llm"}))
	if err == nil {
		t.Fatal("unknown JDX_EMBED_PROVIDER should be rejected")
	}
	if !strings.Contains(err.Error(), "JDX_EMBED_PROVIDER") {
		t.Errorf("error should name JDX_EMBED_PROVIDER, got %v", err)
	}
}

func TestEmbedFields(t *testing.T) {
	c, err := config.Load(env(map[string]string{
		"JDX_EMBED_PROVIDER": "fabric",
		"JDX_EMBED_URL":      "https://fabric.example/v1",
		"JDX_EMBED_MODEL":    "text-embedding-3-small",
		"JDX_EMBED_TOKEN":    "tok123",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.EmbedProvider != "fabric" {
		t.Errorf("EmbedProvider = %q", c.EmbedProvider)
	}
	if c.EmbedURL != "https://fabric.example/v1" {
		t.Errorf("EmbedURL = %q", c.EmbedURL)
	}
	if c.EmbedModel != "text-embedding-3-small" {
		t.Errorf("EmbedModel = %q", c.EmbedModel)
	}
	if c.EmbedToken.Reveal() != "tok123" {
		t.Error("EmbedToken not loaded")
	}
	// Token must not leak through default formatting.
	if strings.Contains(c.EmbedToken.String(), "tok123") {
		t.Error("EmbedToken leaked through String()")
	}
}

func TestIngestTranscribeDefaults(t *testing.T) {
	c, err := config.Load(env(map[string]string{}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.IngestTranscribe != "" {
		t.Errorf("IngestTranscribe should default to empty, got %q", c.IngestTranscribe)
	}
}

func TestIngestTranscribeUnknownIsRejected(t *testing.T) {
	_, err := config.Load(env(map[string]string{"JDX_INGEST_TRANSCRIBE": "openai"}))
	if err == nil {
		t.Fatal("unknown JDX_INGEST_TRANSCRIBE should be rejected")
	}
	if !strings.Contains(err.Error(), "JDX_INGEST_TRANSCRIBE") {
		t.Errorf("error should name JDX_INGEST_TRANSCRIBE, got %v", err)
	}
}

func TestIngestTranscribeFields(t *testing.T) {
	c, err := config.Load(env(map[string]string{
		"JDX_INGEST_TRANSCRIBE":       "fabric",
		"JDX_INGEST_TRANSCRIBE_URL":   "https://fabric.example/v1",
		"JDX_INGEST_TRANSCRIBE_MODEL": "whisper-1",
		"JDX_INGEST_TRANSCRIBE_TOKEN": "ingesttok",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.IngestTranscribe != "fabric" {
		t.Errorf("IngestTranscribe = %q", c.IngestTranscribe)
	}
	if c.IngestTranscribeURL != "https://fabric.example/v1" {
		t.Errorf("IngestTranscribeURL = %q", c.IngestTranscribeURL)
	}
	if c.IngestTranscribeModel != "whisper-1" {
		t.Errorf("IngestTranscribeModel = %q", c.IngestTranscribeModel)
	}
	if c.IngestTranscribeToken.Reveal() != "ingesttok" {
		t.Error("IngestTranscribeToken not loaded")
	}
}

func TestFabricDeviceDir(t *testing.T) {
	c, err := config.Load(env(map[string]string{
		"JDX_FABRIC_DEVICE_DIR": "/var/lib/starchart/voidbind-device",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.FabricDeviceDir != "/var/lib/starchart/voidbind-device" {
		t.Errorf("FabricDeviceDir = %q", c.FabricDeviceDir)
	}
}
