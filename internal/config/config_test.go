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

func TestAPIConfig_ListenAddr(t *testing.T) {
	t.Parallel()
	port := func(n int) *int { return &n }
	if got := (APIConfig{}).ListenAddr(); got != "127.0.0.1:7433" {
		t.Fatalf("default: %q", got)
	}
	if got := (APIConfig{Host: "0.0.0.0"}).ListenAddr(); got != "0.0.0.0:7433" {
		t.Fatalf("host only: %q", got)
	}
	if got := (APIConfig{Host: "0.0.0.0", Port: port(0)}).ListenAddr(); got != "0.0.0.0:0" {
		t.Fatalf("explicit ephemeral: %q", got)
	}
	if got := (APIConfig{Host: "0.0.0.0", Port: port(7433)}).ListenAddr(); got != "0.0.0.0:7433" {
		t.Fatalf("host+port: %q", got)
	}
	if got := (APIConfig{Host: "::1", Port: port(80)}).ListenAddr(); got != "[::1]:80" {
		t.Fatalf("ipv6: %q", got)
	}
}

func TestLoad_APIHostPort(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
api:
  host: 0.0.0.0
  port: 7433
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.ListenAddr() != "0.0.0.0:7433" {
		t.Fatalf("listen = %q", cfg.API.ListenAddr())
	}
}

func TestAPIConfig_BindGuard(t *testing.T) {
	t.Parallel()
	if err := BindGuard("127.0.0.1:0", APIConfig{}); err != nil {
		t.Fatalf("loopback: %v", err)
	}
	if err := BindGuard("0.0.0.0:7433", APIConfig{}); err == nil {
		t.Fatal("want refuse non-loopback without tls/insecure")
	}
	if err := BindGuard("0.0.0.0:7433", APIConfig{TLS: APITLS{Insecure: true}}); err != nil {
		t.Fatalf("insecure: %v", err)
	}
	if err := BindGuard("0.0.0.0:443", APIConfig{TLS: APITLS{Cert: "/c", Key: "/k"}}); err != nil {
		t.Fatalf("tls: %v", err)
	}
	a := APIConfig{Host: "0.0.0.0", TLS: APITLS{Insecure: true}}
	if !a.PublicBind() {
		t.Fatal("0.0.0.0 should be public")
	}
	if (APIConfig{Host: "192.168.1.9"}).PublicBind() {
		t.Fatal("RFC1918 is not public")
	}
}

func TestLoad_BadAPIHostPort(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
api:
  host: "not a host!"
  port: 70000
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "api.host") {
		t.Fatalf("want api.host: %v", err)
	}
	if !strings.Contains(msg, "api.port") {
		t.Fatalf("want api.port: %v", err)
	}
}

func TestLoad_TLSPairRequired(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
api:
  tls:
    cert: /tmp/cert.pem
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "api.tls") {
		t.Fatalf("want api.tls pair: %v", err)
	}
}

func withTempBase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	return dir
}

func TestLoad_MemoryDefaults(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
defaults:
  memory:
    max_logs: 0
    require_update: false
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Defaults.Memory
	if m.MaxLogs != 0 || m.RequireUpdate || m.InjectLogs != 20 {
		t.Fatalf("partial override lost defaults: %+v", m)
	}

	// Absent block keeps the seeded defaults.
	withTempBase(t)
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg.Defaults.Memory; m.MaxLogs != 100 || !m.RequireUpdate || m.InjectLogs != 20 {
		t.Fatalf("seeded defaults: %+v", m)
	}

	// Unknown key under defaults.memory is a decode error.
	dir = withTempBase(t)
	yaml = `
defaults:
  memory:
    cap: 5
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("expected unknown-key error, got %v", err)
	}
}
