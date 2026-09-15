package tmux

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
)

func TestSyncClaudeOAuthAccountShared(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	if err := os.MkdirAll(filepath.Join(userHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	user := []byte(`{"oauthAccount":{"emailAddress":"a@example.com","organizationUuid":"org-1"},"theme":"dark"}`)
	if err := os.WriteFile(filepath.Join(userHome, ".claude.json"), user, 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	if err := EnsureSharedAuth(Claude, harness.AuthModeShared); err != nil {
		t.Fatal(err)
	}
	got := readHarnessOAuthEmail(t, config.HarnessDir(Claude))
	if got != "a@example.com" {
		t.Fatalf("oauthAccount email=%q", got)
	}

	// User logout strips harness oauthAccount.
	if err := os.WriteFile(filepath.Join(userHome, ".claude.json"), []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSharedAuth(Claude, harness.AuthModeShared); err != nil {
		t.Fatal(err)
	}
	if _, ok := readHarnessOAuth(t, config.HarnessDir(Claude)); ok {
		t.Fatal("oauthAccount should be removed when user has none")
	}
}

func TestSyncClaudeOAuthAccountSeparateSkips(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	if err := os.WriteFile(filepath.Join(userHome, ".claude.json"),
		[]byte(`{"oauthAccount":{"emailAddress":"a@example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	if err := EnsureSharedAuth(Claude, harness.AuthModeSeparate); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(config.HarnessDir(Claude), ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("separate mode must not write oauthAccount: %v", err)
	}
}

func TestEnsureSharedAuthGrokSymlink(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	if err := os.MkdirAll(filepath.Join(userHome, ".grok"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(userHome, ".grok", "auth.json")
	if err := os.WriteFile(src, []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	if err := EnsureSharedAuth(Grok, harness.AuthModeShared); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(config.HarnessDir(Grok), "auth.json")
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("expected symlink to user grok auth.json")
	}
	target, err := os.Readlink(dst)
	if err != nil || target != src {
		t.Fatalf("symlink %q -> %q", dst, target)
	}
}

func TestPrepareLaunch_ClaudePinsDefaultCredStore(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	plan := claudeTool{}.PrepareLaunch("claude")
	if plan.Env["CLAUDE_CONFIG_DIR"] == "" {
		t.Fatal("expected CLAUDE_CONFIG_DIR")
	}
	v, ok := plan.Env["CLAUDE_SECURESTORAGE_CONFIG_DIR"]
	if !ok {
		t.Fatal("shared launch must pin CLAUDE_SECURESTORAGE_CONFIG_DIR")
	}
	if v != "" {
		t.Fatalf("pin must be empty string, got %q", v)
	}
}

func readHarnessOAuthEmail(t *testing.T, dir string) string {
	t.Helper()
	oa, ok := readHarnessOAuth(t, dir)
	if !ok {
		t.Fatal("missing oauthAccount")
	}
	m, _ := oa.(map[string]any)
	s, _ := m["emailAddress"].(string)
	return s
}

func readHarnessOAuth(t *testing.T, dir string) (any, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false
		}
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	oa, ok := top["oauthAccount"]
	return oa, ok
}
