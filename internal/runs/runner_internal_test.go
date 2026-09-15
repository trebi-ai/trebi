package runs

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/tmux"
)

// TestPreparePlanAppliesModelEffort covers the launch seam: a model/effort
// pair appends the tool's flags on launch and never on resume.
func TestPreparePlanAppliesModelEffort(t *testing.T) {
	tool, ok := tmux.ByName("claude")
	if !ok {
		t.Fatal("claude tool missing")
	}
	opts := LaunchOpts{Model: "sonnet", Effort: "high"}
	plan, err := preparePlan(tool, "claude", &opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Command, "--model sonnet") ||
		!strings.Contains(plan.Command, "--effort high") {
		t.Fatalf("launch command: %q", plan.Command)
	}

	resume := LaunchOpts{ResumeSessionID: "uuid-1", Model: "sonnet", Effort: "high"}
	plan, err = preparePlan(tool, "claude", &resume)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.Command, "--model") || strings.Contains(plan.Command, "--effort") {
		t.Fatalf("resume must keep the session defaults: %q", plan.Command)
	}
}

// TestApplyResumePlanKeepsResumedFrom covers B17 on the real resume path:
// the predecessor run ULID must survive; only an empty ResumedFrom falls
// back to the harness session id.
func TestApplyResumePlanKeepsResumedFrom(t *testing.T) {
	opts := LaunchOpts{
		CreateOpts:      CreateOpts{ResumedFrom: "01PREDECESSOR"},
		ResumeSessionID: "uuid-session-1",
	}
	plan := tmux.LaunchPlan{}
	opts.applyResumePlan(&plan)
	if opts.ResumedFrom != "01PREDECESSOR" {
		t.Fatalf("resumed_from=%q, want predecessor run id", opts.ResumedFrom)
	}
	if opts.SessionID != "uuid-session-1" {
		t.Fatalf("session_id=%q", opts.SessionID)
	}
	if opts.Reason != ReasonResume {
		t.Fatalf("reason=%q", opts.Reason)
	}

	// Empty ResumedFrom keeps the session-id fallback.
	opts2 := LaunchOpts{ResumeSessionID: "uuid-session-2"}
	opts2.applyResumePlan(&tmux.LaunchPlan{SessionID: "uuid-session-2"})
	if opts2.ResumedFrom != "uuid-session-2" {
		t.Fatalf("fallback resumed_from=%q", opts2.ResumedFrom)
	}
}
