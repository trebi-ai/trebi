package runs_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestTimelineCoalescesAssistantDeltas pins the timeline view of streamed
// assistant output: one row per message, carrying the final text (or the text
// so far while still streaming), not one row per delta chunk.
func TestTimelineCoalescesAssistantDeltas(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", Job: "demo", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	svc.AddEvent(ctx, run.ID, "usage", "wire", map[string]string{"input": "2", "output": "1"})
	svc.AddEvent(ctx, run.ID, "assistant", "wire", map[string]string{"text": "Hel", "delta": "true"})
	svc.AddEvent(ctx, run.ID, "assistant", "wire", map[string]string{"text": "lo, ", "delta": "true"})
	svc.AddEvent(ctx, run.ID, "assistant", "wire", map[string]string{"text": "Hello, world.", "delta": "false"})
	svc.AddEvent(ctx, run.ID, "tool", "wire", map[string]string{"id": "t1", "name": "Bash", "status": "started"})
	svc.AddEvent(ctx, run.ID, "assistant", "wire", map[string]string{"text": "Second", "delta": "true"})
	svc.AddEvent(ctx, run.ID, "assistant", "wire", map[string]string{"text": " second.", "delta": "true"})

	tl, err := svc.Timeline(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var texts []string
	for _, ev := range tl.Events {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == "assistant" {
			var d map[string]string
			if err := json.Unmarshal(ev.Detail, &d); err != nil {
				t.Fatalf("assistant detail: %s", ev.Detail)
			}
			if _, ok := d["delta"]; ok {
				t.Fatalf("delta field leaked into timeline: %s", ev.Detail)
			}
			texts = append(texts, d["text"])
		}
	}
	if len(texts) != 2 {
		t.Fatalf("assistant rows: %d (%v)", len(texts), kinds)
	}
	if texts[0] != "Hello, world." {
		t.Fatalf("complete message text: %q", texts[0])
	}
	if texts[1] != "Second second." {
		t.Fatalf("streaming message text: %q", texts[1])
	}
	// Order holds: the coalesced message sits before the tool, the streaming
	// one after it.
	assistantIdx := []int{}
	toolIdx := -1
	for i, kind := range kinds {
		if kind == "assistant" {
			assistantIdx = append(assistantIdx, i)
		}
		if kind == "tool" {
			toolIdx = i
		}
	}
	if len(assistantIdx) != 2 || toolIdx < 0 || !(assistantIdx[0] < toolIdx && toolIdx < assistantIdx[1]) {
		t.Fatalf("event order: %v", kinds)
	}
}

// TestTimelineCoalescesToolCalls pins one row per tool call: the stream start
// frame, the complete frame with the input, and the result frame fold into a
// single point at the call's position.
func TestTimelineCoalescesToolCalls(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", Job: "demo", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	svc.AddEvent(ctx, run.ID, "tool", "wire", map[string]string{"id": "t1", "name": "Bash", "status": "started", "input": "{}"})
	svc.AddEvent(ctx, run.ID, "tool", "wire", map[string]string{"id": "t1", "name": "Bash", "status": "started", "input": `{"command":"ls"}`})
	svc.AddEvent(ctx, run.ID, "usage", "wire", map[string]string{"input": "2"})
	svc.AddEvent(ctx, run.ID, "tool", "wire", map[string]string{"id": "t1", "status": "failed", "output": "boom"})
	svc.AddEvent(ctx, run.ID, "tool", "wire", map[string]string{"id": "t2", "name": "Read", "status": "started", "input": "{}"})

	tl, err := svc.Timeline(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var tools []map[string]string
	for _, ev := range tl.Events {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == "tool" {
			var d map[string]string
			if err := json.Unmarshal(ev.Detail, &d); err != nil {
				t.Fatalf("tool detail: %s", ev.Detail)
			}
			if _, ok := d["id"]; ok {
				t.Fatalf("tool id leaked into timeline: %s", ev.Detail)
			}
			tools = append(tools, d)
		}
	}
	if len(tools) != 2 {
		t.Fatalf("tool rows: %d (%v)", len(tools), kinds)
	}
	first := tools[0]
	if first["name"] != "Bash" || first["input"] != `{"command":"ls"}` || first["output"] != "boom" || first["status"] != "failed" {
		t.Fatalf("folded tool: %v", first)
	}
	if tools[1]["name"] != "Read" || tools[1]["status"] != "started" {
		t.Fatalf("open tool: %v", tools[1])
	}
	// The call row keeps the position of its first frame: before usage and
	// before the second call.
	if got := strings.Join(kinds, ","); !strings.HasSuffix(got, "tool,usage,tool") {
		t.Fatalf("event order: %s", got)
	}
}

func TestCreateTerminateTimeline(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
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

	svc.SetSessionState(context.Background(), run.ID, runs.SessionWaiting, "test")
	got, err := svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionState != runs.SessionWaiting {
		t.Fatalf("agent=%s", got.SessionState)
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
	svc := runs.New(st, nil, nil)
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
	svc := runs.New(st, nil, nil)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "j", Harness: "claude", Reason: runs.ReasonManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetSessionState(ctx, run.ID, runs.SessionRunning, "test")

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
	svc := runs.New(st, nil, nil)
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

// TestMarkOutcomeOverridesMachineStatus covers the human outcome: Mark sets the
// verdict without touching status/end_reason, StatusByJob reads the outcome,
// and Clear restores the machine record.
func TestMarkOutcomeOverridesMachineStatus(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", Job: "j", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Mark(ctx, runs.MarkOpts{ID: run.ID, Status: runs.StatusComplete}); !errors.Is(err, runs.ErrNotTerminal) {
		t.Fatalf("mark live run err=%v want ErrNotTerminal", err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusFailed, runs.EndIdleTimeout, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Mark(ctx, runs.MarkOpts{ID: run.ID, Status: runs.StatusRunning}); !errors.Is(err, runs.ErrBadOutcome) {
		t.Fatalf("mark running outcome err=%v want ErrBadOutcome", err)
	}

	marked, err := svc.Mark(ctx, runs.MarkOpts{ID: run.ID, Status: runs.StatusComplete, Note: "digest was sent"})
	if err != nil {
		t.Fatal(err)
	}
	if marked.Status != runs.StatusFailed || marked.EndReason != runs.EndIdleTimeout {
		t.Fatalf("mark changed machine facts: %+v", marked)
	}
	if marked.Outcome == nil || marked.Outcome.Status != runs.StatusComplete || marked.Outcome.Note != "digest was sent" {
		t.Fatalf("outcome=%+v", marked.Outcome)
	}
	if marked.EffectiveStatus() != runs.StatusComplete {
		t.Fatalf("effective=%s", marked.EffectiveStatus())
	}

	// The list status filter follows the outcome: complete finds it, failed not.
	complete, err := svc.List(ctx, runs.ListOpts{Status: string(runs.StatusComplete), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(complete) != 1 || complete[0].ID != run.ID {
		t.Fatalf("complete filter after mark: %+v", complete)
	}
	failed, err := svc.List(ctx, runs.ListOpts{Status: string(runs.StatusFailed), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range failed {
		if r.ID == run.ID {
			t.Fatalf("marked run still matches the failed filter")
		}
	}

	// The stats day buckets (Home chart) read the outcome too.
	stats, err := svc.Stats(ctx, runs.StatsOpts{Range: "30d"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Totals.OK != 1 || stats.Totals.Failed != 0 {
		t.Fatalf("totals after mark: %+v", stats.Totals)
	}
	day := run.StartedAt.UTC().Format("2006-01-02")
	for _, d := range stats.Days {
		if d.Date == day && (d.OK != 1 || d.Failed != 0) {
			t.Fatalf("day bucket after mark: %+v", d)
		}
	}

	last, _, err := svc.StatusByJob(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last["ws/j"] != runs.StatusComplete {
		t.Fatalf("StatusByJob=%s want complete", last["ws/j"])
	}

	tl, err := svc.Timeline(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	markedEvent := false
	for _, ev := range tl.Events {
		if ev.Kind == "run.marked" {
			markedEvent = true
		}
	}
	if !markedEvent {
		t.Fatalf("timeline has no run.marked event: %+v", tl.Events)
	}

	cleared, err := svc.Mark(ctx, runs.MarkOpts{ID: run.ID, Clear: true})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Outcome != nil {
		t.Fatalf("outcome after clear=%+v", cleared.Outcome)
	}
	last, _, err = svc.StatusByJob(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last["ws/j"] != runs.StatusFailed {
		t.Fatalf("StatusByJob after clear=%s want failed", last["ws/j"])
	}
	stats, err = svc.Stats(ctx, runs.StatsOpts{Range: "30d"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Totals.OK != 0 || stats.Totals.Failed != 1 {
		t.Fatalf("totals after clear: %+v", stats.Totals)
	}
}

// TestMarkRejectsSupersededRun: a resumed predecessor is history, not a row a
// human disposes of.
func TestMarkRejectsSupersededRun(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()

	prev, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", Job: "j", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, prev.ID, runs.StatusFailed, runs.EndIdleTimeout, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "j", Harness: "claude",
		ResumedFrom: prev.ID, ResumeAttempts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Mark(ctx, runs.MarkOpts{ID: prev.ID, Status: runs.StatusComplete}); !errors.Is(err, runs.ErrSuperseded) {
		t.Fatalf("mark superseded err=%v want ErrSuperseded", err)
	}
}

// TestResumeCap errors when the chain hits MaxResumeAttempts.
func TestResumeCap(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
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

// TestSetSessionStateUnchangedNoBusSpam proves a repeated identical state
// appends no extra session.state bus row and opens no extra segment.
func TestSetSessionStateUnchangedNoBusSpam(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", Job: "j", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	svc.SetSessionState(ctx, run.ID, runs.SessionWaiting, "test")
	svc.SetSessionState(ctx, run.ID, runs.SessionWaiting, "test") // reaper-style repeat

	var busRows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log
		WHERE topic='session.state' AND attrs_json LIKE '%"state":"waiting"%'`).Scan(&busRows); err != nil {
		t.Fatal(err)
	}
	if busRows != 1 {
		t.Fatalf("session.state waiting rows=%d, want 1", busRows)
	}
	var segRows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM run_segments
		WHERE run_id=? AND layer='session' AND state='waiting'`, run.ID).Scan(&segRows); err != nil {
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
	svc := runs.New(st, nil, nil)
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
	svc := runs.New(st, nil, nil)
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
	svc := runs.New(st, nil, nil)
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
	svc := runs.New(st, nil, nil)
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
	svc := runs.New(st, nil, nil)
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
