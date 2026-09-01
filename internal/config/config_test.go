package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
)

func TestLoad_MissingFile_Defaults(t *testing.T) {
	withTempBase(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.Command != "claude" {
		t.Fatalf("command = %q", cfg.Defaults.Command)
	}
	if cfg.Tools.Fallback != "path" {
		t.Fatalf("fallback = %q", cfg.Tools.Fallback)
	}
}

func TestLoad_ValidConfig(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
defaults:
  command: grok
  session:
    timeout: 30m
    nudge: 5m
    if_running: skip
terminal: Terminal
tools:
  fallback: path
  bridge:
    inherit: true
    never: [user_memory]
  claude:
    auth: shared
    bridge:
      resources:
        user_memory: false
        skills: inherit
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.Command != "grok" {
		t.Fatalf("command = %q", cfg.Defaults.Command)
	}
	if cfg.Defaults.Session.Timeout != "30m" {
		t.Fatalf("timeout = %q", cfg.Defaults.Session.Timeout)
	}
	if cfg.Tools.Claude == nil || cfg.Tools.Claude.Bridge == nil {
		t.Fatal("expected claude bridge")
	}
	if cfg.Tools.Claude.Bridge.Resources["user_memory"] != "false" {
		t.Fatalf("user_memory = %q", cfg.Tools.Claude.Bridge.Resources["user_memory"])
	}
}

func TestLoad_UnknownKey(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
defauts:
  command: claude
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "config.yaml") {
		t.Fatalf("want config.yaml in error, got %v", err)
	}
	if !strings.Contains(strings.ToLower(msg), "defauts") && !strings.Contains(strings.ToLower(msg), "field") {
		t.Fatalf("want unknown field hint, got %v", err)
	}
}

func TestLoad_BadEnum(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
defaults:
  session:
    if_running: retry
tools:
  fallback: maybe
  claude:
    auth: shareed
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "config.yaml") {
		t.Fatalf("path missing: %v", err)
	}
	if !strings.Contains(msg, "if_running") {
		t.Fatalf("want if_running: %v", err)
	}
	// multi-error: fallback and auth too
	if !strings.Contains(msg, "fallback") {
		t.Fatalf("want fallback: %v", err)
	}
	if !strings.Contains(msg, "auth") {
		t.Fatalf("want auth: %v", err)
	}
}

func TestLoad_BadDuration(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
defaults:
  session:
    timeout: soon
    nudge: 5
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "timeout") {
		t.Fatalf("want timeout: %v", err)
	}
	if !strings.Contains(msg, "nudge") {
		t.Fatalf("want nudge: %v", err)
	}
}

func TestLoad_BadBridgeMode(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
tools:
  claude:
    bridge:
      resources:
        skills: maybe
        foo: inherit
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "skills") {
		t.Fatalf("want skills mode error: %v", err)
	}
	if !strings.Contains(msg, "foo") {
		t.Fatalf("want unknown resource foo: %v", err)
	}
}

func TestLoad_YAMLSyntax(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
defaults:
  command: "unclosed
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "config.yaml") {
		t.Fatalf("got %v", err)
	}
}

func TestSave_RejectsInvalid(t *testing.T) {
	withTempBase(t)
	cfg := defaultConfig()
	cfg.Defaults.Session.IfRunning = "retry"
	if err := cfg.Save(); err == nil {
		t.Fatal("expected save to reject invalid")
	}
}

func withTempBase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	return dir
}
