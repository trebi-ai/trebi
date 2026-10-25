package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
    max_bytes: 8192
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Defaults.Memory
	if m.MaxLogs != 0 || m.RequireUpdate || m.InjectLogs != 20 || m.MaxBytes != 8192 {
		t.Fatalf("partial override lost defaults: %+v", m)
	}

	// max_bytes 0 turns the cap off; a negative value is invalid.
	if err := cfg.SetPath("defaults.memory.max_bytes", "0"); err != nil || cfg.Defaults.Memory.MaxBytes != 0 {
		t.Fatalf("set 0: %v %+v", err, cfg.Defaults.Memory)
	}
	if err := cfg.SetPath("defaults.memory.max_bytes", "-1"); err == nil {
		t.Fatal("set -1 accepted")
	}
	if err := cfg.UnsetPath("defaults.memory.max_bytes"); err != nil || cfg.Defaults.Memory.MaxBytes != DefaultMemoryMaxBytes {
		t.Fatalf("unset: %v %+v", err, cfg.Defaults.Memory)
	}
	cfg.Defaults.Memory.MaxBytes = -5
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "defaults.memory.max_bytes") {
		t.Fatalf("validate negative: %v", err)
	}
	cfg.Defaults.Memory.MaxBytes = 0

	// State limits: 0 takes the default; a negative value is invalid.
	if got := (StateDefaults{}).Resolved(); got.MaxRows != DefaultStateMaxRows || got.MaxBytes != DefaultStateMaxBytes {
		t.Fatalf("state defaults: %+v", got)
	}
	if err := cfg.SetPath("defaults.state.max_rows", "-1"); err == nil {
		t.Fatal("state max_rows -1 accepted")
	}
	cfg.Defaults.State.MaxBytes = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "defaults.state.max_bytes") {
		t.Fatalf("validate negative state: %v", err)
	}

	// Absent block keeps the seeded defaults.
	withTempBase(t)
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg.Defaults.Memory; m.MaxLogs != 100 || !m.RequireUpdate || m.InjectLogs != 20 || m.MaxBytes != 16384 {
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

func TestSessionMaxDurationMS(t *testing.T) {
	for in, want := range map[string]int64{
		"":    (24 * time.Hour).Milliseconds(),
		"2h":  (2 * time.Hour).Milliseconds(),
		"off": 0,
		"OFF": 0,
		"0":   0,
		"bad": (24 * time.Hour).Milliseconds(),
	} {
		if got := (SessionDefaults{MaxDuration: in}).MaxDurationMS(); got != want {
			t.Errorf("%q: got %d want %d", in, got, want)
		}
	}
}

// TestOMPHarnessConfig pins the omp surface in config.yaml: effort levels,
// model support, the first-class filter, and its own tool pin.
func TestOMPHarnessConfig(t *testing.T) {
	if got := strings.Join(HarnessEffortLevels("omp"), ","); got != "off,minimal,low,medium,high,xhigh,max" {
		t.Fatalf("omp effort levels = %q", got)
	}
	if got := strings.Join(HarnessEffortLevels("grok"), ","); got != "low,medium,high,xhigh" {
		t.Fatalf("grok effort levels = %q", got)
	}
	if !HarnessSupportsModel("omp") {
		t.Fatal("omp must take a model")
	}
	found := false
	for _, name := range FirstClassTools() {
		if name == "omp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("FirstClassTools = %v", FirstClassTools())
	}
	cfg := &Config{}
	enabled := true
	cfg.Tools.SetPin("omp", &ToolPin{Enabled: &enabled, Model: "opencode-go/deepseek-v4.1-flash"})
	if pin := cfg.Tools.Pin("omp"); pin == nil || !cfg.HarnessEnabled("omp") || pin.Model != "opencode-go/deepseek-v4.1-flash" {
		t.Fatalf("omp pin = %+v", pin)
	}
}
