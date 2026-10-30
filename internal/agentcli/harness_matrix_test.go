package agentcli_test

import (
	"testing"

	"github.com/trebi-ai/trebi/internal/agentcli"
)

// TestHarnessMatrix_Registered proves every supported harness has a Tool with
// launch plan, readiness classifier, and name detection (offline, no vendor binary).
func TestHarnessMatrix_Registered(t *testing.T) {
	want := []string{"claude", "grok", "omp", "codex", "opencode", "gemini", "cursor", "copilot", "pi"}
	for _, name := range want {
		tool, ok := agentcli.ByName(name)
		if !ok {
			t.Errorf("missing tool registration for %s", name)
			continue
		}
		if tool.Name() != name && (name != "cursor" || tool.Name() != "cursor") {
			t.Errorf("%s: Name()=%s", name, tool.Name())
		}
		plan := tool.PrepareLaunch(name)
		if plan.Command == "" {
			t.Errorf("%s: empty PrepareLaunch command", name)
		}
		// IsReady should not panic on empty/busy screens
		_ = tool.IsReady("")
		_ = tool.IsReady("something ready-looking")
		_ = tool.HandleStartup("")
		_ = tool.PostReadyDelay()
		mode := agentcli.EventMode(name)
		if mode == "" {
			t.Errorf("%s: empty EventMode", name)
		}
		t.Logf("harness=%s event_mode=%s launch=%q session_id_set=%v",
			name, mode, plan.Command, plan.SessionID != "")
	}
}

func TestDetectPriority(t *testing.T) {
	t.Parallel()
	// Claude wins when command contains claude
	tool := agentcli.Detect("claude --dangerously-skip-permissions")
	if tool.Name() != "claude" {
		t.Fatalf("got %s", tool.Name())
	}
	tool = agentcli.Detect("grok")
	if tool.Name() != "grok" {
		t.Fatalf("got %s", tool.Name())
	}
}
