package runs_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestCreateTerminateTimeline(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID:   "ws1",
		Job:           "demo",
		Reason:        runs.ReasonManual,
		Harness:       "claude",
		PromptPreview: "hello world",
		TmuxSession:   "trebi-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != runs.StatusRunning || run.ID == "" {
		t.Fatalf("%+v", run)
	}

	svc.SetAgentState(context.Background(), run.ID, runs.AgentWaiting, "test")
	got, err := svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentState != runs.AgentWaiting {
		t.Fatalf("agent=%s", got.AgentState)
	}

	tokens := &pricing.TokenUsage{Input: 100, Output: 50, Model: "claude"}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndSentinel, tokens); err != nil {
		t.Fatal(err)
	}
	got, err = svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runs.StatusComplete || got.EndReason != runs.EndSentinel {
		t.Fatalf("%+v", got)
	}
	if got.Tokens.Billed != 150 || got.Tokens.CostUSD <= 0 {
		t.Fatalf("tokens=%+v", got.Tokens)
	}

	tl, err := svc.Timeline(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Segments) < 2 {
		t.Fatalf("segments=%d", len(tl.Segments))
	}
	if len(tl.Events) < 1 || tl.Events[0].Kind != "launch" {
		t.Fatalf("events=%+v", tl.Events)
	}

	n, err := svc.RunningCount(ctx)
	if err != nil || n != 0 {
		t.Fatalf("running=%d err=%v", n, err)
	}
}

func TestQueueIDIdempotency(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()

	_, err = svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "j", QueueID: "q1", Harness: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "j", QueueID: "q1", Harness: "claude",
	})
	if err == nil {
		t.Fatal("expected unique queue_id violation")
	}
}

// TestTerminateClosesAgentSegmentAndIsIdempotent covers D10: Terminate closes
// every open segment, and a second Terminate is a no-op.
func TestTerminateClosesAgentSegmentAndIsIdempotent(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "j", Harness: "claude", Reason: runs.ReasonManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetAgentState(ctx, run.ID, runs.AgentRunning, "test")

	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndSentinel, nil); err != nil {
		t.Fatal(err)
	}
	var open int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM run_segments WHERE run_id=? AND ended_at IS NULL`, run.ID).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Fatalf("open segments after terminate=%d want 0", open)
	}

	first, err := svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Second Terminate with a different status must not change the row.
	if err := svc.Terminate(ctx, run.ID, runs.StatusFailed, runs.EndKill, nil); err != nil {
		t.Fatal(err)
	}
	second, err := svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != first.Status || second.EndReason != first.EndReason || second.Revision != first.Revision {
		t.Fatalf("second terminate changed row: first=%+v second=%+v", first, second)
	}
}

// TestCreateMarksPredecessorResumed covers B17: a run created with ResumedFrom
// flips the terminal predecessor to status resumed; a live predecessor stays.
func TestCreateMarksPredecessorResumed(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()

	prev, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", Job: "j", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, prev.ID, runs.StatusComplete, runs.EndSentinel, nil); err != nil {
		t.Fatal(err)
	}

	next, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "j", Harness: "claude",
		ResumedFrom: prev.ID, ResumeAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, prev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runs.StatusResumed {
		t.Fatalf("predecessor status=%s want resumed", got.Status)
	}
	if next.ResumedFrom != prev.ID {
		t.Fatalf("resumed_from=%s", next.ResumedFrom)
	}

	// A still-running predecessor keeps its status.
	live, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", Job: "j2", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "j2", Harness: "claude", ResumedFrom: live.ID,
	}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.Get(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runs.StatusRunning {
		t.Fatalf("live predecessor status=%s want running", got.Status)
	}
}

// TestResumeCap errors when the chain hits MaxResumeAttempts.
func TestResumeCap(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()

	// SessionID present so Resume passes the no-session check and hits the cap.
	prev, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "j", Harness: "claude",
		SessionID:      "sess-cap",
		ResumeAttempts: runs.MaxResumeAttempts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, prev.ID, runs.StatusComplete, runs.EndSentinel, nil); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Resume(ctx, prev.ID)
	if err == nil {
		t.Fatal("expected resume-cap error")
	}
	if !strings.Contains(err.Error(), "resume cap") {
		t.Fatalf("error %q does not mention the resume cap", err)
	}
}

// TestSetAgentStateUnchangedNoBusSpam proves a repeated identical state
// appends no extra agent.state bus row and opens no extra segment.
func TestSetAgentStateUnchangedNoBusSpam(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", Job: "j", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	svc.SetAgentState(ctx, run.ID, runs.AgentWaiting, "test")
	svc.SetAgentState(ctx, run.ID, runs.AgentWaiting, "test") // reaper-style repeat

	var busRows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log
		WHERE topic='agent.state' AND attrs_json LIKE '%"state":"waiting"%'`).Scan(&busRows); err != nil {
		t.Fatal(err)
	}
	if busRows != 1 {
		t.Fatalf("agent.state waiting rows=%d, want 1", busRows)
	}
	var segRows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM run_segments
		WHERE run_id=? AND layer='agent' AND state='waiting'`, run.ID).Scan(&segRows); err != nil {
		t.Fatal(err)
	}
	if segRows != 1 {
		t.Fatalf("waiting agent segments=%d, want 1", segRows)
	}
}

func TestListHarnessAndModelFilters(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()

	claude, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "a", Harness: "claude", Reason: runs.ReasonManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	grok, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "b", Harness: "grok", Reason: runs.ReasonManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, claude.ID, runs.StatusComplete, runs.EndSentinel,
		&pricing.TokenUsage{Input: 1, Output: 1, Model: "sonnet"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, grok.ID, runs.StatusComplete, runs.EndSentinel,
		&pricing.TokenUsage{Input: 1, Output: 1, Model: "grok-4"}); err != nil {
		t.Fatal(err)
	}

	byHarness, err := svc.List(ctx, runs.ListOpts{Harness: "Claude", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(byHarness) != 1 || byHarness[0].ID != claude.ID {
		t.Fatalf("harness filter: got %+v", byHarness)
	}

	byModel, err := svc.List(ctx, runs.ListOpts{Model: "sonnet", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(byModel) != 1 || byModel[0].ID != claude.ID {
		t.Fatalf("model filter: got %+v", byModel)
	}

	both, err := svc.List(ctx, runs.ListOpts{Harness: "grok", Model: "grok-4", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 1 || both[0].ID != grok.ID {
		t.Fatalf("harness+model filter: got %+v", both)
	}
}
