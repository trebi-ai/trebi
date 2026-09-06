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
			t.Errorf("SetPath(%q, %q): %v", e.Key, sample, err)
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("after set all: %v", err)
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
	if err := cfg.SetPath("notify.email", "inbox-cli send …"); err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Email != "inbox-cli send …" {
		t.Fatalf("%q", cfg.Notify.Email)
	}
	if err := cfg.SetPath("notify.discord", "discord-cli dm …"); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notify.Channels()) != 2 {
		t.Fatalf("channels: %v", cfg.Notify.Channels())
	}
	if err := cfg.SetPath("notify.email", "-"); err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Email != "" {
		t.Fatalf("%q", cfg.Notify.Email)
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
	case key == "terminal":
		return "Terminal"
	case key == "tools.fallback":
		return "path"
	case key == "notify.email", key == "notify.whatsapp", key == "notify.discord":
		return "echo trebi-notify-{{message}}"
	case key == "notify.waiting_after":
		return "5m"
	case key == "notify.usage_warn_at":
		return "80"
	case key == "files.max_bytes":
		return "10485760"
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
	case strings.HasSuffix(key, ".version"):
		return "1.0.0"
	case strings.HasSuffix(key, ".channel"):
		return "latest"
	case strings.HasSuffix(key, ".path"):
		return "/tmp/trebi-tool"
	case strings.HasSuffix(key, ".bridge.inherit"):
		return "true"
	case strings.Contains(key, ".bridge.resources."):
		return "inherit"
	default:
		return "x"
	}
}
