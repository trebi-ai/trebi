package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// `trebi agent-hook` wraps the harness body in a payload string, so an
// approval opened from a waiting hook must still carry the question.
func TestHookQuestionReadsWrappedPayload(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
	}{
		{"wrapped", `{"payload":"{\"question\":\"delete prod?\"}"}`, "delete prod?"},
		{"bare", `{"prompt":"delete prod?"}`, "delete prod?"},
		{"wrapped prose", `{"payload":"not json"}`, ""},
		{"empty", `{}`, ""},
		{"garbage", `nope`, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hookQuestion([]byte(tt.body)); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

// Codex mints its own rollout id, so a trebi session id never matches a file
// name. The transcript is found by the directory the run launched in, which
// only reaches this reader because the run row persists it.
func TestReadTranscriptFindsCodexRolloutByWorkingDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	now := time.Now().UTC()
	dir := filepath.Join(home, ".codex", "sessions",
		now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	workingDir := t.TempDir()
	rollout := filepath.Join(dir, "rollout-a-01a00000-0000-7000-8000-000000000001.jsonl")
	line := `{"type":"session_meta","payload":{"cwd":"` + workingDir +
		`","timestamp":"` + now.Add(-time.Minute).Format(time.RFC3339) + `"}}` + "\n"
	if err := os.WriteFile(rollout, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{}
	run := &runs.Run{
		ID: "r1", Harness: tmux.Codex, SessionID: "trebi-minted-id",
		WorkingDir: workingDir, StartedAt: now.Add(-2 * time.Minute),
	}
	if _, _, ok := s.readTranscript(run); !ok {
		t.Fatal("no transcript for a run whose working dir names its rollout")
	}

	// Without the working dir there is nothing to match, which is the bug.
	run.WorkingDir = ""
	if _, _, ok := s.readTranscript(run); ok {
		t.Fatal("resolved a rollout with no working dir to match on")
	}
}
