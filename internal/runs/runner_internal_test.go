package runs

import (
	"testing"

	"github.com/trebi-ai/trebi/internal/tmux"
)

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
