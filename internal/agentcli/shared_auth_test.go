package agentcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
)

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
