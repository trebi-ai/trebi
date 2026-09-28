package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// testClock is a manual queue and policy clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Now()} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// openQueuePlane builds a daemon over one temp home and registers one job.
func openQueuePlane(t *testing.T, jobName, md string) (*daemon.ControlPlane, *store.Store, string, context.Context) {
	t.Helper()
	return openQueuePlaneAt(t, jobName, md, nil)
}

// openQueuePlaneAt is openQueuePlane with a queue and policy clock.
func openQueuePlaneAt(t *testing.T, jobName, md string, now func() time.Time) (*daemon.ControlPlane, *store.Store, string, context.Context) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	jobDir := filepath.Join(dir, "ws", jobName)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cp := daemon.New(st, daemon.Options{QueueInterval: 30 * time.Millisecond, Now: now})
	ws, err := cp.Workspaces.Add(context.Background(), filepath.Join(dir, "ws"), "ws")
	if err != nil {
		t.Fatal(err)
	}
	_ = cp.Workspaces.RebuildIndex(context.Background(), ws)
	return cp, st, ws.ID, context.Background()
}

// waitQueue polls one queue row until want reports true.
func waitQueue(t *testing.T, st *store.Store, id string, want func(status, errMsg, next string) bool) (string, string, string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var status, errMsg, next string
	for time.Now().Before(deadline) {
		_ = st.DB().QueryRow(`SELECT status, COALESCE(error_message,''), COALESCE(next_attempt_at,'') FROM event_queue WHERE id=?`, id).
			Scan(&status, &errMsg, &next)
		if want(status, errMsg, next) {
			return status, errMsg, next
		}
		time.Sleep(20 * time.Millisecond)
	}
	return status, errMsg, next
}

// TestQueuePolicyDefersSecondFire proves the trigger policy is checked at the
// claim (plan E.2): the first fire launches, the second waits with a due time.
func TestQueuePolicyDefersSecondFire(t *testing.T) {
	md := "---\nharness: fake\ntriggers:\n  - event: tick\n    match: \"true\"\n    debounce: 1h\n---\n\nGo.\n"
	cp, st, wsID, ctx := openQueuePlane(t, "tickjob", md)

	q := dispatch.NewQueue(st)
	first := &dispatch.QueueItem{Connector: "tick", WorkspaceID: wsID, Job: "tickjob", Status: dispatch.QueuePending, EventRaw: `{"n":1}`}
	if err := q.Enqueue(ctx, first); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	status, _, _ := waitQueue(t, st, first.ID, func(s, _, _ string) bool { return s == "done" })
	if status != "done" {
		t.Fatalf("first fire status=%s want done", status)
	}

	second := &dispatch.QueueItem{Connector: "tick", WorkspaceID: wsID, Job: "tickjob", Status: dispatch.QueuePending, EventRaw: `{"n":2}`}
	if err := q.Enqueue(ctx, second); err != nil {
		t.Fatal(err)
	}
	status, errMsg, next := waitQueue(t, st, second.ID, func(s, _, next string) bool { return s == "pending" && next != "" })
	if status != "pending" {
		t.Fatalf("second fire status=%s want pending", status)
	}
	if errMsg != "policy" {
		t.Fatalf("error_message=%q want policy", errMsg)
	}
	due, err := time.Parse(time.RFC3339, next)
	if err != nil {
		t.Fatalf("next_attempt_at=%q: %v", next, err)
	}
	if due.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("next_attempt_at=%s is not a debounce of 1h", next)
	}
}

// TestQueueSkipsRowsNotDue proves ClaimNext and queueLanes ignore a row whose
// next_attempt_at is in the future.
func TestQueueSkipsRowsNotDue(t *testing.T) {
	md := "---\nharness: fake\n---\n\nGo.\n"
	cp, st, wsID, ctx := openQueuePlane(t, "later", md)

	later := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	item := &dispatch.QueueItem{Connector: "x", WorkspaceID: wsID, Job: "later", Status: dispatch.QueuePending, EventRaw: `{}`}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE event_queue SET next_attempt_at=?, error_message='policy' WHERE id=?`, later, item.ID); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()
	time.Sleep(300 * time.Millisecond)

	var status string
	_ = st.DB().QueryRow(`SELECT status FROM event_queue WHERE id=?`, item.ID).Scan(&status)
	if status != "pending" {
		t.Fatalf("status=%s want pending", status)
	}
	var runs int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM runs WHERE job='later'`).Scan(&runs)
	if runs != 0 {
		t.Fatalf("runs=%d; a not-due row must not launch", runs)
	}
}

