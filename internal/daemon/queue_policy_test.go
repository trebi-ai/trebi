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
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/waittest"
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

	if name, ok := strings.CutPrefix(jobName, "ops/"); ok {
		writeOpsJob(t, filepath.Join(dir, "ws"), name, md)
	} else {
		writeFileAt(t, filepath.Join(dir, "ws", jobName, "JOB.md"), md)
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

// quietT is a test whose waits return on timeout and leave the check to the caller.
type quietT struct{ *testing.T }

func (quietT) Fatalf(string, ...any) {}

// waitQueue polls one queue row until want reports true. It returns the last
// values on timeout.
func waitQueue(t *testing.T, st *store.Store, id string, want func(status, errMsg, next string) bool) (string, string, string) {
	t.Helper()
	var status, errMsg, next string
	waittest.UntilWake(quietT{t}, st.BusWake, "the queue row", func() bool {
		_ = st.DB().QueryRow(`SELECT status, COALESCE(error_message,''), COALESCE(next_attempt_at,'') FROM event_queue WHERE id=?`, id).
			Scan(&status, &errMsg, &next)
		return want(status, errMsg, next)
	})
	return status, errMsg, next
}

// TestQueuePolicyDefersSecondFire proves the trigger policy is checked at the
// claim (plan E.2): the first fire launches, the second waits with a due time.
func TestQueuePolicyDefersSecondFire(t *testing.T) {
	cp, st, wsID, ctx := openQueuePlane(t, "ops/tickjob", "---\nharness: fake\n---\n\nGo.\n")
	routeOps(t, "tick", "rt_00000000e1", "tickjob", routing.GetsEveryMessage, job.TriggerYAML{Match: "true", Limit: "1/1h"})

	q := dispatch.NewQueue(st)
	first := &dispatch.QueueItem{Connector: "tick", WorkspaceID: wsID, JobID: indexID(t, st, wsID, "ops/tickjob"), JobName: "ops/tickjob", RouteID: "rt_00000000e1", Status: dispatch.QueuePending, EventRaw: `{"n":1}`}
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

	second := &dispatch.QueueItem{Connector: "tick", WorkspaceID: wsID, JobID: indexID(t, st, wsID, "ops/tickjob"), JobName: "ops/tickjob", RouteID: "rt_00000000e1", Status: dispatch.QueuePending, EventRaw: `{"n":2}`}
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
		t.Fatalf("next_attempt_at=%s is not a limit window of 1h", next)
	}
}

// TestQueueSkipsRowsNotDue proves ClaimNext and queueLanes ignore a row whose
// next_attempt_at is in the future.
func TestQueueSkipsRowsNotDue(t *testing.T) {
	md := "---\nharness: fake\n---\n\nGo.\n"
	cp, st, wsID, ctx := openQueuePlane(t, "ops/later", md)

	later := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	item := &dispatch.QueueItem{Connector: "x", WorkspaceID: wsID, JobID: indexID(t, st, wsID, "ops/later"), JobName: "ops/later", Status: dispatch.QueuePending, EventRaw: `{}`}
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
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM runs WHERE job_id=?`, indexID(t, st, wsID, "ops/later")).Scan(&runs)
	if runs != 0 {
		t.Fatalf("runs=%d; a not-due row must not launch", runs)
	}
}

// TestQueueSkipsArchivedJob: the launch gate skips a row of an archived job
// with the error archived and does not retry it.
func TestQueueSkipsArchivedJob(t *testing.T) {
	md := "---\nharness: fake\narchived_at: 2026-11-01T10:00:00Z\n---\n\nGo.\n"
	cp, st, wsID, ctx := openQueuePlane(t, "ops/old", md)

	item := &dispatch.QueueItem{Connector: "x", WorkspaceID: wsID, JobID: indexID(t, st, wsID, "ops/old"), JobName: "ops/old", Status: dispatch.QueuePending, EventRaw: `{}`}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	status, errMsg, _ := waitQueue(t, st, item.ID, func(s, _, _ string) bool { return s == "done" })
	var result string
	var attempts int
	_ = st.DB().QueryRow(`SELECT COALESCE(result,''), attempts FROM event_queue WHERE id=?`, item.ID).Scan(&result, &attempts)
	if status != "done" || result != string(dispatch.ResultSkipped) || errMsg != "archived" {
		t.Fatalf("status=%s result=%s error=%q want done/skipped/archived", status, result, errMsg)
	}
	if attempts > 1 {
		t.Fatalf("attempts=%d; an archived row must not retry", attempts)
	}
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM runs WHERE job_id=?`, item.JobID).Scan(&n)
	if n != 0 {
		t.Fatalf("runs=%d; an archived job must not launch", n)
	}
}

