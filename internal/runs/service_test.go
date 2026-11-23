package runs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestTerminateLogsRunFinish pins the ops record that the run's Log tab shows:
// every terminal transition writes one "run finished" line naming the run.
func TestTerminateLogsRunFinish(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndSentinel, nil); err != nil {
		t.Fatal(err)
	}
	// Idempotent second call writes no second record.
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndSentinel, nil); err != nil {
		t.Fatal(err)
	}

	logs := buf.String()
	if got := strings.Count(logs, "run finished"); got != 1 {
		t.Fatalf("run finished records: %d\n%s", got, logs)
	}
	if !strings.Contains(logs, "id="+run.ID) ||
		!strings.Contains(logs, "status=complete") ||
		!strings.Contains(logs, "end_reason=sentinel") {
		t.Fatalf("finish record fields:\n%s", logs)
	}
}

// TestResumeRefusesSuccessfulRun pins the guard: a run that finished its job
// must not be resumed (the vendor session is complete); an interrupted run
// still reopens as its next turn.
func TestResumeRefusesSuccessfulRun(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()

	mk := func() *runs.Run {
		t.Helper()
		run, err := svc.Create(ctx, runs.CreateOpts{
			WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Harness: "fake", SessionID: "sess-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}

	done := mk()
	if err := svc.Terminate(ctx, done.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resume(ctx, done.ID, runs.LaunchOpts{}, runs.ResumeOpts{}); err == nil || !strings.Contains(err.Error(), "finished successfully") {
		t.Fatalf("resume of a successful run: err=%v", err)
	}

	killed := mk()
	if err := svc.Terminate(ctx, killed.ID, runs.StatusExited, runs.EndKill, nil); err != nil {
		t.Fatal(err)
	}
	next, err := svc.Resume(ctx, killed.ID, runs.LaunchOpts{}, runs.ResumeOpts{TraceID: "tr1", HopID: "hop_1"})
	if err != nil {
		t.Fatalf("resume of an interrupted run: %v", err)
	}
	if next.ID != killed.ID || next.TurnCount != 2 || next.ResumeAttempts != 1 {
		t.Fatalf("reopened: %+v", next)
	}
	// The reopen frame carries the chain of the new turn.
	var trace, attrs string
	if err := st.DB().QueryRow(`SELECT trace_id, attrs_json FROM event_log WHERE topic='run.updated' AND entity_id=? AND attrs_json LIKE '%"turn":"2"%'`,
		killed.ID).Scan(&trace, &attrs); err != nil {
		t.Fatal(err)
	}
	if trace != "tr1" || !strings.Contains(attrs, `"hop_id":"hop_1"`) {
		t.Fatalf("reopen frame trace=%q attrs=%s", trace, attrs)
	}
}

// TestAddEventPublishesRunEvent pins the live-view wakeup: significant run
// events ride the bus as run.event, while streamed assistant deltas and
// session.state (which has its own topic) do not.
func TestAddEventPublishesRunEvent(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	svc.AddEvent(ctx, run.ID, "assistant", "wire", map[string]string{"text": "Hello.", "delta": "false"})
	svc.AddEvent(ctx, run.ID, "assistant", "wire", map[string]string{"text": "Hel", "delta": "true"})
	svc.AddEvent(ctx, run.ID, "tool", "wire", map[string]string{"id": "t1", "name": "Bash", "status": "started"})
	svc.AddEvent(ctx, run.ID, "session.state", "wire", map[string]string{"state": "idle"})

	events, err := st.ListBus(ctx, 0, []string{"run.event"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("run.event frames: %d %+v", len(events), events)
	}
	for _, ev := range events {
		if ev.Entity != "run" || ev.ID != run.ID {
			t.Fatalf("frame addressing: %+v", ev)
		}
		var attrs map[string]any
		if err := json.Unmarshal(ev.Attrs, &attrs); err != nil {
			t.Fatal(err)
		}
		if attrs["run_id"] != run.ID {
			t.Fatalf("attrs.run_id: %v", attrs)
		}
	}
}

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

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Harness: "claude"})
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

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Harness: "claude"})
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
		WorkspaceID: "ws1",
		JobID:       "demo", JobName: "demo",
		Reason:        runs.ReasonManual,
		Harness:       "claude",
		PromptPreview: "hello world",
		PaneSession:   "trebi-test",
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
		WorkspaceID: "ws", JobID: "j", JobName: "j", QueueID: "q1", Harness: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "j", JobName: "j", QueueID: "q1", Harness: "claude",
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
		WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude", Reason: runs.ReasonManual,
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

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude"})
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
	if last["j"] != runs.StatusComplete {
		t.Fatalf("StatusByJob=%s want complete", last["j"])
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
	if last["j"] != runs.StatusFailed {
		t.Fatalf("StatusByJob after clear=%s want failed", last["j"])
	}
	stats, err = svc.Stats(ctx, runs.StatsOpts{Range: "30d"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Totals.OK != 0 || stats.Totals.Failed != 1 {
		t.Fatalf("totals after clear: %+v", stats.Totals)
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
		WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude",
		SessionID:      "sess-cap",
		ResumeAttempts: runs.MaxResumeAttempts,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Interrupted, not finished: a successful run is refused before the cap.
	if err := svc.Terminate(ctx, prev.ID, runs.StatusExited, runs.EndKill, nil); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Resume(ctx, prev.ID, runs.LaunchOpts{}, runs.ResumeOpts{})
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

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude"})
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
		WorkspaceID: "ws", JobID: "a", JobName: "a", Harness: "claude", Reason: runs.ReasonManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	grok, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "b", JobName: "b", Harness: "grok", Reason: runs.ReasonManual,
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

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "claude"})
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

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "demo", JobName: "demo"})
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

	other, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "demo2", JobName: "demo2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DecideApproval(ctx, other.ID, open.ID, "allow"); !errors.Is(err, runs.ErrApprovalRunMismatch) {
		t.Fatalf("cross-run approval id: %v", err)
	}
}