// TestQueueRetryBackoff proves an action error is final (P1-15): the row is
// dead with the error, not retried. TestAssistantWaitThenAskErrorRetries
// covers the backoff of a retryable error.
func TestQueueRetryBackoff(t *testing.T) {
	// A connector name that does not resolve fails before the run row exists.
	md := "---\nharness: claude\nconnectors: [ghost]\n---\n\nGo.\n"
	cp, st, wsID, ctx := openQueuePlane(t, "badkit", md)
	t.Setenv("TREBI_FAKE_HARNESS", "")
	_ = os.Unsetenv("TREBI_FAKE_HARNESS")

	item := &dispatch.QueueItem{Connector: "x", WorkspaceID: wsID, Job: "badkit", Status: dispatch.QueuePending, EventRaw: `{}`}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	status, errMsg, _ := waitQueue(t, st, item.ID, func(s, _, _ string) bool { return s == "done" })
	var result string
	_ = st.DB().QueryRow(`SELECT COALESCE(result,'') FROM event_queue WHERE id=?`, item.ID).Scan(&result)
	if status != "done" || result != string(dispatch.ResultDead) {
		t.Fatalf("status=%s result=%s want done/dead", status, result)
	}
	if !strings.Contains(errMsg, "ghost") {
		t.Fatalf("error_message=%q, want the action error", errMsg)
	}
}

