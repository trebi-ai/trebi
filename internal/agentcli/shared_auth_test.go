package agentcli

import (
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
