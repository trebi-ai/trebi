package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// openQueuePlane builds a daemon over one temp home and registers one job.
func openQueuePlane(t *testing.T, jobName, md string) (*daemon.ControlPlane, *store.Store, string, context.Context) {
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

	cp := daemon.New(st, daemon.Options{QueueInterval: 30 * time.Millisecond})
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

// TestQueueRetryBackoff proves a launch error returns the row to pending with
// a backoff instead of an immediate requeue.
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

	status, errMsg, next := waitQueue(t, st, item.ID, func(s, _, next string) bool { return next != "" })
	if status != "pending" || errMsg != "retry" {
		t.Fatalf("status=%s error_message=%q want pending/retry", status, errMsg)
	}
	due, err := time.Parse(time.RFC3339, next)
	if err != nil {
		t.Fatalf("next_attempt_at=%q: %v", next, err)
	}
	if d := time.Until(due); d < 10*time.Second || d > 25*time.Second {
		t.Fatalf("backoff=%s, want about 15s", d)
	}
}

// TestAssistantTurnWaits proves a second assistant item for a conversation
// with a live turn goes back to pending with reason turn_active (plan E.4).
func TestAssistantTurnWaits(t *testing.T) {
	md := "---\nharness: fake\n---\n\nGo.\n"
	cp, st, wsID, ctx := openQueuePlane(t, "asstlane", md)

	convID, err := cp.Conversations.EnsureForChannel(ctx, wsID, "", conversations.ChannelAssistant, "ref-wait")
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