// TestQueueRetryBackoff proves an action error is final (P1-15): the row is
// dead with the error, not retried. TestAssistantWaitThenAskErrorRetries
// covers the backoff of a retryable error.
func TestQueueRetryBackoff(t *testing.T) {
	// A connector name that does not resolve fails before the run row exists.
	md := "---\nharness: claude\nconnectors: [ghost]\n---\n\nGo.\n"
	cp, st, wsID, ctx := openQueuePlane(t, "ops/badkit", md)
	t.Setenv("TREBI_FAKE_HARNESS", "")
	_ = os.Unsetenv("TREBI_FAKE_HARNESS")

	item := &dispatch.QueueItem{Connector: "x", WorkspaceID: wsID, JobID: indexID(t, st, wsID, "ops/badkit"), JobName: "ops/badkit", Status: dispatch.QueuePending, EventRaw: `{}`}
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
		Connector: conversations.ChannelAssistant, WorkspaceID: wsID, JobID: wsID, JobName: "asstlane",
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
// attempt (P1-9): 8 events under limit 1/1m all launch, one per window.
func TestQueueRateLimitLaunchesEveryEvent(t *testing.T) {
	md := "---\nharness: fake\nsession:\n  if_running: replace\n---\n\nGo.\n"
	clock := newTestClock()
	cp, st, wsID, ctx := openQueuePlaneAt(t, "ops/burst", md, clock.Now)
	routeOps(t, "tick", "rt_00000000e2", "burst", routing.GetsEveryMessage, job.TriggerYAML{Match: "true", Limit: "1/1m"})
	q := dispatch.NewQueue(st)
	for i := 0; i < 8; i++ {
		if err := q.Enqueue(ctx, &dispatch.QueueItem{Connector: "tick", WorkspaceID: wsID, JobID: indexID(t, st, wsID, "ops/burst"), JobName: "ops/burst", RouteID: "rt_00000000e2", EventRaw: `{}`}); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	launched := func() int { return queueCount(t, st, `status='done' AND result='launched'`) }
	for window := 1; window <= 8; window++ {
		waittest.UntilWake(quietT{t}, st.BusWake, "the launch of the window", func() bool { return launched() >= window })
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
		Connector: conversations.ChannelAssistant, WorkspaceID: wsID, JobID: wsID, JobName: "asstretry",
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
// not the connection of its route has no policy key (P1-12).
func TestEscalationItemTakesNoTriggerPolicy(t *testing.T) {
	md := "---\nharness: fake\nsession:\n  if_running: replace\n---\n\nGo.\n"
	cp, st, wsID, ctx := openQueuePlane(t, "ops/esc", md)
	routeOps(t, "tick", "rt_00000000e3", "esc", routing.GetsEveryMessage, job.TriggerYAML{Match: "true", Limit: "1/1h"})
	q := dispatch.NewQueue(st)
	first := &dispatch.QueueItem{Connector: "tick", WorkspaceID: wsID, JobID: indexID(t, st, wsID, "ops/esc"), JobName: "ops/esc", RouteID: "rt_00000000e3", EventRaw: `{}`}
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
	esc := &dispatch.QueueItem{Connector: "escalation", WorkspaceID: wsID, JobID: indexID(t, st, wsID, "ops/esc"), JobName: "ops/esc", EventRaw: `{}`}
	if err := q.Enqueue(ctx, esc); err != nil {
		t.Fatal(err)
	}
	status, errMsg, _ := waitQueue(t, st, esc.ID, func(s, _, _ string) bool { return s == "done" })
	if status != "done" || errMsg == "policy" {
		t.Fatalf("escalation status=%s error_message=%q; it must not wait on the trigger limit", status, errMsg)
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
		WorkspaceID: ws.ID, JobID: indexID(t, st, ws.ID, "ops"), JobName: "ops", Kind: runs.KindAgent, Harness: "fake",
		ConversationKey: "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	item := &dispatch.QueueItem{
		Connector: "x", WorkspaceID: ws.ID, JobID: indexID(t, st, ws.ID, "ops"), JobName: "ops", Kind: runs.KindAgent,
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

// TestJobCoalesceSendsMessage proves a channel event for a live job run
// reaches the run as the message, not as the job prompt again. A reaction
// quotes the message it reacts to.
func TestJobCoalesceSendsMessage(t *testing.T) {
	vars := map[string]string{"sender": "Ana", "text": "yes to J16", "room": "room-1"}
	react := map[string]string{"sender": "Ana", "text": "👍", "room": "room-1", "reply_to": "m1"}
	cases := []struct {
		name   string
		events []string // INSERT values: id, event_type, source_id, text, raw_json
		vars   map[string]string
		want   string
	}{
		{name: "message", vars: vars, want: "New message in this conversation from Ana:\n\nyes to J16"},
		{
			name:   "reaction with the text of the adapter",
			events: []string{`'ev-2','reaction','r1','👍','{"data":{"emoji":"👍","message_id":"m1","message_text":"Rate for J16\nis $85"}}'`},
			vars:   react, want: "Ana reacted 👍 to message m1:\n\n> Rate for J16\n> is $85",
		},
		{
			name: "reaction with a stored message",
			events: []string{
				`'ev-1','message','m1','Rate for J16',''`,
				`'ev-2','reaction','r1','👍','{"data":{"emoji":"👍","message_id":"m1"}}'`,
			},
			vars: react, want: "Ana reacted 👍 to message m1:\n\n> Rate for J16",
		},
		{
			name:   "reaction to an unknown message",
			events: []string{`'ev-2','reaction','r1','👍','{"data":{"emoji":"👍","message_id":"m1"}}'`},
			vars:   react, want: "Ana reacted 👍 to message m1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			home.SetBase(dir)
			t.Setenv("TREBI_HOME", dir)
			t.Setenv("TREBI_FAKE_HARNESS", "1")
			t.Cleanup(func() { home.SetBase("") })
			writeOpsJob(t, filepath.Join(dir, "ws"), "triage", "---\ntitle: Triage\nharness: fake\n---\n\nLong job prompt for {{sender}}.\n")
			st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			for _, v := range tc.events {
				if _, err := st.DB().Exec(`INSERT INTO events (id, event_type, source_id, text, raw_json, connector, instance, received_at, trace_id)
					VALUES (` + v + `, 'chat', 'chat', '2026-10-19T00:00:00Z', 'tr')`); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			cp := daemon.New(st, daemon.Options{QueueInterval: 30 * time.Millisecond})
			ws, err := cp.Workspaces.Add(ctx, filepath.Join(dir, "ws"), "ws")
			if err != nil {
				t.Fatal(err)
			}
			if err := cp.Workspaces.RebuildIndex(ctx, ws); err != nil {
				t.Fatal(err)
			}
			jobID := indexID(t, st, ws.ID, "ops/triage")
			live, err := cp.Runs.Create(ctx, runs.CreateOpts{
				WorkspaceID: ws.ID, JobID: jobID, JobName: "ops/triage", Kind: runs.KindJob, Harness: "fake",
				ConversationKey: "room-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			item := &dispatch.QueueItem{
				Connector: "chat", WorkspaceID: ws.ID, JobID: jobID, JobName: "ops/triage", Kind: runs.KindJob,
				ConversationKey: "room-1", EventID: "ev-2", Status: dispatch.QueuePending, Vars: tc.vars,
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
			if d["prompt_preview"] != tc.want {
				t.Fatalf("coalesce text=%q, want %q", d["prompt_preview"], tc.want)
			}
		})
	}
}

// TestAssistantTriggerPolicy proves assistant triggers take their limit
// (P1-10): the first item prompts the session, the second waits.
func TestAssistantTriggerPolicy(t *testing.T) {
	cp, st, wsID, ctx := openQueuePlane(t, "asstpol", "---\nharness: fake\n---\n\nGo.\n")
	ws, err := cp.Workspaces.Get(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "trebi.yaml"), []byte("assistant:\n  harness: fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	routeOps(t, "tick", "rt_00000000e4", routing.Assistant, routing.GetsEveryMessage, job.TriggerYAML{Limit: "1/1h"})
	q := dispatch.NewQueue(st)
	first := &dispatch.QueueItem{
		Connector: "tick", WorkspaceID: wsID, JobID: wsID, JobName: "assistant", Kind: dispatch.KindAssistant,
		EventID: "evt-1", RouteID: "rt_00000000e4", Prompt: "one", Status: dispatch.QueuePending,
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
		Connector: "tick", WorkspaceID: wsID, JobID: wsID, JobName: "assistant", Kind: dispatch.KindAssistant,
		EventID: "evt-2", RouteID: "rt_00000000e4", Prompt: "two", Status: dispatch.QueuePending,
	}
	if err := q.Enqueue(ctx, second); err != nil {
		t.Fatal(err)
	}
	status, errMsg, _ := waitQueue(t, st, second.ID, func(_, e, _ string) bool { return e != "" })
	if status != "pending" || errMsg != "policy" {
		t.Fatalf("second status=%s error_message=%q, want pending/policy", status, errMsg)
	}
}

// TestQueuePolicyMatchesTriggerBySlug proves a trigger that names the slug
// applies to an item that holds the connection id.
func TestQueuePolicyMatchesTriggerBySlug(t *testing.T) {
	cp, st, wsID, ctx := openQueuePlane(t, "ops/feedjob", "---\nharness: fake\n---\n\nGo.\n")
	routeOps(t, "feed", "rt_00000000e5", "feedjob", routing.GetsEveryMessage, job.TriggerYAML{Match: "true", Limit: "1/1h"})

	feed, err := cp.Connectors.Create(ctx, connectors.Connection{Type: "rss", Slug: "feed", From: connectors.FromInline, Manifest: &manifest.Manifest{
		Schema: manifest.SchemaID, Name: "rss", Title: "RSS", Description: "Test feed", Version: "1.0.0",
		Events: &manifest.Events{Protocol: manifest.ProtocolConnector, Command: "sleep 60", Types: []manifest.EventType{{Type: "item"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	q := dispatch.NewQueue(st)
	first := &dispatch.QueueItem{Connector: feed.ID, WorkspaceID: wsID, JobID: indexID(t, st, wsID, "ops/feedjob"), JobName: "ops/feedjob", RouteID: "rt_00000000e5", Status: dispatch.QueuePending, EventRaw: `{"n":1}`}
	if err := q.Enqueue(ctx, first); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	if status, _, _ := waitQueue(t, st, first.ID, func(s, _, _ string) bool { return s == "done" }); status != "done" {
		t.Fatalf("first fire status=%s want done", status)
	}
	second := &dispatch.QueueItem{Connector: feed.ID, WorkspaceID: wsID, JobID: indexID(t, st, wsID, "ops/feedjob"), JobName: "ops/feedjob", RouteID: "rt_00000000e5", Status: dispatch.QueuePending, EventRaw: `{"n":2}`}
	if err := q.Enqueue(ctx, second); err != nil {
		t.Fatal(err)
	}
	status, errMsg, _ := waitQueue(t, st, second.ID, func(s, _, next string) bool { return s == "pending" && next != "" })
	if status != "pending" || errMsg != "policy" {
		t.Fatalf("second fire status=%s error=%q want pending policy", status, errMsg)
	}
}

// TestTriggerFloodDrops proves a full lane drops new events: 100 events give
// 10 waiting rows, 90 dropped outcomes, and one trigger_flood inbox item.
func TestTriggerFloodDrops(t *testing.T) {
	cp, st, wsID, ctx := openQueuePlane(t, "ops/floodjob", "---\nharness: fake\n---\n\nGo.\n")
	routeOps(t, "feed", "rt_00000000e6", "floodjob", routing.GetsEveryMessage, job.TriggerYAML{Match: "true"})
	addFeed(t, cp)
	for i := 0; i < 100; i++ {
		if _, err := cp.Connectors.InjectEvent(ctx, "feed", map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	if n := queueCount(t, st, `job_id='`+indexID(t, st, wsID, "ops/floodjob")+`' AND status='pending'`); n != dispatch.LaneCap {
		t.Fatalf("pending=%d want %d", n, dispatch.LaneCap)
	}
	var dropped, flood int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM event_outcomes WHERE outcome=?`, string(dispatch.OutcomeDropped)).Scan(&dropped)
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM inbox_items WHERE alert_kind='trigger_flood' AND workspace_id=?`, wsID).Scan(&flood)
	if dropped != 100-dispatch.LaneCap {
		t.Fatalf("dropped=%d want %d", dropped, 100-dispatch.LaneCap)
	}
	if flood != 1 {
		t.Fatalf("trigger_flood items=%d want 1", flood)
	}
}

// TestManualLaunchWaitsForLiveRun proves waiting manual launches stay pending
// with reason run_active, do not count an attempt, and launch in id order
// after the live run ends.
func TestManualLaunchWaitsForLiveRun(t *testing.T) {
	clock := newTestClock()
	cp, st, wsID, ctx := openQueuePlaneAt(t, "ops/waiter", "---\nharness: fake\n---\n\nGo.\n", clock.Now)
	jobID := indexID(t, st, wsID, "ops/waiter")
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`INSERT INTO runs (id,workspace_id,job_id,kind,status,trace_id,started_at,updated_at)
		VALUES ('live-1',?,?,'job','running','trace-live',?,?)`, wsID, jobID, now, now); err != nil {
		t.Fatal(err)
	}

	q := dispatch.NewQueue(st)
	var ids []string
	for _, p := range []string{"one", "two"} {
		item := &dispatch.QueueItem{Connector: dispatch.ConnectorManual, WorkspaceID: wsID, JobID: jobID, JobName: "ops/waiter",
			Launch: &dispatch.LaunchRequest{Reason: string(runs.ReasonManual), Prompt: p}}
		if err := q.Enqueue(ctx, item); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, item.ID)
	}
	status, errMsg, _ := waitQueue(t, st, ids[0], func(_, e, _ string) bool { return e == string(dispatch.DeferRunActive) })
	if status != "pending" || errMsg != string(dispatch.DeferRunActive) {
		t.Fatalf("status=%s error_message=%q want pending/run_active", status, errMsg)
	}
	if n := queueCount(t, st, `job_id='`+jobID+`' AND attempts > 0`); n != 0 {
		t.Fatalf("%d rows with attempts; a run_active wait is not an attempt", n)
	}
	if _, err := st.DB().Exec(`UPDATE runs SET status='done', end_reason='complete' WHERE id='live-1'`); err != nil {
		t.Fatal(err)
	}

	waittest.Until(quietT{t}, "both manual launches", func() bool {
		if queueCount(t, st, `job_id='`+jobID+`' AND result='launched'`) >= 2 {
			return true
		}
		clock.Advance(3 * time.Second)
		time.Sleep(50 * time.Millisecond)
		return false
	})
	if n := queueCount(t, st, `job_id='`+jobID+`' AND result='launched'`); n != 2 {
		t.Fatalf("launched=%d want 2", n)
	}
	rows, err := st.DB().Query(`SELECT queue_id FROM runs WHERE job_id=? AND queue_id IS NOT NULL ORDER BY id`, jobID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Fatalf("launch order=%v want %v", got, ids)
	}
}

// TestQueueResumeWaitsForLiveTurn: a resume item for a run with a live turn
// waits without an attempt, then reopens the run as its next turn.
func TestQueueResumeWaitsForLiveTurn(t *testing.T) {
	clock := newTestClock()
	cp, st, wsID, ctx := openQueuePlaneAt(t, "ops/waiter", "---\nharness: fake\n---\n\nGo.\n", clock.Now)
	jobID := indexID(t, st, wsID, "ops/waiter")
	prev, err := cp.Runs.Create(ctx, runs.CreateOpts{
		WorkspaceID: wsID, JobID: jobID, JobName: "ops/waiter", Kind: runs.KindJob, Harness: "fake", SessionID: "sess-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	item := &dispatch.QueueItem{Connector: "retry", WorkspaceID: wsID, JobID: jobID, JobName: "ops/waiter", Kind: runs.KindJob,
		ResumeOf: prev.ID, Prompt: "resume", Status: dispatch.QueuePending}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()
	status, errMsg, _ := waitQueue(t, st, item.ID, func(_, e, _ string) bool { return e == string(dispatch.DeferRunActive) })
	if status != "pending" || errMsg != string(dispatch.DeferRunActive) {
		t.Fatalf("status=%s error_message=%q want pending/run_active", status, errMsg)
	}
	if n := queueCount(t, st, `id='`+item.ID+`' AND attempts > 0`); n != 0 {
		t.Fatal("a run_active wait is not an attempt")
	}
	if err := cp.Runs.Terminate(ctx, prev.ID, runs.StatusExited, runs.EndKill, nil); err != nil {
		t.Fatal(err)
	}
	waittest.Until(quietT{t}, "the launch of the waiting item", func() bool {
		if queueCount(t, st, `id='`+item.ID+`' AND result='launched'`) >= 1 {
			return true
		}
		clock.Advance(3 * time.Second)
		time.Sleep(50 * time.Millisecond)
		return false
	})
	turns, err := cp.Runs.Turns(ctx, prev.ID)
	if err != nil || len(turns) != 2 || turns[1].QueueID != item.ID || turns[1].Reason != runs.ReasonResume {
		t.Fatalf("turns %+v err %v", turns, err)
	}
	if n := queueCount(t, st, `result='launched' AND run_id='`+prev.ID+`'`); n != 1 {
		t.Fatalf("launched rows for the run: %d", n)
	}
}

// indexID reads the static ID of one indexed job or agent.
func indexID(t *testing.T, st *store.Store, wsID, name string) string {
	t.Helper()
	var id string
	if err := st.DB().QueryRow(`SELECT id FROM job_index WHERE workspace_id=? AND name=?`, wsID, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestBatchTrigger proves a batch trigger collects its events into one row
// that launches one run at the next tick, with the events block and file.
func TestBatchTrigger(t *testing.T) {
	cases := []struct {
		name, prompt string
		wantPrefix   string
	}{
		{"default block", "Summarize.", "Summarize.\n\nNew events (3):\n- "},
		{"events var", "Count {{event_count}}:\n{{events}}", "Count 3:\n- "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newTestClock()
			md := "---\nharness: fake\n---\n\n" + tc.prompt + "\n"
			cp, st, wsID, ctx := openQueuePlaneAt(t, "ops/digest", md, clock.Now)
			routeOps(t, "feed", "rt_00000000e7", "digest", routing.GetsEveryMessage, job.TriggerYAML{Batch: "10m"})
			addFeed(t, cp)
			jobID := indexID(t, st, wsID, "ops/digest")
			var eventIDs []string
			for i := range 3 {
				id, err := cp.Connectors.InjectEvent(ctx, "feed", map[string]any{"n": i})
				if err != nil {
					t.Fatal(err)
				}
				eventIDs = append(eventIDs, id)
			}
			var queueID, next string
			if err := st.DB().QueryRow(`SELECT id, next_attempt_at FROM event_queue WHERE job_id=?`, jobID).Scan(&queueID, &next); err != nil {
				t.Fatal(err)
			}
			if want := clock.Now().UTC().Truncate(time.Second).Add(10 * time.Minute).Format(time.RFC3339); next != want {
				t.Fatalf("next_attempt_at=%s want %s", next, want)
			}
			if n := queueCount(t, st, `job_id='`+jobID+`'`); n != 1 {
				t.Fatalf("queue rows=%d want 1", n)
			}
			var queued int
			_ = st.DB().QueryRow(`SELECT COUNT(*) FROM event_outcomes WHERE outcome=? AND queue_id=?`, string(dispatch.OutcomeQueued), queueID).Scan(&queued)
			if queued != 3 {
				t.Fatalf("queued outcomes on the row=%d want 3", queued)
			}

			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			cp.Start(runCtx)
			defer cp.Stop()
			time.Sleep(150 * time.Millisecond)
			if n := queueCount(t, st, `job_id='`+jobID+`' AND status='pending'`); n != 1 {
				t.Fatal("the row launched before its tick")
			}
			clock.Advance(10 * time.Minute)
			waitQueue(t, st, queueID, func(status, _, _ string) bool { return status == "done" })

			var runID string
			if err := st.DB().QueryRow(`SELECT run_id FROM event_queue WHERE id=? AND result='launched'`, queueID).Scan(&runID); err != nil {
				t.Fatalf("row not launched: %v", err)
			}
			run, err := cp.Runs.Get(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(run.PromptPreview, tc.wantPrefix) {
				t.Fatalf("prompt=%q want prefix %q", run.PromptPreview, tc.wantPrefix)
			}
			if run.ConversationKey != "" {
				t.Fatalf("conversation_key=%q; a batch run has none", run.ConversationKey)
			}
			b := run.Trigger.Batch
			if b == nil || b.Count != 3 || b.QueueID != queueID || strings.Join(b.EventIDs, ",") != strings.Join(eventIDs, ",") {
				t.Fatalf("trigger batch=%+v want 3 events %v", b, eventIDs)
			}
			file := filepath.Join(home.RunDir(runID), "events.json")
			if run.Trigger.Vars["events_file"] != file {
				t.Fatalf("events_file=%q want %q", run.Trigger.Vars["events_file"], file)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var events []dispatch.BatchEvent
			if err := json.Unmarshal(data, &events); err != nil || len(events) != 3 || events[0].EventID != eventIDs[0] {
				t.Fatalf("events.json=%s err=%v", data, err)
			}
		})
	}
}

// TestBatchJoinsWaitingRow proves an event joins a due row that waits for a
// live run of its job, so the run that follows gets every event.
func TestBatchJoinsWaitingRow(t *testing.T) {
	clock := newTestClock()
	cp, st, wsID, ctx := openQueuePlaneAt(t, "ops/waits", "---\nharness: fake\n---\n\nGo.\n", clock.Now)
	routeOps(t, "feed", "rt_00000000e8", "waits", routing.GetsEveryMessage, job.TriggerYAML{Batch: "1m"})
	addFeed(t, cp)
	jobID := indexID(t, st, wsID, "ops/waits")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`INSERT INTO runs (id,workspace_id,job_id,kind,status,trace_id,started_at,updated_at)
		VALUES ('live-1',?,?,'job','running','trace-live',?,?)`, wsID, jobID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.Connectors.InjectEvent(ctx, "feed", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	var queueID string
	if err := st.DB().QueryRow(`SELECT id FROM event_queue WHERE job_id=?`, jobID).Scan(&queueID); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()
	clock.Advance(time.Minute)
	waitQueue(t, st, queueID, func(_, e, _ string) bool { return e == string(dispatch.DeferRunActive) })
	if _, err := cp.Connectors.InjectEvent(ctx, "feed", map[string]any{"n": 2}); err != nil {
		t.Fatal(err)
	}
	if n := queueCount(t, st, `job_id='`+jobID+`'`); n != 1 {
		t.Fatalf("queue rows=%d; the event must join the waiting row", n)
	}
	if _, err := st.DB().Exec(`UPDATE runs SET status='done', end_reason='complete' WHERE id='live-1'`); err != nil {
		t.Fatal(err)
	}
	waittest.Until(quietT{t}, "the launch of the joined row", func() bool {
		if queueCount(t, st, `id='`+queueID+`' AND result='launched'`) > 0 {
			return true
		}
		clock.Advance(3 * time.Second)
		time.Sleep(50 * time.Millisecond)
		return false
	})
	var runID string
	if err := st.DB().QueryRow(`SELECT run_id FROM event_queue WHERE id=? AND result='launched'`, queueID).Scan(&runID); err != nil {
		t.Fatalf("row not launched: %v", err)
	}
	run, err := cp.Runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Trigger.Batch == nil || run.Trigger.Batch.Count != 2 {
		t.Fatalf("trigger batch=%+v want 2 events", run.Trigger.Batch)
	}
}

// addFeed adds the inline connection "feed" that emits JSON lines.
func addFeed(t *testing.T, cp *daemon.ControlPlane) {
	t.Helper()
	if _, err := cp.Connectors.Create(context.Background(), connectors.Connection{Type: "feed", Slug: "feed", From: connectors.FromInline, Manifest: &manifest.Manifest{
		Schema: manifest.SchemaID, Name: "feed", Title: "Feed", Description: "Test feed", Version: "1.0.0",
		Events: &manifest.Events{Command: "sleep 60", Format: manifest.FormatJSON},
	}}); err != nil {
		t.Fatal(err)
	}
}

// writeOpsJob writes the job agents/ops/<name> and the AGENT.md of ops.
// It returns the job folder.
func writeOpsJob(t *testing.T, wsPath, name, md string) string {
	t.Helper()
	dir := opsJobDir(t, wsPath, name)
	writeFileAt(t, filepath.Join(dir, "JOB.md"), md)
	return dir
}

// opsJobDir writes the AGENT.md of ops and returns the folder of the job
// ops/<name>. It does not make the folder.
func opsJobDir(t *testing.T, wsPath, name string) string {
	t.Helper()
	agent := filepath.Join(wsPath, "agents", "ops", "AGENT.md")
	if _, err := os.Stat(agent); err != nil {
		writeFileAt(t, agent, "---\ndescription: Test agent.\nharness: fake\n---\n\nRun ops.\n")
	}
	return filepath.Join(wsPath, "agents", "ops", name)
}

// routeOps writes routes/<conn>.yaml in the test workspace with one route to
// the job ops/<name>, or to the assistant when name is "assistant".
func routeOps(t *testing.T, conn, id, name, gets string, f job.TriggerYAML) {
	t.Helper()
	r := routing.Route{ID: id, Job: "ops/" + name, Gets: gets, Filter: f}
	if name == routing.Assistant {
		r = routing.Route{ID: id, Assistant: true, Gets: gets, Filter: f}
	}
	routingtest.Write(t, filepath.Join(home.Base(), "ws"), conn, routing.File{Routes: []routing.Route{r}})
}

// writeFileAt writes one file and makes its folder.
func writeFileAt(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
