package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	config "github.com/trebi-ai/trebi/internal/config"
)

func TestResolveInherit_DefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	def := ResourceDef{Key: "skills", DefaultInherit: true}
	if !ResolveInherit("claude", "skills", nil, def) {
		t.Fatal("default inherit")
	}
	falseV := false
	cfg := &config.Config{
		Tools: config.ToolsConfig{
			Bridge: &config.BridgeConfig{Inherit: &falseV},
		},
	}
	if ResolveInherit("claude", "skills", cfg, def) {
		t.Fatal("global inherit false")
	}
	cfg = &config.Config{
		Tools: config.ToolsConfig{
			Bridge: &config.BridgeConfig{Never: []string{"user_memory"}},
			Claude: &config.ToolPin{
				Bridge: &config.BridgeConfig{
					Resources: config.ResourceModeMap{"skills": "inherit", "user_memory": "false"},
				},
			},
		},
	}
	if !ResolveInherit("claude", "skills", cfg, def) {
		t.Fatal("skills inherit")
	}
	mem := ResourceDef{Key: "user_memory", DefaultInherit: true}
	if ResolveInherit("claude", "user_memory", cfg, mem) {
		t.Fatal("user_memory false")
	}
	// never wins even if tool inherit true
	trueV := true
	cfg = &config.Config{
		Tools: config.ToolsConfig{
			Bridge: &config.BridgeConfig{Never: []string{"mcp"}},
			Claude: &config.ToolPin{Bridge: &config.BridgeConfig{Inherit: &trueV}},
		},
	}
	mcp := ResourceDef{Key: "mcp", DefaultInherit: true}
	if ResolveInherit("claude", "mcp", cfg, mcp) {
		t.Fatal("never should force false")
	}
	// only allowlist
	cfg = &config.Config{
		Tools: config.ToolsConfig{
			Bridge: &config.BridgeConfig{Only: []string{"skills"}},
		},
	}
	if !ResolveInherit("claude", "skills", cfg, def) {
		t.Fatal("only skills")
	}
	if ResolveInherit("claude", "mcp", cfg, mcp) {
		t.Fatal("mcp not in only")
	}
	// keybindings default false
	kb := ResourceDef{Key: "keybindings", DefaultInherit: false}
	if ResolveInherit("claude", "keybindings", &config.Config{}, kb) {
		t.Fatal("keybindings default off")
	}
}

