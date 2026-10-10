package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
)

// filesEnv gives a service with a fake user home and trebi home.
func filesEnv(t *testing.T) (*Service, Roots) {
	t.Helper()
	base := t.TempDir()
	home.SetBase(base)
	t.Setenv("TREBI_HOME", base)
	t.Cleanup(func() { home.SetBase("") })
	roots := Roots{UserHome: t.TempDir(), HarnessBase: filepath.Join(base, "harness")}
	return New(openStore(t), nil, Options{Roots: roots}), roots
}

func writeUser(t *testing.T, roots Roots, rel, content string) string {
	t.Helper()
	p := filepath.Join(roots.UserHome, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestOwnFileDropsHooksAndSurvivesForce(t *testing.T) {
	svc, roots := filesEnv(t)
	ctx := context.Background()
	writeUser(t, roots, ".claude/settings.json", `{"model":"opus","hooks":{"Stop":[]}}`)
	if err := svc.OwnFile(ctx, "claude", "settings_prefs", "user"); err != nil {
		t.Fatal(err)
	}
	content, _, err := svc.ReadFile("claude", "settings_prefs", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "hooks") || !strings.Contains(content, "opus") {
		t.Fatalf("own copy: %s", content)
	}
	// The bridge never replaces an own file, also with force.
	if err := EnsureBridgesOpts("claude", loadCfg(t), roots, EnsureOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	after, _, _ := svc.ReadFile("claude", "settings_prefs", "trebi")
	if after != content {
		t.Fatalf("bridge changed the own file:\n%s\nwant\n%s", after, content)
	}
	rows, err := svc.Files("claude")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Mode != "own" || rows[0].OwnCreatedAt == nil || OwnCount("claude", loadCfg(t)) != 1 {
		t.Fatalf("row: %+v", rows[0])
	}
}

func TestWriteFileChecks(t *testing.T) {
	svc, roots := filesEnv(t)
	ctx := context.Background()
	if _, err := svc.WriteFile(ctx, "codex", "config_prefs", "x = 1\n", ""); !errors.Is(err, ErrNotOwn) {
		t.Fatalf("yours mode: %v", err)
	}
	writeUser(t, roots, ".codex/config.toml", "model = \"gpt-5.5\"\n")
	if err := svc.OwnFile(ctx, "codex", "config_prefs", "user"); err != nil {
		t.Fatal(err)
	}
	_, etag, _ := svc.ReadFile("codex", "config_prefs", "trebi")

	var stale *StaleError
	if _, err := svc.WriteFile(ctx, "codex", "config_prefs", "a = 1\n", "old"); !errors.As(err, &stale) || stale.ETag != etag {
		t.Fatalf("stale etag: %v", err)
	}
	var pe *ParseError
	if _, err := svc.WriteFile(ctx, "codex", "config_prefs", "a = 1\nb = = 2\n", etag); !errors.As(err, &pe) || pe.Line != 2 {
		t.Fatalf("toml error: %v", err)
	}
	next, err := svc.WriteFile(ctx, "codex", "config_prefs", "a = 1\n", etag)
	if err != nil || next == etag {
		t.Fatalf("write: %v %q", err, next)
	}
	if _, err := svc.WriteFile(ctx, "codex", "config_prefs", strings.Repeat("a", MaxConfigFileBytes+1), ""); !errors.Is(err, ErrFileTooBig) {
		t.Fatalf("size: %v", err)
	}

	writeUser(t, roots, ".claude/settings.json", "{}")
	if err := svc.OwnFile(ctx, "claude", "settings_prefs", "empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.WriteFile(ctx, "claude", "settings_prefs", "{\n  \"a\": 1,\n}\n", ""); !errors.As(err, &pe) || pe.Line != 3 {
		t.Fatalf("json error: %v", err)
	}
}

func TestResetFileBacksUpAndRestores(t *testing.T) {
	svc, roots := filesEnv(t)
	ctx := context.Background()
	user := writeUser(t, roots, ".grok/AGENTS.md", "user rules\n")
	if err := svc.OwnFile(ctx, "grok", "user_memory", "empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.WriteFile(ctx, "grok", "user_memory", "trebi rules\n", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResetFile(ctx, "grok", "user_memory"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(roots.HarnessBase, "grok", "AGENTS.md")
	if target, err := os.Readlink(dst); err != nil || target != user {
		t.Fatalf("reset must link the user file: %q %v", target, err)
	}
	rows, _ := svc.Files("grok")
	var row FileRow
	for _, r := range rows {
		if r.Key == "user_memory" {
			row = r
		}
	}
	if row.Mode != "yours" || row.BackupUntil == nil {
		t.Fatalf("row after reset: %+v", row)
	}
	if err := svc.OwnFile(ctx, "grok", "user_memory", "backup"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := svc.ReadFile("grok", "user_memory", ""); got != "trebi rules\n" {
		t.Fatalf("restore: %q", got)
	}
}

func TestUserChangedSinceOwn(t *testing.T) {
	svc, roots := filesEnv(t)
	ctx := context.Background()
	user := writeUser(t, roots, ".codex/AGENTS.md", "a\n")
	if err := svc.OwnFile(ctx, "codex", "user_memory", "user"); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(user, later, later); err != nil {
		t.Fatal(err)
	}
	rows, _ := svc.Files("codex")
	for _, r := range rows {
		if r.Key == "user_memory" && !r.UserChangedSinceOwn {
			t.Fatalf("user change not seen: %+v", r)
		}
	}
}

func TestGrokCompat(t *testing.T) {
	svc, roots := filesEnv(t)
	ctx := context.Background()
	if err := config.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := svc.Apply(ctx, "grok", Patch{Compat: map[string]map[string]bool{"claude": {"skills": false, "hooks": true}}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(roots.HarnessBase, "grok", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, want := range []string{"skills = false", "hooks = true", "agents = true", "disable_codebase_upload = true"} {
		if !strings.Contains(out, want) {
			t.Fatalf("config.toml lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "[compat.claude]") != 1 {
		t.Fatalf("one compat table:\n%s", out)
	}
	if err := svc.Apply(ctx, "codex", Patch{Compat: map[string]map[string]bool{"claude": {"skills": false}}}); err == nil {
		t.Fatal("codex has no compat switches")
	}
}

func TestSetTOMLKey(t *testing.T) {
	t.Parallel()
	in := "[compat.claude]\nhooks = false\n\n[harness]\nx = 1\n"
	got := setTOMLKey(in, "compat.claude", "hooks", "true")
	got = setTOMLKey(got, "compat.claude", "skills", "false")
	got = setTOMLKey(got, "other", "k", "1")
	want := "[compat.claude]\nskills = false\nhooks = true\n\n[harness]\nx = 1\n\n[other]\nk = 1\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
