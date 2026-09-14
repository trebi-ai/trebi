package harness

import (
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/config"
)

func TestDiscoverSkills_EmptyHome(t *testing.T) {
	if got := DiscoverSkills("", nil); got != nil {
		t.Fatalf("empty home: %v", got)
	}
}

func TestDiscoverSkills_Union(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude", "skills", "sling", "SKILL.md"), "# Sling\n")
	write(t, filepath.Join(home, ".codex", "skills", "sling", "SKILL.md"), "# Sling\n")
	write(t, filepath.Join(home, ".cursor", "skills-cursor", "browser", "SKILL.md"), "# Browser\n")
	write(t, filepath.Join(home, ".claude", "skills", "ignore-me", "README.md"), "nope\n")
	write(t, filepath.Join(home, ".pi", "agent", "skills", "sling", "SKILL.md"), "# Sling\n")

	got := DiscoverSkills(home, &config.Config{})
	byName := map[string]DiscoveredSkill{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if len(got) != 2 {
		t.Fatalf("want 2 skills, got %+v", got)
	}
	sling := byName["sling"]
	if sling.Name == "" || len(sling.Origins) != 3 {
		t.Fatalf("sling origins %d %+v", len(sling.Origins), sling)
	}
	if !sling.Injected {
		t.Fatal("sling should be injected by default inherit")
	}
	if len(sling.Harnesses) != 3 {
		t.Fatalf("harnesses %v", sling.Harnesses)
	}
	if byName["browser"].Name == "" || byName["ignore-me"].Name != "" {
		t.Fatalf("browser/ignore: %+v", byName)
	}
}

func TestDiscoverSkills_InheritOff(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".cursor", "skills-cursor", "x", "SKILL.md"), "# X\n")
	cfg := &config.Config{
		Tools: config.ToolsConfig{
			Bridge: &config.BridgeConfig{Never: []string{"skills"}},
		},
	}
	got := DiscoverSkills(home, cfg)
	if len(got) != 1 || got[0].Injected {
		t.Fatalf("want declared not injected: %+v", got)
	}
}
