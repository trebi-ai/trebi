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

func TestDiscoverSkills_AgentsRoot(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".agents", "skills", "only-agents", "SKILL.md"), "# Only\n")

	got := DiscoverSkills(home, &config.Config{})
	if len(got) != 1 || got[0].Name != "only-agents" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Path != filepath.Join(home, ".agents", "skills", "only-agents") {
		t.Fatalf("path %s", got[0].Path)
	}
	if len(got[0].Origins) != 1 || got[0].Origins[0].Tool != "agents" {
		t.Fatalf("origins %+v", got[0].Origins)
	}
	if len(got[0].Harnesses) != 0 || got[0].Injected {
		t.Fatalf("harnesses/injected %+v", got[0])
	}
}

func TestDiscoverSkills_PointerFollows(t *testing.T) {
	home := t.TempDir()
	realDir := filepath.Join(home, ".agents", "skills", "sling")
	write(t, filepath.Join(realDir, "SKILL.md"), "# Real sling\n\nBody.\n")
	write(t, filepath.Join(realDir, "NOTES.md"), "notes\n")
	ptr := filepath.Join(home, ".claude", "skills", "sling", "SKILL.md")
	write(t, ptr, "@"+filepath.Join(realDir, "SKILL.md")+"\n")

	got := DiscoverSkills(home, &config.Config{})
	if len(got) != 1 || got[0].Name != "sling" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Path != realDir {
		t.Fatalf("path want %s got %s", realDir, got[0].Path)
	}
	if len(got[0].Origins) != 2 {
		t.Fatalf("origins %+v", got[0].Origins)
	}
	md := ReadSkillMD(got[0].Path, home)
	if string(md) != "# Real sling\n\nBody.\n" {
		t.Fatalf("md %q", md)
	}
	// Following the pointer dir itself also yields the real document.
	md = ReadSkillMD(filepath.Dir(ptr), home)
	if string(md) != "# Real sling\n\nBody.\n" {
		t.Fatalf("follow %q", md)
	}
}

func TestReadSkillMD_RefusesEscape(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret.md"), "nope\n")
	dir := filepath.Join(home, ".claude", "skills", "x")
	ptr := "@" + filepath.Join(outside, "secret.md") + "\n"
	write(t, filepath.Join(dir, "SKILL.md"), ptr)
	md := ReadSkillMD(dir, home)
	if string(md) != ptr {
		t.Fatalf("want unfollowed pointer, got %q", md)
	}
	if ResolveSkillDir(dir, home) != dir {
		t.Fatalf("resolve escaped")
	}
}
