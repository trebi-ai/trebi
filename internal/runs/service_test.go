package runs_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestDecideApprovalAtomicWithBus(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", Job: "demo", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.DecideApproval(ctx, run.ID, "", "allow")
	if err != nil {
		t.Fatal(err)
	}
	if a.Revision != 1 || a.Decision != "allow" || a.RunID != run.ID {
		t.Fatalf("%+v", a)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM approvals WHERE id=?`, a.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("row n=%d err=%v", n, err)
	}
	evs, err := st.ListBus(ctx, 0, []string{"approval.decided"}, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("bus %d %v", len(evs), err)
	}
	if evs[0].Revision != 1 || evs[0].ID != a.ID {
		t.Fatalf("frame %+v", evs[0])
	}

	if _, err := svc.DecideApproval(ctx, run.ID, "", ""); err == nil {
		t.Fatal("expected empty decision rejected")
	}
	if _, err := svc.DecideApproval(ctx, run.ID, "", "nope"); err == nil {
		t.Fatal("expected invalid decision")
	}
	if _, err := svc.DecideApproval(ctx, "missing", "", "allow"); err == nil {
		t.Fatal("expected missing run")
	}
}

func TestOpenApprovalThenDecide(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", Job: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	open, err := svc.OpenApproval(ctx, run.ID, "may I write the file?")
	if err != nil {
		t.Fatal(err)
	}
	if open.Status != "pending" || open.Prompt != "may I write the file?" {
		t.Fatalf("%+v", open)
	}
	again, err := svc.OpenApproval(ctx, run.ID, "second ask")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != open.ID {
		t.Fatal("a repeated hook must not stack a second pending row")
	}

	pending, err := svc.ListApprovals(ctx, runs.ApprovalListOpts{State: "pending"})
	if err != nil || len(pending) != 1 || pending[0].Prompt != "may I write the file?" {
		t.Fatalf("pending %+v %v", pending, err)
	}

	decided, err := svc.DecideApproval(ctx, run.ID, "", "allow")
	if err != nil {
		t.Fatal(err)
	}
	if decided.ID != open.ID || decided.Status != "decided" || decided.Revision != 2 {
		t.Fatalf("decide answered the wrong row: %+v", decided)
	}
	if left, err := svc.ListApprovals(ctx, runs.ApprovalListOpts{State: "pending"}); err != nil || len(left) != 0 {
		t.Fatalf("still pending %+v %v", left, err)
	}

	other, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", Job: "demo2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DecideApproval(ctx, other.ID, open.ID, "allow"); !errors.Is(err, runs.ErrApprovalRunMismatch) {
		t.Fatalf("cross-run approval id: %v", err)
	}
}

func TestWaitFor(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", Job: "demo", Reason: runs.ReasonManual})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	got, err := svc.WaitFor(ctx, run.ID, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.IsTerminal() {
		t.Fatalf("a running run must not read as terminal: %s", got.Status)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("wait returned before the deadline")
	}

	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndSentinel, nil); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	got, err = svc.WaitFor(ctx, run.ID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runs.StatusComplete {
		t.Fatalf("status=%s", got.Status)
	}
	if time.Since(start) > time.Second {
		t.Fatal("a terminal run must return at once")
	}
}

func TestStatsRangeAndMTD(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()

	rep, err := svc.Stats(ctx, runs.StatsOpts{Range: "7d"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Range != "7d" || rep.Totals.Runs != 0 {
		t.Fatalf("empty store: %+v", rep)
	}
	if len(rep.Days) != 8 {
		t.Fatalf("7d must cover 8 day buckets, got %d", len(rep.Days))
	}

	run, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws1", Job: "demo", Reason: runs.ReasonManual, Harness: "fake",
	})
	if err != nil {
		t.Fatal(err)
	}
	usage := &pricing.TokenUsage{Input: 1000, Output: 500, Model: "claude"}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndSentinel, usage); err != nil {
		t.Fatal(err)
	}

	rep, err = svc.Stats(ctx, runs.StatsOpts{Range: "24h"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Totals.Runs != 1 || rep.Totals.OK != 1 || rep.MTD.Runs != 1 {
		t.Fatalf("after one run: %+v", rep)
	}
	if rep.Totals.TokensBilled != 1500 || rep.Totals.CostUSD <= 0 {
		t.Fatalf("cost and tokens: %+v", rep.Totals)
	}
	if rep.Days[len(rep.Days)-1].Runs != 1 {
		t.Fatalf("today bucket must hold the run: %+v", rep.Days)
	}

	rep, err = svc.Stats(ctx, runs.StatsOpts{Range: "24h", Job: "other"})
	if err != nil || rep.Totals.Runs != 0 {
		t.Fatalf("job filter: %+v %v", rep, err)
	}
}