// TestAssistantTurnWaits proves a second assistant item for a conversation
// with a live turn goes back to pending with reason turn_active (plan E.4).
func TestAssistantTurnWaits(t *testing.T) {
	md := "---\nharness: fake\n---\n\nGo.\n"
	cp, st, wsID, ctx := openQueuePlane(t, "asstlane", md)

	convID, err := cp.Conversations.EnsureForChannel(ctx, wsID, "", conversations.Address{Connector: conversations.ChannelAssistant, Room: "ref-wait"})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()
	// Start closes rows left running by a previous process, so the live row is
	// written after it.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`INSERT INTO assistant_sessions
		(conversation_id, workspace_id, harness, mode, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`, convID, wsID, "fake", "auto", "running", now, now); err != nil {
		t.Fatal(err)
	}
	if !cp.Assistant().TurnActive(ctx, convID) {
		t.Fatalf("TurnActive(%s) is false right after a running insert", convID)
	}

	item := &dispatch.QueueItem{
		Connector: conversations.ChannelAssistant, WorkspaceID: wsID, Job: "asstlane",
		Kind: dispatch.KindAssistant, ConversationKey: "ref-wait",
		Prompt: "hello", Status: dispatch.QueuePending,
	}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}

	status, errMsg, next := waitQueue(t, st, item.ID, func(s, _, next string) bool { return next != "" })
	if status != "pending" || errMsg != "turn_active" {
		t.Fatalf("status=%s error_message=%q want pending/turn_active", status, errMsg)
	}
	if due, err := time.Parse(time.RFC3339, next); err != nil || time.Until(due) > 10*time.Second {
		t.Fatalf("next_attempt_at=%q err=%v; want about 2s", next, err)
	}
}

// queueCount counts rows that match one WHERE clause.
func queueCount(t *testing.T, st *store.Store, where string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE ` + where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestQueueRateLimitLaunchesEveryEvent proves a policy wait is not an
// attempt (P1-9): 8 events under rate_limit 1/1m all launch, one per window.
func TestQueueRateLimitLaunchesEveryEvent(t *testing.T) {
	md := "---\nharness: fake\nsession:\n  if_running: allow\ntriggers:\n  - event: tick\n    match: \"true\"\n    rate_limit: 1/1m\n---\n\nGo.\n"
	clock := newTestClock()
	cp, st, wsID, ctx := openQueuePlaneAt(t, "burst", md, clock.Now)
	q := dispatch.NewQueue(st)
	for i := 0; i < 8; i++ {
		if err := q.Enqueue(ctx, &dispatch.QueueItem{Connector: "tick", WorkspaceID: wsID, Job: "burst", EventRaw: `{}`}); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	launched := func() int { return queueCount(t, st, `status='done' AND result='launched'`) }
	for window := 1; window <= 8; window++ {
		deadline := time.Now().Add(5 * time.Second)
		for launched() < window && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if n := launched(); n != window {
			t.Fatalf("window %d: launched=%d dead=%d", window, n, queueCount(t, st, `result='dead'`))
		}
		time.Sleep(200 * time.Millisecond)
		if n := launched(); n != window {
			t.Fatalf("window %d: launched=%d, want one launch per window", window, n)
		}
		clock.Advance(61 * time.Second)
	}
	if n := queueCount(t, st, `attempts != 1`); n != 0 {
		t.Fatalf("%d rows with attempts != 1; a wait must not count", n)
	}
}

// TestAssistantWaitThenAskErrorRetries proves a turn wait is not an attempt
// (P1-9): an item that waits 30 s for a turn and then gets one Ask error is
// retried with a backoff.
func TestAssistantWaitThenAskErrorRetries(t *testing.T) {
	clock := newTestClock()
	cp, st, wsID, ctx := openQueuePlaneAt(t, "asstretry", "---\nharness: fake\n---\n\nGo.\n", clock.Now)
	// A disabled assistant still resolves the fake harness, so the health
	// gate passes and Ask fails.
	fake, disabled := "fake", true
	if _, err := cp.Workspaces.SetAssistantOverride(ctx, wsID,
		assistant.Override{Harness: &fake, Disabled: &disabled}, ""); err != nil {
		t.Fatal(err)
	}
	convID, err := cp.Conversations.EnsureForChannel(ctx, wsID, "", conversations.Address{Connector: conversations.ChannelAssistant, Room: "ref-retry"})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`INSERT INTO assistant_sessions
		(conversation_id, workspace_id, harness, mode, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`, convID, wsID, "fake", "auto", "running", now, now); err != nil {
		t.Fatal(err)
	}
	item := &dispatch.QueueItem{
		Connector: conversations.ChannelAssistant, WorkspaceID: wsID, Job: "asstretry",
		Kind: dispatch.KindAssistant, ConversationKey: "ref-retry",
		Prompt: "hello", Status: dispatch.QueuePending,
	}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	// Ten turn waits of about 3 s each.
	for i := 0; i < 10; i++ {
		_, errMsg, next := waitQueue(t, st, item.ID, func(s, e, next string) bool {
			due, err := time.Parse(time.RFC3339, next)
			return s == "pending" && e == "turn_active" && err == nil && due.After(clock.Now())
		})
		if errMsg != "turn_active" {
			t.Fatalf("wait %d: error_message=%q next=%q", i, errMsg, next)
		}
		clock.Advance(3 * time.Second)
	}
	if _, err := st.DB().Exec(`UPDATE assistant_sessions SET status='idle' WHERE conversation_id=?`, convID); err != nil {
		t.Fatal(err)
	}
	clock.Advance(3 * time.Second)
	status, errMsg, next := waitQueue(t, st, item.ID, func(_, e, _ string) bool { return e == "retry" })
	if status != "pending" || errMsg != "retry" {
		t.Fatalf("status=%s error_message=%q, want pending/retry", status, errMsg)
	}
	if n := queueCount(t, st, `attempts=1`); n != 1 {
		t.Fatalf("attempts != 1 after one Ask error")
	}
	due, err := time.Parse(time.RFC3339, next)
	if err != nil {
		t.Fatalf("next_attempt_at=%q: %v", next, err)
	}
	if d := due.Sub(clock.Now()); d < 10*time.Second || d > 25*time.Second {
		t.Fatalf("backoff=%s, want about 15s", d)
	}
}

// TestEscalationItemTakesNoTriggerPolicy proves an item whose connector is
// not the event of trigger OnIndex has no policy key (P1-12).
func TestEscalationItemTakesNoTriggerPolicy(t *testing.T) {
	md := "---\nharness: fake\nsession:\n  if_running: allow\ntriggers:\n  - event: tick\n    match: \"true\"\n    debounce: 1h\n---\n\nGo.\n"
	cp, st, wsID, ctx := openQueuePlane(t, "esc", md)
	q := dispatch.NewQueue(st)
	first := &dispatch.QueueItem{Connector: "tick", WorkspaceID: wsID, Job: "esc", EventRaw: `{}`}
	if err := q.Enqueue(ctx, first); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()
	if status, _, _ := waitQueue(t, st, first.ID, func(s, _, _ string) bool { return s == "done" }); status != "done" {
		t.Fatalf("first status=%s", status)
	}
	esc := &dispatch.QueueItem{Connector: "escalation", WorkspaceID: wsID, Job: "esc", EventRaw: `{}`}
	if err := q.Enqueue(ctx, esc); err != nil {
		t.Fatal(err)
	}
	status, errMsg, _ := waitQueue(t, st, esc.ID, func(s, _, _ string) bool { return s == "done" })
	if status != "done" || errMsg == "policy" {
		t.Fatalf("escalation status=%s error_message=%q; it must not wait on the trigger debounce", status, errMsg)
	}
}

// TestAgentCoalesceKeepsMessage proves a second chat message while the
// agent run is live reaches the run with its text (P1-13).
func TestAgentCoalesceKeepsMessage(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })
	agentDir := filepath.Join(dir, "ws", "agents", "ops")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"),
		[]byte("---\ntitle: Ops\nharness: fake\n---\n\nAnswer the message.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	cp := daemon.New(st, daemon.Options{QueueInterval: 30 * time.Millisecond})
	ws, err := cp.Workspaces.Add(ctx, filepath.Join(dir, "ws"), "ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Workspaces.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	live, err := cp.Runs.Create(ctx, runs.CreateOpts{
		WorkspaceID: ws.ID, Job: "agents/ops", Kind: runs.KindAgent, Harness: "fake",
		ConversationKey: "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	item := &dispatch.QueueItem{
		Connector: "x", WorkspaceID: ws.ID, Job: "agents/ops", Kind: runs.KindAgent,
		ConversationKey: "u1", Prompt: "second message", Status: dispatch.QueuePending,
	}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()
	if status, _, _ := waitQueue(t, st, item.ID, func(s, _, _ string) bool { return s == "done" }); status != "done" {
		t.Fatalf("item status=%s want done", status)
	}
	var detail string
	if err := st.DB().QueryRow(`SELECT COALESCE(detail_json,'') FROM run_events WHERE run_id=? AND kind='coalesce'`, live.ID).
		Scan(&detail); err != nil {
		t.Fatalf("coalesce event: %v", err)
	}
	var d map[string]string
	_ = json.Unmarshal([]byte(detail), &d)
	if d["prompt_preview"] != "second message" {
		t.Fatalf("coalesce text=%q, want the message", d["prompt_preview"])
	}
}

// TestAssistantTriggerPolicy proves assistant triggers take their debounce
// (P1-10): the first item prompts the session, the second waits.
func TestAssistantTriggerPolicy(t *testing.T) {
	cp, st, wsID, ctx := openQueuePlane(t, "asstpol", "---\nharness: fake\n---\n\nGo.\n")
	ws, err := cp.Workspaces.Get(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	yml := "assistant:\n  harness: fake\n  triggers:\n    - event: tick\n      debounce: 1h\n"
	if err := os.WriteFile(filepath.Join(ws.Path, "trebi.yaml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	q := dispatch.NewQueue(st)
	first := &dispatch.QueueItem{
		Connector: "tick", WorkspaceID: wsID, Job: "assistant", Kind: dispatch.KindAssistant,
		EventID: "evt-1", Prompt: "one", Status: dispatch.QueuePending,
	}
	if err := q.Enqueue(ctx, first); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()
	if status, _, _ := waitQueue(t, st, first.ID, func(s, _, _ string) bool { return s == "done" }); status != "done" {
		t.Fatalf("first status=%s want done", status)
	}
	second := &dispatch.QueueItem{
		Connector: "tick", WorkspaceID: wsID, Job: "assistant", Kind: dispatch.KindAssistant,
		EventID: "evt-2", Prompt: "two", Status: dispatch.QueuePending,
	}
	if err := q.Enqueue(ctx, second); err != nil {
		t.Fatal(err)
	}
	status, errMsg, _ := waitQueue(t, st, second.ID, func(_, e, _ string) bool { return e != "" })
	if status != "pending" || errMsg != "policy" {
		t.Fatalf("second status=%s error_message=%q, want pending/policy", status, errMsg)
	}
}