// TestApprovalsCloseWithTheTurn: an ended turn has no session to answer an
// approval, so the turn end expires it, an ended run opens none, and the boot
// sweep closes the rows that an earlier build left.
func TestApprovalsCloseWithTheTurn(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()
	pending := func() int {
		t.Helper()
		list, err := svc.ListApprovals(ctx, runs.ApprovalListOpts{State: "pending"})
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "demo", JobName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenApprovalKeyed(ctx, run.ID, "may I run ls?", "perm-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenApproval(ctx, run.ID, "Tool permission requested"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	if n := pending(); n != 0 {
		t.Fatalf("pending after the turn end = %d", n)
	}
	if _, err := svc.OpenApproval(ctx, run.ID, "late hook"); !errors.Is(err, runs.ErrNotRunning) {
		t.Fatalf("approval on an ended run: %v", err)
	}

	stale, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "demo2", JobName: "demo2"})
	if err != nil {
		t.Fatal(err)
	}
	live, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "demo3", JobName: "demo3"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{stale.ID, live.ID} {
		if _, err := svc.OpenApproval(ctx, id, "ask"); err != nil {
			t.Fatal(err)
		}
	}
	// An earlier build ended the run and left its row.
	if _, err := st.DB().ExecContext(ctx, `UPDATE runs SET status='complete' WHERE id=?`, stale.ID); err != nil {
		t.Fatal(err)
	}
	n, err := svc.ExpireStaleApprovals(ctx)
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d %v", n, err)
	}
	if a, err := svc.PendingApproval(ctx, live.ID); err != nil || a == nil {
		t.Fatalf("the live run lost its approval: %v %v", a, err)
	}
	if n := pending(); n != 1 {
		t.Fatalf("pending after the sweep = %d", n)
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

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Reason: runs.ReasonManual})
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
		WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Reason: runs.ReasonManual, Harness: "fake",
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

// TestStatsTokenSplit checks the token counters per range. The split sums
// to tokens_billed plus tokens_cached, and the day rows sum to the totals.
func TestStatsTokenSplit(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	type seed struct {
		age   time.Duration
		usage pricing.TokenUsage
	}
	seeds := []seed{
		{0, pricing.TokenUsage{Input: 100, Output: 50, CacheRead: 1000, CacheCreation: 10, Model: "claude"}},
		{10 * 24 * time.Hour, pricing.TokenUsage{Input: 7, Output: 3, CacheRead: 20, CacheCreation: 5, Model: "claude"}},
		{40 * 24 * time.Hour, pricing.TokenUsage{Input: 1, Output: 1, CacheRead: 1, CacheCreation: 1, Model: "claude"}},
	}
	for _, sd := range seeds {
		run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Reason: runs.ReasonManual, Harness: "fake"})
		if err != nil {
			t.Fatal(err)
		}
		u := sd.usage
		if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndSentinel, &u); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-sd.age).Format(time.RFC3339)
		if _, err := st.DB().Exec(`UPDATE runs SET started_at=? WHERE id=?`, at, run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`UPDATE run_turns SET started_at=? WHERE run_id=?`, at, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	sum := func(in func(time.Duration) bool) runs.StatsTotals {
		var out runs.StatsTotals
		for _, sd := range seeds {
			if !in(sd.age) {
				continue
			}
			u := sd.usage
			out.Runs++
			out.TokensInput += u.Input
			out.TokensOutput += u.Output
			out.TokensCacheRead += u.CacheRead
			out.TokensCacheCreation += u.CacheCreation
			out.TokensBilled += u.Input + u.Output + u.CacheCreation
			out.TokensCached += u.CacheRead
		}
		return out
	}
	within := func(d time.Duration) func(time.Duration) bool {
		return func(age time.Duration) bool { return age < d }
	}
	tests := []struct {
		rng  string
		want runs.StatsTotals
	}{
		{"24h", sum(within(24 * time.Hour))},
		{"7d", sum(within(7 * 24 * time.Hour))},
		{"30d", sum(within(30 * 24 * time.Hour))},
		{"mtd", sum(func(age time.Duration) bool { return !now.Add(-age).Before(monthStart) })},
	}
	tokens := func(x runs.StatsTotals) [7]int64 {
		return [7]int64{int64(x.Runs), x.TokensInput, x.TokensOutput, x.TokensCacheRead, x.TokensCacheCreation, x.TokensBilled, x.TokensCached}
	}
	for _, tc := range tests {
		t.Run(tc.rng, func(t *testing.T) {
			rep, err := svc.Stats(ctx, runs.StatsOpts{Range: tc.rng})
			if err != nil {
				t.Fatal(err)
			}
			if tokens(rep.Totals) != tokens(tc.want) {
				t.Fatalf("totals %+v, want %+v", rep.Totals, tc.want)
			}
			if tokens(rep.MTD) != tokens(tests[3].want) {
				t.Fatalf("mtd %+v, want %+v", rep.MTD, tests[3].want)
			}
			got := rep.Totals
			if got.TokensInput+got.TokensOutput+got.TokensCacheRead+got.TokensCacheCreation != got.TokensBilled+got.TokensCached {
				t.Fatalf("split does not sum to billed plus cached: %+v", got)
			}
			var days runs.StatsTotals
			for _, d := range rep.Days {
				days.Runs += d.Runs
				days.TokensInput += d.TokensInput
				days.TokensOutput += d.TokensOutput
				days.TokensCacheRead += d.TokensCacheRead
				days.TokensCacheCreation += d.TokensCacheCreation
				days.TokensBilled += d.TokensBilled
				days.TokensCached += d.TokensCached
			}
			if tokens(days) != tokens(rep.Totals) {
				t.Fatalf("day rows %+v, want %+v", days, rep.Totals)
			}
		})
	}
}

