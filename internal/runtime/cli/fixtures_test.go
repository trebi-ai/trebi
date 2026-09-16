package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sdkFixtureDir returns the newest SDK-generated fixture directory.
func sdkFixtureDir(t *testing.T) string {
	t.Helper()
	matches, err := filepath.Glob("testdata/claude/*/basic.ndjson")
	if err != nil || len(matches) == 0 {
		t.Skip("no SDK-generated fixtures; run scripts/gen-claude-fixtures")
	}
	return filepath.Dir(matches[len(matches)-1])
}

// TestClaudeSDKFixtureReplay replays real SDK-recorder sessions through the
// Go driver (Part L.1): init + session id, assistant text, result taxonomy,
// and the control_request/can_use_tool surface.
func TestClaudeSDKFixtureReplay(t *testing.T) {
	t.Parallel()
	dir := sdkFixtureDir(t)

	basic := replayFixture(t, filepath.Join(dir, "basic.ndjson"), newClaudeProtocol())
	var init, result *Event
	var text strings.Builder
	for i := range basic {
		switch basic[i].Type {
		case EventInit:
			init = &basic[i]
		case EventAssistant:
			text.WriteString(basic[i].Text)
		case EventResult:
			result = &basic[i]
		}
	}
	if init == nil || init.SessionID == "" {
		t.Fatalf("no init/session id: %+v", basic)
	}
	if !strings.Contains(strings.ToUpper(text.String()), "PONG") {
		t.Fatalf("assistant text = %q", text.String())
	}
	if result == nil || result.Result == nil || result.Result.IsError || result.Result.Subtype != "success" {
		t.Fatalf("result = %+v", result)
	}
	if result.Result.Usage.Output <= 0 {
		t.Fatalf("usage = %+v", result.Result.Usage)
	}
	if result.Result.SessionID == "" || result.Result.SessionID != init.SessionID {
		t.Fatalf("session id mismatch: init=%q result=%q", init.SessionID, result.Result.SessionID)
	}

	for _, name := range []string{"permission_allow.ndjson", "permission_deny.ndjson"} {
		evs := replayFixture(t, filepath.Join(dir, name), newClaudeProtocol())
		var perm *Event
		for i := range evs {
			if evs[i].Type == EventPermission {
				perm = &evs[i]
			}
		}
		if perm == nil || perm.Permission == nil || perm.Permission.Tool == "" {
			t.Fatalf("%s: no can_use_tool permission: %+v", name, evs)
		}
		if !strings.Contains(perm.Permission.Input, "file_path") {
			t.Fatalf("%s: permission input not carried: %q", name, perm.Permission.Input)
		}
	}
}

// TestClaudeSDKFixtureMeta pins the recorded SDK/CLI versions.
func TestClaudeSDKFixtureMeta(t *testing.T) {
	t.Parallel()
	dir := sdkFixtureDir(t)
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatalf("meta.json: %v", err)
	}
	for _, want := range []string{"\"sdk\"", "\"cli\"", "\"recorded_at\""} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("meta.json missing %s: %s", want, data)
		}
	}
}
