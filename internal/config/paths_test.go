package config

import (
	"strings"
	"testing"
)

func TestPathKeyDescription_Coverage(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.applyDefaults()
	for _, e := range cfg.PathEntries(nil) {
		if e.Description == "" {
			t.Errorf("missing description for %s", e.Key)
		}
	}
	if PathKeyDescription("defaults.command") == "" {
		t.Fatal("defaults.command")
	}
	if !strings.Contains(PathKeyDescription("tools.claude.auth"), "shared") {
		t.Fatal(PathKeyDescription("tools.claude.auth"))
	}
	// Every bridge resource needs a written description, not the generic
	// "Bridge resource %q" fallback that PathKeyDescription returns for
	// keys missing from bridgeResourceDescriptions.
	for _, e := range cfg.PathEntries(nil) {
		if strings.HasPrefix(e.Description, "Bridge resource ") {
			t.Errorf("%s: undocumented bridge resource", e.Key)
		}
	}
}

func TestListPathEntries_SourceConfigVsDefault(t *testing.T) {
	t.Parallel()
	yaml := []byte(`
defaults:
  command: grok
  session:
    timeout: 30m
tools:
  claude:
    bridge:
      resources:
        user_memory: false
`)
	cfg := defaultConfig()
	if err := decodeConfig(yaml, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.applyDefaults()
	decl, err := DeclaredPaths(yaml)
	if err != nil {
		t.Fatal(err)
	}
	ents := cfg.PathEntries(decl)
	by := map[string]PathEntry{}
	for _, e := range ents {
		by[e.Key] = e
	}
	if by["defaults.command"].Value != "grok" || by["defaults.command"].Source != PathSourceFile {
		t.Fatalf("command: %+v", by["defaults.command"])
	}
	if by["defaults.session.timeout"].Value != "30m" || by["defaults.session.timeout"].Source != PathSourceFile {
		t.Fatalf("timeout: %+v", by["defaults.session.timeout"])
	}
	if by["terminal"].Source != PathSourceDefault || by["terminal"].Value != "Terminal" {
		t.Fatalf("terminal: %+v", by["terminal"])
	}
	um := by["tools.claude.bridge.resources.user_memory"]
	if um.Value != "off" || um.Source != PathSourceFile {
		t.Fatalf("user_memory: %+v", um)
	}
	sk := by["tools.claude.bridge.resources.skills"]
	if sk.Value != "inherit" || sk.Source != PathSourceDefault {
		t.Fatalf("skills: %+v", sk)
	}
}

// TestSetPath_AllShowKeys ensures every key from config show is settable.
func TestSetPath_AllShowKeys(t *testing.T) {
	cfg := defaultConfig()
	cfg.applyDefaults()
	ents := cfg.PathEntries(nil)
	for _, e := range ents {
		sample := sampleSetValue(e.Key)
		if err := cfg.SetPath(e.Key, sample); err != nil {
			if strings.Contains(err.Error(), "read-only") {
				continue
			}
			t.Errorf("SetPath(%q, %q): %v", e.Key, sample, err)
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("after set all: %v", err)
	}
}

func TestSetPath_ToolModelEffortEnv(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.applyDefaults()
	if err := cfg.SetPath("tools.claude.model", "sonnet"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetPath("tools.claude.effort", "xhigh"); err != nil {
		t.Fatal(err)
	}
	if cfg.Tools.Claude.Model != "sonnet" || cfg.Tools.Claude.Effort != "xhigh" {
		t.Fatalf("%+v", cfg.Tools.Claude)
	}
	if err := cfg.SetPath("tools.claude.effort", "nope"); err == nil {
		t.Fatal("invalid effort accepted")
	}
	if err := cfg.SetPath("tools.cursor.effort", "high"); err == nil {
		t.Fatal("cursor has no effort flag")
	}
	if err := cfg.SetPath("tools.opencode.effort", "fast"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetPath("tools.claude.env.LOG_LEVEL", "debug"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetPath("tools.claude.env.1BAD", "x"); err == nil {
		t.Fatal("invalid env key accepted")
	}
	if cfg.Tools.Claude.Env["LOG_LEVEL"] != "debug" {
		t.Fatalf("env: %v", cfg.Tools.Claude.Env)
	}
	if err := cfg.SetPath("tools.claude.env.LOG_LEVEL", ""); err != nil {
		t.Fatal(err)
	}
	if cfg.Tools.Claude.Env != nil {
		t.Fatalf("clearing the last key must drop the map: %v", cfg.Tools.Claude.Env)
	}
}

func TestSetPath_RoundTrip(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.applyDefaults()
	if err := cfg.SetPath("defaults.session.timeout", "45m"); err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.Session.Timeout != "45m" {
		t.Fatal(cfg.Defaults.Session.Timeout)
	}
	if err := cfg.SetPath("tools.claude.bridge.resources.user_memory", "false"); err != nil {
		t.Fatal(err)
	}
	if !IsInherit("false") && cfg.Tools.Claude.Bridge.Resources["user_memory"] != "false" {
		t.Fatalf("%v", cfg.Tools.Claude.Bridge.Resources)
	}
	if err := cfg.SetPath("defaults.session.if_running", "retry"); err == nil {
		t.Fatal("want bad if_running error")
	}
	if err := cfg.SetPath("no.such.path", "x"); err == nil {
		t.Fatal("want unknown key")
	}
	if err := cfg.SetPath("api.host", "0.0.0.0"); err != nil {
		t.Fatal(err)
	}
	if cfg.API.Host != "0.0.0.0" {
		t.Fatal(cfg.API.Host)
	}
	if err := cfg.SetPath("api.port", "7433"); err != nil {
		t.Fatal(err)
	}
	if cfg.API.ListenAddr() != "0.0.0.0:7433" {
		t.Fatal(cfg.API.ListenAddr())
	}
	if err := cfg.SetPath("api.host", "not a host!"); err == nil {
		t.Fatal("want bad api.host error")
	}
	if err := cfg.SetPath("api.tls.insecure", "true"); err != nil {
		t.Fatal(err)
	}
	if !cfg.API.TLS.Insecure {
		t.Fatal("tls.insecure")
	}
	if err := cfg.SetPath("api.tls.cert", "/tmp/cert.pem"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetPath("api.tls.key", "/tmp/key.pem"); err != nil {
		t.Fatal(err)
	}
	if !cfg.API.TLSEnabled() {
		t.Fatal("tls")
	}
	if err := cfg.SetPath("notify.route", `[{"via":"whatsapp","to":"+1555"},{"via":"email","to":"admins"}]`); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notify.Route) != 2 || cfg.Notify.Route[1].To != "admins" {
		t.Fatalf("%+v", cfg.Notify.Route)
	}
	if err := cfg.SetPath("notify.route", `["email"]`); err == nil {
		t.Fatal("old channel list must fail")
	}
	if err := cfg.SetPath("notify.route", "-"); err != nil || cfg.Notify.Route != nil {
		t.Fatalf("clear %v %+v", err, cfg.Notify.Route)
	}
	if err := cfg.SetPath("notify.approval_after", "soon"); err == nil {
		t.Fatal("bad approval_after must fail")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFilterPathEntries(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.applyDefaults()
	ents := cfg.PathEntries(nil)

	got, err := FilterPathEntries(ents, []string{"defaults.*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("expected defaults.* matches")
	}
	for _, e := range got {
		if !strings.HasPrefix(e.Key, "defaults.") {
			t.Fatalf("unexpected key %q", e.Key)
		}
	}

	// Bare section path = prefix: tools.grok → tools.grok.* (no trailing .* needed)
	section, err := FilterPathEntries(ents, []string{"tools.grok"})
	if err != nil {
		t.Fatal(err)
	}
	if len(section) == 0 {
		t.Fatal("expected tools.grok section matches")
	}
	for _, e := range section {
		if e.Key != "tools.grok" && !strings.HasPrefix(e.Key, "tools.grok.") {
			t.Fatalf("unexpected key under tools.grok: %q", e.Key)
		}
	}
	// Must not pull tools.grokmore or tools.claude
	for _, e := range section {
		if strings.HasPrefix(e.Key, "tools.claude") {
			t.Fatalf("section leaked: %q", e.Key)
		}
	}

	// defaults → all defaults.* children
	defs, err := FilterPathEntries(ents, []string{"defaults"})
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) == 0 {
		t.Fatal("expected defaults section")
	}
	for _, e := range defs {
		if e.Key != "defaults" && !strings.HasPrefix(e.Key, "defaults.") {
			t.Fatalf("unexpected under defaults: %q", e.Key)
		}
	}

	exact, err := FilterPathEntries(ents, []string{"defaults.command"})
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) != 1 || exact[0].Key != "defaults.command" {
		t.Fatalf("exact: %+v", exact)
	}

	multi, err := FilterPathEntries(ents, []string{"terminal", "tools.fallback"})
	if err != nil {
		t.Fatal(err)
	}
	if len(multi) != 2 {
		t.Fatalf("multi: %d", len(multi))
	}

	none, err := FilterPathEntries(ents, []string{"no.such.*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("want empty, got %d", len(none))
	}

	nonePref, err := FilterPathEntries(ents, []string{"no.such.section"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nonePref) != 0 {
		t.Fatalf("want empty prefix, got %d", len(nonePref))
	}

	if _, err := FilterPathEntries(ents, []string{"["}); err == nil {
		t.Fatal("want bad pattern error")
	}
}

func TestDeclaredPaths(t *testing.T) {
	t.Parallel()
	d, err := DeclaredPaths([]byte("defaults:\n  command: claude\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !d["defaults"] || !d["defaults.command"] {
		t.Fatalf("%v", d)
	}
	if d["terminal"] {
		t.Fatal("terminal not declared")
	}
}

func TestSortedPaths_NonEmpty(t *testing.T) {
	t.Parallel()
	p := SortedPaths()
	if len(p) < 20 {
		t.Fatalf("too few paths: %d", len(p))
	}
	if !strings.HasPrefix(p[0], "defaults.") && p[0] != "defaults.command" {
		// just ensure sorted
		for i := 1; i < len(p); i++ {
			if p[i] < p[i-1] {
				t.Fatal("unsorted")
			}
		}
	}
}

// sampleSetValue returns a valid value for config set of key (for coverage tests).
func sampleSetValue(key string) string {
	switch {
	case key == "defaults.command":
		return "claude"
	case key == "api.host":
		return "127.0.0.1"
	case key == "api.port":
		return "0"
	case key == "api.tls.cert", key == "api.tls.key":
		return "/tmp/trebi-tls.pem"
	case key == "api.tls.insecure":
		return "false"
	case key == "terminal":
		return "Terminal"
	case key == "tools.fallback":
		return "path"
	case key == "harness.runtime":
		return "cli"
	case key == "notify.route":
		return `[{"via":"email","to":"contact"}]`
	case key == "notify.approval_after":
		return "5m"
	case key == "requests.expire_after", key == "requests.retain":
		return "24h"
	case key == "ui.public_url":
		return "https://trebi.example.com"
	case key == "defaults.workspace":
		return "ops"
	case key == "email.worker":
		return "https://trebi-mail.example.workers.dev"
	case key == "connectors.registries":
		return "internal=https://mcp.internal.example"
	case key == "cloud.api_url":
		return "https://staging-api.trebi.ai"
	case key == "cloud.enabled", key == "cloud.remote_mcp":
		return "true"
	case key == "email.token":
		return "secret:trebi_mail"
	case key == "email.domain":
		return "mail.example.com"
	case key == "email.admins":
		return "ops@example.com"
	case key == "email.content_days":
		return "30"
	case key == "email.poll_interval":
		return "30s"
	case key == "email.require_dmarc":
		return "true"
	case key == "email.from_name":
		return "Trebi"
	case key == "notify.waiting_after":
		return "5m"
	case key == "notify.usage_warn_at":
		return "80"
	case key == "files.max_bytes":
		return "10485760"
	case key == "defaults.memory.max_bytes":
		return "16384"
	case key == "defaults.state.max_rows", key == "defaults.state.max_bytes":
		return "5000"
	case key == "defaults.session.if_running":
		return "skip"
	case key == "defaults.session.nudge":
		return "5m"
	case strings.HasPrefix(key, "defaults.session."):
		return "30m"
	case key == "tools.bridge.inherit":
		return "true"
	case key == "tools.bridge.only", key == "tools.bridge.never":
		return `["skills"]`
	case strings.HasSuffix(key, ".auth"):
		return "shared"
	case strings.HasSuffix(key, ".runtime"):
		return "cli"
	case strings.HasSuffix(key, ".version"):
		return "1.0.0"
	case strings.HasSuffix(key, ".channel"):
		return "latest"
	case strings.HasSuffix(key, ".model"):
		return "claude-opus-5"
	case strings.HasSuffix(key, ".effort"):
		return "high"
	case strings.Contains(key, ".env."):
		return "1"
	case strings.HasSuffix(key, ".path"):
		return "/tmp/trebi-tool"
	case strings.HasSuffix(key, ".bridge.inherit"):
		return "true"
	case strings.Contains(key, ".bridge.resources."):
		return "inherit"
	case key == "assistant.harness":
		return "native"
	case key == "assistant.native.api_format":
		return "openai"
	case key == "assistant.mode":
		return "auto_edit"
	case key == "assistant.effort":
		return "high"
	case key == "assistant.idle_close":
		return "15m"
	case key == "assistant.permission_timeout":
		return "10m"
	case key == "assistant.max_live", key == "assistant.native.max_tokens":
		return "8192"
	case key == "assistant.native.context_tokens", key == "assistant.native.limits.daily_tokens", key == "assistant.native.limits.max_turns":
		return "0"
	case key == "assistant.native.web_search":
		return "true"
	case key == "assistant.api_key":
		return "sk-test"
	default:
		return "x"
	}
}