// TestTraceUsageSumsTokens checks that a chain trace sums the turn count,
// the cost, and the token split of its turns.
func TestTraceUsageSumsTokens(t *testing.T) {
	t.Parallel()
	svc, _, ctx := openTurnSvc(t)
	var want pricing.TokenTotals
	for _, u := range []pricing.TokenUsage{
		{Input: 100, Output: 50, CacheRead: 1000, CacheCreation: 10, Model: "claude"},
		{Input: 7, Output: 3, CacheRead: 20, CacheCreation: 5, Model: "claude"},
	} {
		run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "fake", TraceID: "T1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndSentinel, &u); err != nil {
			t.Fatal(err)
		}
		want.Add(u)
	}
	got, err := svc.TraceUsage(ctx, "T1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Turns != 2 || got.Tokens != want || got.CostUSD <= 0 {
		t.Fatalf("trace usage %+v, want 2 turns and %+v", got, want)
	}
}

// gateStub is a request ParkGate with a fixed answer that records RunEnded.
type gateStub struct {
	open  bool
	hint  string
	mu    sync.Mutex
	ended []runs.Status
}

func (g *gateStub) Name() string { return runs.GateRequest }

func (g *gateStub) Open(context.Context, string) (bool, bool, error) { return g.open, false, nil }

func (g *gateStub) Claim(context.Context, string) ([]runs.ParkDelivery, error) { return nil, nil }

func (g *gateStub) Settle(context.Context, string, []runs.ParkDelivery, runs.ResumeResult) error {
	return nil
}

func (g *gateStub) Stale(context.Context) ([]string, error) { return nil, nil }

func (g *gateStub) PromptHint(context.Context, runs.LaunchOpts) string { return g.hint }

func (g *gateStub) RunEnded(_ context.Context, _ string, st runs.Status) {
	g.mu.Lock()
	g.ended = append(g.ended, st)
	g.mu.Unlock()
}

