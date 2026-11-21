package runs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/secrets"
)

func TestMemoryIssues(t *testing.T) {
	t.Parallel()
	missing := memory.Issue{Code: "missing_section", Line: 8, Msg: `missing section "## Lessons"`}
	cases := []struct {
		name   string
		in     memoryIssues
		broken bool
		detail string
	}{
		{"unchanged only", memoryIssues{memory.Unchanged()}, false, ""},
		{"damage and unchanged", memoryIssues{missing, memory.Unchanged()}, true, `Line 8: missing section '## Lessons'.`},
		{"no line", memoryIssues{{Code: "missing", Msg: "file is missing."}}, true, "file is missing."},
		{"two issues", memoryIssues{missing, {Code: "missing_log", Msg: "no Log entry"}}, true, `Line 8: missing section '## Lessons'. no Log entry.`},
	}
	for _, tc := range cases {
		if got := tc.in.broken(); got != tc.broken {
			t.Errorf("%s: broken = %v", tc.name, got)
		}
		if got := tc.in.detail(); got != tc.detail {
			t.Errorf("%s: detail = %q, want %q", tc.name, got, tc.detail)
		}
	}
}

// TestPreparePlanAppliesModelEffort covers the launch seam: a model/effort
// pair appends the tool's flags on launch and never on resume.
func TestPreparePlanAppliesModelEffort(t *testing.T) {
	tool, ok := agentcli.ByName("claude")
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
	opts.applyResumePlan(&agentcli.LaunchPlan{})
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
	run := Run{Harness: agentcli.Codex, Runtime: "tui", SessionID: "trebi-uuid", WorkingDir: work, StartedAt: started}
	if got := run.harnessSessionID(); got != id {
		t.Fatalf("pane id %q, want %q", got, id)
	}
	run.Runtime = "cli"
	if got := run.harnessSessionID(); got != "trebi-uuid" {
		t.Fatalf("cli id %q, want the stored id", got)
	}
}

// TestHarnessVariantLaunch covers the program and the env of a run on a
// variant: the program is the base, and a missing secret stays out.
func TestHarnessVariantLaunch(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Tools.SetPin("claude-zai", &config.ToolPin{Base: "claude", Env: map[string]string{
		"ANTHROPIC_BASE_URL":   "https://api.z.ai/api/anthropic",
		"ANTHROPIC_AUTH_TOKEN": "secret:harness.claude-zai.ANTHROPIC_AUTH_TOKEN",
	}})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		opts LaunchOpts
		want string
	}{
		{LaunchOpts{CreateOpts: CreateOpts{Harness: "claude-zai"}}, "claude"},
		{LaunchOpts{CreateOpts: CreateOpts{Harness: "codex"}}, "codex"},
		{LaunchOpts{CreateOpts: CreateOpts{Harness: "claude-zai"}, Command: "claude --x"}, "claude --x"},
	} {
		if got := tc.opts.harnessCommand(); got != tc.want {
			t.Fatalf("%+v: %q want %q", tc.opts, got, tc.want)
		}
	}
	env := harnessEnvUnder(cfg, "claude-zai", map[string]string{"ANTHROPIC_BASE_URL": "https://job.example"})
	if env["ANTHROPIC_BASE_URL"] != "https://job.example" {
		t.Fatalf("the job env wins: %v", env)
	}
	if _, ok := env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Fatalf("a missing secret passed as a reference: %v", env)
	}
	if err := secrets.Set("harness.claude-zai.ANTHROPIC_AUTH_TOKEN", "tok"); err != nil {
		t.Fatal(err)
	}
	if env := harnessEnvUnder(cfg, "claude-zai", nil); env["ANTHROPIC_AUTH_TOKEN"] != "tok" {
		t.Fatalf("secret: %v", env)
	}
}