func TestEnsureBridges_SymlinkAndOptOut(t *testing.T) {
	t.Parallel()
	user := t.TempDir()
	harnessBase := t.TempDir()
	skills := filepath.Join(user, ".claude", "skills", "foo")
	if err := os.MkdirAll(skills, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "SKILL.md"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	mem := filepath.Join(user, ".claude", "CLAUDE.md")
	if err := os.WriteFile(mem, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Tools: config.ToolsConfig{
			Claude: &config.ToolPin{
				Bridge: &config.BridgeConfig{
					Resources: config.ResourceModeMap{"user_memory": "false"},
				},
			},
		},
	}
	roots := Roots{UserHome: user, HarnessBase: harnessBase}
	if err := EnsureBridgesRoots("claude", cfg, roots); err != nil {
		t.Fatal(err)
	}
	hSkills := filepath.Join(harnessBase, "claude", "skills")
	fi, err := os.Lstat(hSkills)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("skills should be symlink")
	}
	// user_memory off
	if _, err := os.Lstat(filepath.Join(harnessBase, "claude", "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatalf("CLAUDE.md should be absent, err=%v", err)
	}
	// idempotent
	if err := EnsureBridgesRoots("claude", cfg, roots); err != nil {
		t.Fatal(err)
	}
	// ensure removed leftover symlink when opt-out
	if err := os.Symlink(mem, filepath.Join(harnessBase, "claude", "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureBridgesRoots("claude", cfg, roots); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(harnessBase, "claude", "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatal("expected CLAUDE.md symlink removed")
	}
	// no writes under user home beyond what we created for the test
	// (skills dir still only our file)
}

func TestEnsureBridges_BlockedRealDir(t *testing.T) {
	t.Parallel()
	user := t.TempDir()
	harnessBase := t.TempDir()
	if err := os.MkdirAll(filepath.Join(user, ".claude", "skills"), 0755); err != nil {
		t.Fatal(err)
	}
	// real dir at dest
	dst := filepath.Join(harnessBase, "claude", "skills")
	if err := os.MkdirAll(dst, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dst, "keep-me")
	if err := os.WriteFile(marker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	roots := Roots{UserHome: user, HarnessBase: harnessBase}
	err := EnsureBridgesRoots("claude", &config.Config{}, roots)
	if err == nil {
		t.Fatal("want error when blocked without force")
	}
	if !strings.Contains(err.Error(), "skills") || !errors.Is(err, ErrBridgeBlocked) {
		// errors.Join may wrap; check message
		if !strings.Contains(err.Error(), "not a symlink") {
			t.Fatalf("want blocked error, got %v", err)
		}
	}
	// marker still there without force
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("must not clobber real dir without --force")
	}
	rows := BridgeStatusRoots("claude", &config.Config{}, roots)
	var skillsRow *BridgeStatusRow
	for i := range rows {
		if rows[i].Resource == "skills" {
			skillsRow = &rows[i]
			break
		}
	}
	if skillsRow == nil || skillsRow.Status != "blocked" {
		t.Fatalf("want blocked, got %+v", skillsRow)
	}

	// --force replaces real dir with symlink
	if err := EnsureBridgesOpts("claude", &config.Config{}, roots, EnsureOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("force should remove real dir contents")
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("force should create symlink")
	}
	target, _ := os.Readlink(dst)
	want := filepath.Join(user, ".claude", "skills")
	if target != want {
		t.Fatalf("symlink %q want %q", target, want)
	}
}

func TestEnsureBridges_NoUserHomeWrites(t *testing.T) {
	t.Parallel()
	user := t.TempDir()
	harnessBase := t.TempDir()
	// empty user tree
	before, _ := dirSnapshot(user)
	roots := Roots{UserHome: user, HarnessBase: harnessBase}
	_ = EnsureBridgesRoots("claude", &config.Config{}, roots)
	after, _ := dirSnapshot(user)
	if before != after {
		t.Fatalf("user home changed:\nbefore %s\nafter %s", before, after)
	}
}

func TestMCPMergeAndSettingsPrefs(t *testing.T) {
	t.Parallel()
	user := t.TempDir()
	harnessBase := t.TempDir()
	if err := os.MkdirAll(filepath.Join(user, ".claude"), 0755); err != nil {
		t.Fatal(err)
	}
	// user mcp.json
	if err := os.WriteFile(filepath.Join(user, ".claude", "mcp.json"), []byte(`{"mcpServers":{"a":{}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	// user .claude.json mcpServers
	if err := os.WriteFile(filepath.Join(user, ".claude.json"), []byte(`{"mcpServers":{"b":{"url":"x"}},"theme":"dark"}`), 0644); err != nil {
		t.Fatal(err)
	}
	// user settings with hooks
	userSettings := map[string]any{
		"model":          "opus",
		"hooks":          map[string]any{"Notification": []any{}},
		"enabledPlugins": map[string]any{"p": true},
		"statusLine":     "noisy",
	}
	sb, _ := json.Marshal(userSettings)
	if err := os.WriteFile(filepath.Join(user, ".claude", "settings.json"), sb, 0644); err != nil {
		t.Fatal(err)
	}
	// trebi-settings must survive
	hClaude := filepath.Join(harnessBase, "claude")
	if err := os.MkdirAll(hClaude, 0755); err != nil {
		t.Fatal(err)
	}
	trebiPath := filepath.Join(hClaude, "trebi-settings.json")
	if err := os.WriteFile(trebiPath, []byte(`{"hooks":{"Stop":[]}}`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{} // defaults inherit
	roots := Roots{UserHome: user, HarnessBase: harnessBase}
	if err := EnsureBridgesRoots("claude", cfg, roots); err != nil {
		t.Fatal(err)
	}
	// mcp.json symlink
	fi, err := os.Lstat(filepath.Join(hClaude, "mcp.json"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("mcp.json symlink: %v %v", fi, err)
	}
	// mcpServers merge
	hb, _ := os.ReadFile(filepath.Join(hClaude, ".claude.json"))
	var hm map[string]any
	_ = json.Unmarshal(hb, &hm)
	servers, _ := hm["mcpServers"].(map[string]any)
	if servers["b"] == nil {
		t.Fatalf("want mcpServers.b: %v", hm)
	}
	// settings prefs
	sb2, _ := os.ReadFile(filepath.Join(hClaude, "settings.json"))
	var hs map[string]any
	_ = json.Unmarshal(sb2, &hs)
	if _, ok := hs["hooks"]; ok {
		t.Fatal("hooks must be stripped")
	}
	if hs["model"] != "opus" {
		t.Fatalf("model %v", hs["model"])
	}
	// status_line default false → no statusLine
	if _, ok := hs["statusLine"]; ok {
		t.Fatal("statusLine should not copy by default")
	}
	// trebi-settings untouched
	jb, _ := os.ReadFile(trebiPath)
	if !strings.Contains(string(jb), "Stop") {
		t.Fatalf("trebi-settings clobbered: %s", jb)
	}
}

func TestGrokConfigPrefs_PreservesHooks(t *testing.T) {
	t.Parallel()
	user := t.TempDir()
	harnessBase := t.TempDir()
	if err := os.MkdirAll(filepath.Join(user, ".grok"), 0755); err != nil {
		t.Fatal(err)
	}
	userTOML := "permission_mode = \"always_allow\"\n"
	if err := os.WriteFile(filepath.Join(user, ".grok", "config.toml"), []byte(userTOML), 0644); err != nil {
		t.Fatal(err)
	}
	hGrok := filepath.Join(harnessBase, "grok")
	if err := os.MkdirAll(hGrok, 0755); err != nil {
		t.Fatal(err)
	}
	seed := "[compat.claude]\nhooks = false\n\n[harness]\ndisable_codebase_upload = true\n"
	if err := os.WriteFile(filepath.Join(hGrok, "config.toml"), []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}
	roots := Roots{UserHome: user, HarnessBase: harnessBase}
	if err := EnsureBridgesRoots("grok", &config.Config{}, roots); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(hGrok, "config.toml"))
	s := string(b)
	if !strings.Contains(s, "hooks = false") {
		t.Fatalf("hooks flag lost: %s", s)
	}
	if !strings.Contains(s, "disable_codebase_upload") {
		t.Fatalf("upload flag lost: %s", s)
	}
	if !strings.Contains(s, "permission_mode") {
		t.Fatalf("permission_mode not merged: %s", s)
	}
}

func dirSnapshot(root string) (string, error) {
	var b strings.Builder
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		b.WriteString(rel)
		b.WriteByte('\n')
		return nil
	})
	return b.String(), nil
}