// TestTerminateParksWithOpenRequest pins the park rule: a clean end with an
// open request parks; a failure stays a failure. RunEnded fires once.
func TestTerminateParksWithOpenRequest(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	gate := &gateStub{open: true}
	svc.SetParker(runs.NewParker(svc, nil, gate))
	ctx := context.Background()

	for _, tc := range []struct {
		status     runs.Status
		end        runs.EndReason
		wantStatus runs.Status
		wantEnd    runs.EndReason
	}{
		{runs.StatusComplete, runs.EndResult, runs.StatusParked, runs.EndRequest},
		{runs.StatusExited, runs.EndKill, runs.StatusParked, runs.EndRequest},
		{runs.StatusFailed, runs.EndFailed, runs.StatusFailed, runs.EndFailed},
	} {
		gate.mu.Lock()
		gate.ended = nil
		gate.mu.Unlock()
		run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Harness: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.Terminate(ctx, run.ID, tc.status, tc.end, nil); err != nil {
			t.Fatal(err)
		}
		if err := svc.Terminate(ctx, run.ID, tc.status, tc.end, nil); err != nil {
			t.Fatal(err)
		}
		got, err := svc.Get(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != tc.wantStatus || got.EndReason != tc.wantEnd {
			t.Fatalf("%s: got %s/%s", tc.status, got.Status, got.EndReason)
		}
		gate.mu.Lock()
		ended := slices.Clone(gate.ended)
		gate.mu.Unlock()
		if len(ended) != 1 || ended[0] != tc.wantStatus {
			t.Fatalf("%s: RunEnded calls %v", tc.status, ended)
		}
	}
}

// TestUnparkCompletesParkedRun pins the dismiss path.
func TestUnparkCompletesParkedRun(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	gate := &gateStub{open: true}
	svc.SetParker(runs.NewParker(svc, nil, gate))
	ctx := context.Background()
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	// A gate with an open item keeps the run parked.
	if err := svc.Unpark(ctx, run.ID, runs.EndRequestDismissed); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(ctx, run.ID); got.Status != runs.StatusParked {
		t.Fatalf("unpark with open item: %s", got.Status)
	}
	gate.open = false
	if err := svc.Unpark(ctx, run.ID, runs.EndRequestDismissed); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(ctx, run.ID)
	if got.Status != runs.StatusComplete || got.EndReason != runs.EndRequestDismissed {
		t.Fatalf("got %s/%s", got.Status, got.EndReason)
	}
	if err := svc.Unpark(ctx, run.ID, runs.EndRequestDismissed); !errors.Is(err, runs.ErrNotParked) {
		t.Fatalf("second unpark: %v", err)
	}
}

// TestReopenTurns: a reopen adds a turn to the same run, and the run sums
// the tokens and the active time of its turns.
func TestReopenTurns(t *testing.T) {
	t.Parallel()
	svc, _, ctx := openTurnSvc(t)
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude", Reason: runs.ReasonManual, QueueID: "q1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, &pricing.TokenUsage{Input: 100, Output: 10}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	got, err := svc.Reopen(ctx, run.ID, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude", Reason: runs.ReasonResume, QueueID: "q2"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != run.ID || got.Status != runs.StatusRunning || got.EndedAt != nil || got.TurnCount != 2 {
		t.Fatalf("reopened run %+v", got)
	}
	time.Sleep(5 * time.Millisecond)
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, &pricing.TokenUsage{Input: 50, Output: 5}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	turns, err := svc.Turns(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Reason != runs.ReasonManual || turns[1].Reason != runs.ReasonResume ||
		turns[0].QueueID != "q1" || turns[1].QueueID != "q2" {
		t.Fatalf("turns %+v", turns)
	}
	if got.Tokens.Input != 150 || got.Tokens.Output != 15 || turns[1].Tokens.Input != 50 {
		t.Fatalf("run tokens %+v turn 2 tokens %+v", got.Tokens, turns[1].Tokens)
	}
	if got.DurationMS != turns[0].DurationMS+turns[1].DurationMS || got.DurationMS <= 0 {
		t.Fatalf("active %d, turns %d + %d", got.DurationMS, turns[0].DurationMS, turns[1].DurationMS)
	}
	all, err := svc.List(ctx, runs.ListOpts{Workspace: "ws"})
	if err != nil || len(all) != 1 {
		t.Fatalf("list %d runs, err %v", len(all), err)
	}
}

// TestReopenClosesStaleIdleSegment: an idle session row that outlives the
// in-memory snapshot (a restart) closes when the next turn starts, with its own
// duration, not the duration of the next turn.
func TestReopenClosesStaleIdleSegment(t *testing.T) {
	t.Parallel()
	svc, st, ctx := openTurnSvc(t)
	opts := runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude", Reason: runs.ReasonManual}
	run, err := svc.Create(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	svc.SetSessionState(ctx, run.ID, runs.SessionIdle, "test")
	time.Sleep(30 * time.Millisecond)
	restarted := runs.New(st, nil, nil)
	opts.Reason = runs.ReasonResume
	if _, err := restarted.Reopen(ctx, run.ID, opts); err != nil {
		t.Fatal(err)
	}
	restarted.SetSessionState(ctx, run.ID, runs.SessionRunning, "test")
	time.Sleep(5 * time.Millisecond)
	if err := restarted.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	tl, err := restarted.Timeline(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var idle, running *runs.TimelineSegment
	for i := range tl.Segments {
		seg := &tl.Segments[i]
		if seg.Layer != "session" || seg.End == nil {
			continue
		}
		switch seg.State {
		case "idle":
			idle = seg
		case "running":
			running = seg
		}
	}
	if idle == nil || running == nil {
		t.Fatalf("segments %+v", tl.Segments)
	}
	if idle.End.After(running.Start) {
		t.Fatalf("idle ends %s after running starts %s", idle.End, running.Start)
	}
	if want := idle.End.Sub(idle.Start).Milliseconds(); idle.DurationMS == nil || *idle.DurationMS != want || want < 25 {
		t.Fatalf("idle duration %v, want %d", idle.DurationMS, want)
	}
}

// TestReopenLiveRun: a running run cannot reopen.
func TestReopenLiveRun(t *testing.T) {
	t.Parallel()
	svc, _, ctx := openTurnSvc(t)
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reopen(ctx, run.ID, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Reason: runs.ReasonResume}); !errors.Is(err, runs.ErrRunActive) {
		t.Fatalf("err %v, want ErrRunActive", err)
	}
	if turns, _ := svc.Turns(ctx, run.ID); len(turns) != 1 {
		t.Fatalf("turns %+v", turns)
	}
}

// TestReopenRace: of two concurrent reopens, one wins.
func TestReopenRace(t *testing.T) {
	t.Parallel()
	svc, _, ctx := openTurnSvc(t)
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = svc.Reopen(ctx, run.ID, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Reason: runs.ReasonTrigger})
		}()
	}
	wg.Wait()
	ok, active := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, runs.ErrRunActive):
			active++
		default:
			t.Fatal(err)
		}
	}
	got, _ := svc.Get(ctx, run.ID)
	if ok != 1 || active != 1 || got.TurnCount != 2 {
		t.Fatalf("ok %d active %d turns %d", ok, active, got.TurnCount)
	}
}

