package runs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// TestPreparePlanAppliesModelEffort covers the launch seam: a model/effort
// pair appends the tool's flags on launch and never on resume.
func TestPreparePlanAppliesModelEffort(t *testing.T) {
	tool, ok := tmux.ByName("claude")
	if !ok {
		t.Fatal("claude tool missing")
	}
	opts := LaunchOpts{CreateOpts: CreateOpts{Model: "sonnet"}, Effort: "high"}
	plan, err := preparePlan(tool, "claude", &opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Command, "--model sonnet") ||
		!strings.Contains(plan.Command, "--effort high") {
		t.Fatalf("launch command: %q", plan.Command)
	}

	resume := LaunchOpts{CreateOpts: CreateOpts{Model: "sonnet"}, ResumeSessionID: "uuid-1", Effort: "high"}
	plan, err = preparePlan(tool, "claude", &resume)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.Command, "--model") || strings.Contains(plan.Command, "--effort") {
		t.Fatalf("resume must keep the session defaults: %q", plan.Command)
	}
}

// TestApplyResumePlanSession keeps the harness session of a resume.
func TestApplyResumePlanSession(t *testing.T) {
	opts := LaunchOpts{ResumeSessionID: "uuid-session-1"}
	opts.applyResumePlan(&tmux.LaunchPlan{})
	if opts.SessionID != "uuid-session-1" || opts.PreviousRunID != "" {
		t.Fatalf("session_id=%q previous_run_id=%q", opts.SessionID, opts.PreviousRunID)
	}
	if opts.Reason != ReasonResume {
		t.Fatalf("reason=%q", opts.Reason)
	}
}

// TestHarnessSessionIDCodexPane: a codex pane run resumes the id in its
// rollout file name, not the trebi id.
func TestHarnessSessionIDCodexPane(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	t.Setenv("HOME", t.TempDir())
	codex := t.TempDir()
	t.Setenv("CODEX_HOME", codex)
	work := t.TempDir()
	started := time.Now().UTC()
	const id = "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	day := filepath.Join(codex, "sessions", started.Format("2006"), started.Format("01"), started.Format("02"))
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"type":"session_meta","payload":{"timestamp":%q,"id":%q,"cwd":%q}}`+"\n", started.Add(time.Second).Format(time.RFC3339Nano), id, work)
	if err := os.WriteFile(filepath.Join(day, "rollout-"+started.Format("2006-01-02T15-04-05")+"-"+id+".jsonl"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	run := Run{Harness: tmux.Codex, Runtime: "tui", SessionID: "trebi-uuid", WorkingDir: work, StartedAt: started}
	if got := run.harnessSessionID(); got != id {
		t.Fatalf("pane id %q, want %q", got, id)
	}
	run.Runtime = "cli"
	if got := run.harnessSessionID(); got != "trebi-uuid" {
		t.Fatalf("cli id %q, want the stored id", got)
	}
}
