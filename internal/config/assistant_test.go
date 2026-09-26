package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/secrets"
)

func TestAssistantUnknownFormat(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.Assistant.Native.APIFormat = "groq"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "api_format") {
		t.Fatalf("want api_format error, got %v", err)
	}
}

func TestAssistantEnabledAndKeySource(t *testing.T) {
	dir := withTempBase(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_ = os.Unsetenv("OPENAI_API_KEY")

	cfg := defaultConfig()
	if cfg.Assistant.Native.Enabled() {
		t.Fatal("empty model should be disabled")
	}
	if err := cfg.SetPath("assistant.native.model", "claude-sonnet-4-5"); err != nil {
		t.Fatal(err)
	}
	if cfg.Assistant.Native.Enabled() {
		t.Fatal("model without key should be disabled")
	}
	if src := cfg.Assistant.Native.KeySourceName(); src != KeySourceNone {
		t.Fatalf("source=%s", src)
	}

	t.Setenv("DEMO_KEY", "from-env")
	if err := cfg.SetPath("assistant.native.api_key_env", "DEMO_KEY"); err != nil {
		t.Fatal(err)
	}
	key, src, err := cfg.Assistant.Native.ResolveKey()
	if err != nil || key != "from-env" || src != "env:DEMO_KEY" {
		t.Fatalf("env resolve: %q %q %v", key, src, err)
	}
	if !cfg.Assistant.Native.Enabled() {
		t.Fatal("want enabled via env")
	}

	if err := cfg.SetPath("assistant.api_key", "sk-secret"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	key, src, err = cfg.Assistant.Native.ResolveKey()
	if err != nil || key != "sk-secret" || src != KeySourceSecret {
		t.Fatalf("secret wins: %q %q %v", key, src, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if strings.Contains(string(raw), "sk-secret") {
		t.Fatal("Save leaked api_key")
	}
	if !strings.Contains(string(raw), "api_key_present: true") {
		t.Fatalf("want present flag: %s", raw)
	}

	if err := cfg.UnsetPath("assistant.api_key"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Get(AssistantKeySecret); err == nil {
		t.Fatal("secret should be gone")
	}
	key, src, err = cfg.Assistant.Native.ResolveKey()
	if err != nil || key != "from-env" || src != "env:DEMO_KEY" {
		t.Fatalf("fallback after unset: %q %q %v", key, src, err)
	}
}

func TestAssistantWellKnownEnv(t *testing.T) {
	withTempBase(t)
	t.Setenv("ANTHROPIC_API_KEY", "ant-well")
	cfg := defaultConfig()
	cfg.Assistant.Native.Model = "claude-sonnet-4-5"
	cfg.Assistant.Native.APIFormat = "anthropic"
	key, src, err := cfg.Assistant.Native.ResolveKey()
	if err != nil || key != "ant-well" || src != "well-known:ANTHROPIC_API_KEY" {
		t.Fatalf("%q %q %v", key, src, err)
	}
}

func TestAssistantYAMLKeyMigrates(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
assistant:
  native:
    model: claude-sonnet-4-5
    api_format: anthropic
  api_key: sk-from-yaml
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	key, src, err := cfg.Assistant.Native.ResolveKey()
	if err != nil || key != "sk-from-yaml" || src != KeySourceSecret {
		t.Fatalf("%q %q %v", key, src, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if strings.Contains(string(raw), "sk-from-yaml") {
		t.Fatal("migrated file still has key")
	}
}