// TestReopenClearsOutcome: the human outcome of the last turn does not carry
// into the new turn.
func TestReopenClearsOutcome(t *testing.T) {
	t.Parallel()
	svc, _, ctx := openTurnSvc(t)
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Mark(ctx, runs.MarkOpts{ID: run.ID, Status: runs.StatusFailed, Note: "wrong"}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Reopen(ctx, run.ID, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Reason: runs.ReasonTrigger})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != nil || got.EffectiveStatus() != runs.StatusRunning {
		t.Fatalf("outcome %+v status %s", got.Outcome, got.EffectiveStatus())
	}
}

// TestReopenKeepsTerminalGuard: a late end of turn 1 does not close turn 2,
// and a second end of turn 2 does nothing.
func TestReopenKeepsTerminalGuard(t *testing.T) {
	t.Parallel()
	svc, _, ctx := openTurnSvc(t)
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.TerminateTurn(ctx, run.ID, 1, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reopen(ctx, run.ID, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Reason: runs.ReasonTrigger}); err != nil {
		t.Fatal(err)
	}
	if err := svc.TerminateTurn(ctx, run.ID, 1, runs.StatusFailed, runs.EndKill, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(ctx, run.ID); got.Status != runs.StatusRunning {
		t.Fatalf("stale end closed turn 2: %s", got.Status)
	}
	if err := svc.TerminateTurn(ctx, run.ID, 2, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.TerminateTurn(ctx, run.ID, 2, runs.StatusFailed, runs.EndKill, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(ctx, run.ID)
	turns, _ := svc.Turns(ctx, run.ID)
	if got.Status != runs.StatusComplete || len(turns) != 2 || turns[0].Status != runs.StatusComplete || turns[1].Status != runs.StatusComplete {
		t.Fatalf("run %s turns %+v", got.Status, turns)
	}
}

// TestTurnQueueIDUnique: one queue item starts at most one turn.
func TestTurnQueueIDUnique(t *testing.T) {
	t.Parallel()
	svc, _, ctx := openTurnSvc(t)
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude", QueueID: "q1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reopen(ctx, run.ID, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Reason: runs.ReasonTrigger, QueueID: "q1"}); err == nil {
		t.Fatal("expected a unique queue id error")
	}
	got, _ := svc.Get(ctx, run.ID)
	if got.TurnCount != 1 || got.Status != runs.StatusComplete {
		t.Fatalf("failed reopen changed the run: turns %d status %s", got.TurnCount, got.Status)
	}
}

// TestCostByTurnMonth: a turn counts in the month it started, not the month
// of its run.
func TestCostByTurnMonth(t *testing.T) {
	t.Parallel()
	svc, st, ctx := openTurnSvc(t)
	const agentID, jobID = "agt_00000000a1", "jb_00000000b1"
	seedIndex(t, st, agentID, "ws", "a", "agent", "")
	seedIndex(t, st, jobID, "ws", "a/build", "job", agentID)
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: jobID, JobName: "a/build", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	usage := pricing.TokenUsage{Input: 1_000_000, Model: "default"}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, &usage); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	lastMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Add(-24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`UPDATE run_turns SET started_at=? WHERE run_id=? AND n=1`, lastMonth, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE runs SET started_at=? WHERE id=?`, lastMonth, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reopen(ctx, run.ID, runs.CreateOpts{WorkspaceID: "ws", JobID: jobID, JobName: "a/build", Reason: runs.ReasonTrigger}); err != nil {
		t.Fatal(err)
	}
	usage = pricing.TokenUsage{Input: 1_000_000, Model: "default"}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, &usage); err != nil {
		t.Fatal(err)
	}
	want := pricing.TokenUsage{Input: 1_000_000, Model: "default"}
	want.ApplyRates()
	spends, err := svc.OwnerSpend(ctx, "ws", []string{agentID}, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got := spends[0][agentID]; want.CostUSD <= 0 || got.CostUSD != want.CostUSD || got.Tokens.Input != 1_000_000 || got.Tokens.Billed != 1_000_000 {
		t.Fatalf("spend %+v, want cost %v and 1M input tokens (turn 2 only)", got, want.CostUSD)
	}
}

// TestTurnAfter finds the turn that a request answer started.
func TestTurnAfter(t *testing.T) {
	t.Parallel()
	svc, _, ctx := openTurnSvc(t)
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Millisecond)
	if n, err := svc.TurnAfter(ctx, run.ID, runs.ReasonRequest, since); err != nil || n != 0 {
		t.Fatalf("before reopen n %d err %v", n, err)
	}
	if _, err := svc.Reopen(ctx, run.ID, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Reason: runs.ReasonRequest}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		reason runs.RunReason
		since  time.Time
		want   int
	}{
		{runs.ReasonRequest, since, 2},
		{runs.ReasonResume, since, 0},
		{runs.ReasonRequest, time.Now().Add(time.Hour), 0},
	}
	for _, c := range cases {
		if n, err := svc.TurnAfter(ctx, run.ID, c.reason, c.since); err != nil || n != c.want {
			t.Fatalf("%s since %v: n %d err %v, want %d", c.reason, c.since, n, err, c.want)
		}
	}
}

// seedIndex adds a job_index row, so that the name and owner of id resolve.
func seedIndex(t *testing.T, st *store.Store, id, ws, name, kind, ownerID string) {
	t.Helper()
	if _, err := st.DB().Exec(`INSERT INTO job_index (id, workspace_id, name, path, kind, owner_id, updated_at) VALUES (?,?,?,?,?,?,?)`,
		id, ws, name, "/"+name, kind, ownerID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
}

// TestListForTurnWatchSelectsOwner: the turn-watch list carries the owner of
// each run and its end reason, because the reply and report mails read them.
func TestListForTurnWatchSelectsOwner(t *testing.T) {
	svc, st, ctx := openTurnSvc(t)
	seedIndex(t, st, "jb_agent", "ws1", "research", "agent", "")
	run, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws1", JobID: "jb_agent", JobName: "research", Kind: runs.KindAgent,
		Reason: runs.ReasonReport, ConversationID: "conv1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusFailed, runs.EndFailed, nil); err != nil {
		t.Fatal(err)
	}
	list, err := svc.ListForTurnWatch(ctx, time.Now().Add(-time.Hour))
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v %v", list, err)
	}
	got := list[0]
	if got.Workspace != "ws1" || got.JobID != "jb_agent" || got.JobName != "research" ||
		got.EndReason != runs.EndFailed || got.Reason != runs.ReasonReport {
		t.Fatalf("run = %+v", got)
	}
}

func openTurnSvc(t *testing.T) (*runs.Service, *store.Store, context.Context) {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return runs.New(st, nil, nil), st, context.Background()
}

// readyGate has one delivery ready per Claim, then nothing.
type readyGate struct {
	name    string
	waiting bool
	d       []runs.ParkDelivery
	settled []runs.ResumeResult
}

func (g *readyGate) Name() string { return g.name }

func (g *readyGate) Open(context.Context, string) (bool, bool, error) {
	return g.waiting, len(g.d) > 0, nil
}

func (g *readyGate) RunEnded(context.Context, string, runs.Status) {}

func (g *readyGate) Claim(context.Context, string) ([]runs.ParkDelivery, error) {
	d := g.d
	g.d = nil
	return d, nil
}

func (g *readyGate) Settle(_ context.Context, _ string, _ []runs.ParkDelivery, res runs.ResumeResult) error {
	g.settled = append(g.settled, res)
	return nil
}

func (g *readyGate) Stale(context.Context) ([]string, error) { return nil, nil }

func (g *readyGate) PromptHint(context.Context, runs.LaunchOpts) string { return "" }

type resumeStub struct{ calls, fresh []runs.ResumeOpts }

func (r *resumeStub) Resume(_ context.Context, prevID string, o runs.ResumeOpts) (*runs.Run, error) {
	r.calls = append(r.calls, o)
	return &runs.Run{ID: prevID, TurnCount: 2}, nil
}

func (r *resumeStub) Fresh(_ context.Context, _ string, o runs.ResumeOpts) (*runs.Run, error) {
	r.fresh = append(r.fresh, o)
	return nil, errors.New("no fresh")
}

// TestParkerResumesOnce pins E.2: a run that waits for a request answer and
// an agent reply resumes once, after both, with both parts in time order.
func TestParkerResumesOnce(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	req := &readyGate{name: runs.GateRequest, d: []runs.ParkDelivery{{Gate: runs.GateRequest, Prompt: "answer", At: t0.Add(time.Minute), Reason: runs.ReasonRequest}}}
	ask := &readyGate{name: runs.GateAsk, waiting: true, d: []runs.ParkDelivery{{Gate: runs.GateAsk, Prompt: "reply", At: t0, Reason: runs.ReasonReply, HopID: "hop_1", TraceID: "T1", Path: []string{"a"}}}}
	rs := &resumeStub{}
	p := runs.NewParker(svc, rs, req, ask)
	svc.SetParker(p)
	ctx := context.Background()
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Harness: "claude", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(ctx, run.ID); got.Status != runs.StatusParked || got.EndReason != runs.EndRequest || len(rs.calls) != 0 {
		t.Fatalf("park: %s/%s calls %d", got.Status, got.EndReason, len(rs.calls))
	}
	ask.waiting = false
	if err := p.Wake(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.Wake(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if len(rs.calls) != 1 {
		t.Fatalf("resumes %d", len(rs.calls))
	}
	o := rs.calls[0]
	if o.Prompt != "reply\n\nanswer" || o.Reason != runs.ReasonReply || o.HopID != "hop_1" || o.TraceID != "T1" {
		t.Fatalf("resume %+v", o)
	}
	if len(req.settled) != 1 || len(ask.settled) != 1 || req.settled[0].RunID != run.ID {
		t.Fatalf("settled %v %v", req.settled, ask.settled)
	}
}

// errResumer fails every resume with err and counts the fresh runs.
type errResumer struct {
	err   error
	calls int
	fresh int
}

func (r *errResumer) Resume(context.Context, string, runs.ResumeOpts) (*runs.Run, error) {
	r.calls++
	return nil, r.err
}

func (r *errResumer) Fresh(context.Context, string, runs.ResumeOpts) (*runs.Run, error) {
	r.fresh++
	return nil, errors.New("no fresh")
}

// TestRestartGateResumesAtBoot: a run that a restart parked waits for the
// boot recovery, then resumes once as its next turn with the restart prompt.
func TestRestartGateResumesAtBoot(t *testing.T) {
	t.Parallel()
	svc, _, ctx := openTurnSvc(t)
	rs := &resumeStub{}
	p := runs.NewParker(svc, rs, runs.NewRestartGate(svc))
	svc.SetParker(p)
	run := restartParked(t, svc, "s1")
	if err := p.Wake(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if len(rs.calls) != 0 {
		t.Fatalf("a wake before the boot recovery resumed: %+v", rs.calls)
	}
	if err := p.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rs.calls) != 1 || rs.calls[0].Prompt != runs.RestartPrompt || rs.calls[0].Reason != runs.ReasonResume {
		t.Fatalf("resumes %+v", rs.calls)
	}
	if len(rs.fresh) != 0 {
		t.Fatalf("fresh runs %+v", rs.fresh)
	}
}

// TestRestartGateJoinsMessage: a message before the boot recovery resumes
// the run once, with the restart prompt first.
func TestRestartGateJoinsMessage(t *testing.T) {
	t.Parallel()
	svc, _, ctx := openTurnSvc(t)
	rs := &resumeStub{}
	svc.SetParker(runs.NewParker(svc, rs, runs.NewRestartGate(svc)))
	run := restartParked(t, svc, "s1")
	if _, err := svc.SendMessage(ctx, run.ID, "Also check the totals."); err != nil {
		t.Fatal(err)
	}
	want := runs.RestartPrompt + "\n\nAlso check the totals."
	if len(rs.calls) != 1 || rs.calls[0].Prompt != want || rs.calls[0].Reason != runs.ReasonMessage {
		t.Fatalf("resumes %+v", rs.calls)
	}
}

// TestRestartGateNoFresh: a run that cannot resume after a restart ends
// failed / session_gone with a reason, and never starts a fresh run.
func TestRestartGateNoFresh(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		session string
		err     error
		calls   int
	}{
		{name: "harness cannot resume", session: "s1", err: runs.ErrResumeUnsupported, calls: 1},
		{name: "no session", err: runs.ErrNoSession},
		{name: "launch error", session: "s1", err: errors.New("job file is gone"), calls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, _, ctx := openTurnSvc(t)
			rs := &errResumer{err: tc.err}
			p := runs.NewParker(svc, rs, runs.NewRestartGate(svc))
			svc.SetParker(p)
			run := restartParked(t, svc, tc.session)
			if err := p.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if rs.calls != tc.calls || rs.fresh != 0 {
				t.Fatalf("resumes %d fresh %d", rs.calls, rs.fresh)
			}
			got, err := svc.Get(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != runs.StatusFailed || got.EndReason != runs.EndSessionGone {
				t.Fatalf("status=%s end=%s", got.Status, got.EndReason)
			}
			tl, err := svc.Timeline(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, ev := range tl.Events {
				found = found || (ev.Kind == "error" && strings.Contains(string(ev.Detail), "could not continue after the restart"))
			}
			if !found {
				t.Fatal("no error event with the reason")
			}
		})
	}
}

// TestRunModel pins the run model: Create stores the launch model, Get and
// List return it before the first turn ends, an empty turn model keeps it,
// and a reported model replaces it.
func TestRunModel(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()

	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "a", Harness: "claude", Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Model != "opus" {
		t.Fatalf("created model = %q", run.Model)
	}
	bare, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "b", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	model := func(id string) string {
		t.Helper()
		got, err := svc.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Model != got.Tokens.Model {
			t.Fatalf("model %q and tokens.model %q differ", got.Model, got.Tokens.Model)
		}
		return got.Model
	}
	if got := model(run.ID); got != "opus" {
		t.Fatalf("live model = %q", got)
	}
	if got := model(bare.ID); got != "" {
		t.Fatalf("bare model = %q", got)
	}
	list, err := svc.List(ctx, runs.ListOpts{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list {
		if r.ID == run.ID && r.Model != "opus" {
			t.Fatalf("list model = %q", r.Model)
		}
	}

	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndSentinel, &pricing.TokenUsage{Input: 1}); err != nil {
		t.Fatal(err)
	}
	if got := model(run.ID); got != "opus" {
		t.Fatalf("empty turn model replaced the run model: %q", got)
	}
	if err := svc.Terminate(ctx, bare.ID, runs.StatusComplete, runs.EndSentinel,
		&pricing.TokenUsage{Input: 1, Model: "claude-opus-5-5"}); err != nil {
		t.Fatal(err)
	}
	if got := model(bare.ID); got != "claude-opus-5-5" {
		t.Fatalf("reported model = %q", got)
	}
}

// restartParked makes a run that a daemon restart parked.
func restartParked(t *testing.T, svc *runs.Service, session string) *runs.Run {
	t.Helper()
	ctx := context.Background()
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Harness: "claude", SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusParked, runs.EndRestart, nil); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runs.StatusParked || got.EndReason != runs.EndRestart {
		t.Fatalf("park: %s/%s", got.Status, got.EndReason)
	}
	return got
}

func BenchmarkActivityAgents(b *testing.B) {
	svc, ctx, from := benchStore(b)
	o := runs.ActivityOpts{From: from, To: from.Add(30 * 24 * time.Hour), Bin: 4 * time.Hour, Group: runs.ActivityByAgent}
	b.ResetTimer()
	for range b.N {
		if _, err := svc.Activity(ctx, o); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkActivityJobs(b *testing.B) {
	svc, ctx, from := benchStore(b)
	o := runs.ActivityOpts{From: from, To: from.Add(30 * 24 * time.Hour), Bin: 4 * time.Hour, Group: runs.ActivityByJob, Owner: "agt_00"}
	b.ResetTimer()
	for range b.N {
		if _, err := svc.Activity(ctx, o); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJobActivityRecent(b *testing.B) {
	svc, ctx, from := benchStore(b)
	b.ResetTimer()
	for range b.N {
		if _, err := svc.JobActivity(ctx, "ws", 14, from); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJobActivityOwners(b *testing.B) {
	svc, ctx, from := benchStore(b)
	owners := make([]string, 20)
	for i := range owners {
		owners[i] = fmt.Sprintf("agt_%02d", i)
	}
	b.ResetTimer()
	for range b.N {
		if _, err := svc.OwnerActivity(ctx, "ws", owners, 20, from); err != nil {
			b.Fatal(err)
		}
	}
}

// benchStore holds a synthetic store: 10,000 runs over 30 days, 20 agents,
// 150 jobs, and up to 3 turns per run. It returns the start of the window.
func benchStore(b *testing.B) (*runs.Service, context.Context, time.Time) {
	b.Helper()
	st, err := store.OpenPath(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = st.Close() })
	const nAgents, nJobs, nRuns = 20, 150, 10000
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	tx, err := st.DB().Begin()
	if err != nil {
		b.Fatal(err)
	}
	now := start.Format(time.RFC3339)
	for a := range nAgents {
		if _, err := tx.Exec(`INSERT INTO job_index (id, workspace_id, name, path, kind, owner_id, updated_at) VALUES (?,?,?,?,?,?,?)`,
			fmt.Sprintf("agt_%02d", a), "ws", fmt.Sprintf("a%02d", a), "/a", "agent", "", now); err != nil {
			b.Fatal(err)
		}
	}
	for j := range nJobs {
		if _, err := tx.Exec(`INSERT INTO job_index (id, workspace_id, name, path, kind, owner_id, updated_at) VALUES (?,?,?,?,?,?,?)`,
			fmt.Sprintf("jb_%03d", j), "ws", fmt.Sprintf("a%02d/j%03d", j%nAgents, j), "/j", "job", fmt.Sprintf("agt_%02d", j%nAgents), now); err != nil {
			b.Fatal(err)
		}
	}
	statuses := []string{"complete", "complete", "complete", "failed", "parked", "skipped"}
	for r := range nRuns {
		at := start.Add(time.Duration(r) * 30 * 24 * time.Hour / nRuns).Format(time.RFC3339)
		id := fmt.Sprintf("run_%06d", r)
		if _, err := tx.Exec(`INSERT INTO runs (id, workspace_id, job_id, status, started_at, trace_id, updated_at) VALUES (?,?,?,?,?,?,?)`,
			id, "ws", fmt.Sprintf("jb_%03d", r*7%nJobs), statuses[r%len(statuses)], at, id, at); err != nil {
			b.Fatal(err)
		}
		for n := 1; n <= 1+r%3; n++ {
			if _, err := tx.Exec(`INSERT INTO run_turns (run_id, n, reason, status, started_at, tokens_input, tokens_output, tokens_model) VALUES (?,?,?,?,?,?,?,?)`,
				id, n, "manual", "complete", at, 1000*n, 100*n, "default"); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return runs.New(st, nil, nil), context.Background(), start
}

func TestTerminateTurnWakesStream(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	wake := st.BusWake()
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndIdleTimeout, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("TerminateTurn did not wake the bus")
	}
	wake = st.BusWake()
	if _, err := svc.Mark(ctx, runs.MarkOpts{ID: run.ID, Status: runs.StatusComplete}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("Mark did not wake the bus")
	}
}

func TestWakeChatStyleLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		chat    *runs.WakeChat
		want    []string
		notWant []string
		exact   string
		runes   int // length of the room in the line; 0 skips the check
	}{
		{name: "nil", chat: nil, exact: runs.ChatStyleLine},
		{
			name: "slack room",
			chat: &runs.WakeChat{Connector: "Slack", Room: "#ops"},
			want: []string{"this Slack chat ('#ops')", "Do not post", runs.NoReplyToken, "_italic_"},
		},
		{
			name:    "empty room",
			chat:    &runs.WakeChat{Connector: "Slack"},
			want:    []string{"to this Slack chat as its own message"},
			notWant: []string{"('"},
		},
		{
			name: "empty connector",
			chat: &runs.WakeChat{Room: "ops"},
			want: []string{"to this chat ('ops')"},
		},
		{
			name:    "long room with a newline",
			chat:    &runs.WakeChat{Connector: "Chat", Room: "a\nb" + strings.Repeat("é", 150)},
			want:    []string{"('a b"},
			notWant: []string{"\n"},
			runes:   100,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.chat.StyleLine()
			if tc.exact != "" && got != tc.exact {
				t.Fatalf("got %q, want %q", got, tc.exact)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in %q", w, got)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in %q", w, got)
				}
			}
			if tc.runes > 0 {
				room := got[strings.Index(got, "('")+2 : strings.Index(got, "')")]
				if n := len([]rune(room)); n != tc.runes {
					t.Errorf("room has %d runes, want %d", n, tc.runes)
				}
			}
		})
	}
}
