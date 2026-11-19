package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"encoding/json"

	"github.com/oklog/ulid/v2"

	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/waittest"
)

type escalationFixture struct {
	cp      *daemon.ControlPlane
	st      *store.Store
	wsID    string
	jobID   string
	ownerID string
}

func newEscalationFixture(t *testing.T) *escalationFixture {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	wsPath := filepath.Join(dir, "ws")
	agentDir := filepath.Join(wsPath, "agents", "keeper")
	jobDir := filepath.Join(agentDir, "build")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(agentDir, "AGENT.md"): "---\ndescription: Test agent.\nharness: fake\n---\n\nKeep the repository healthy.\n",
		filepath.Join(jobDir, "JOB.md"):     "---\nharness: fake\n---\n\nBuild the repository.\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cp := daemon.New(st, daemon.Options{})
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "escalation")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Workspaces.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	f := &escalationFixture{cp: cp, st: st, wsID: ws.ID}
	for name, id := range map[string]*string{"keeper/build": &f.jobID, "keeper": &f.ownerID} {
		if err := st.DB().QueryRow(`SELECT id FROM job_index WHERE workspace_id=? AND name=?`, ws.ID, name).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func insertRunFailure(t *testing.T, f *escalationFixture, id, kind string, seq int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := f.st.DB().Exec(`INSERT INTO runs
		(id,workspace_id,job_id,kind,status,end_reason,trace_id,started_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		id, f.wsID, f.jobID, kind, "failed", "failed", ulid.Make().String(), now, now); err != nil {
		t.Fatal(err)
	}
	attrs := map[string]string{
		"workspace": f.wsID, "job_id": f.jobID, "job_name": "keeper/build", "kind": kind,
		"owner_id": f.ownerID, "end_reason": "failed", "status": "failed",
	}
	raw, err := json.Marshal(attrs)
	if err != nil {
		t.Fatal(err)
	}
	f.cp.Alerts.HandleEvent(context.Background(), store.StreamEvent{Seq: seq, Topic: "run.failed", ID: id, Attrs: raw})
}

func TestOwnedJobFailureEscalatesThenRaises(t *testing.T) {
	ctx := context.Background()
	f := newEscalationFixture(t)
	for i := 0; i < 2; i++ {
		insertRunFailure(t, f, "job-fail-"+string(rune('a'+i)), "job", int64(i+1))
	}
	var queueCount, failureCount int
	if err := f.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM event_queue WHERE connector='escalation'`).Scan(&queueCount); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_items WHERE alert_kind='run_failed'`).Scan(&failureCount); err != nil {
		t.Fatal(err)
	}
	if queueCount != 2 || failureCount != 0 {
		t.Fatalf("escalations=%d failures=%d want 2/0", queueCount, failureCount)
	}

	insertRunFailure(t, f, "job-fail-fourth", "job", int64(4))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	f.cp.Start(runCtx)
	defer f.cp.Stop()
	if err := f.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM event_queue WHERE connector='escalation'`).Scan(&queueCount); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_items WHERE alert_kind='run_failed' AND run_id='job-fail-fourth'`).Scan(&failureCount); err != nil {
		t.Fatal(err)
	}
	if queueCount != 2 || failureCount != 1 {
		t.Fatalf("after third escalations=%d failures=%d want 2/1", queueCount, failureCount)
	}
	var reason string
	waittest.Until(t, "the owner escalation run to launch", func() bool {
		err := f.st.DB().QueryRowContext(ctx, `SELECT reason FROM runs WHERE job_id=? AND kind='agent'`, f.ownerID).Scan(&reason)
		return err == nil
	})
	if reason != "escalation" {
		t.Fatalf("owner wake reason=%q", reason)
	}
}

func TestAgentFailureRaisesDirectly(t *testing.T) {
	ctx := context.Background()
	f := newEscalationFixture(t)
	insertRunFailure(t, f, "agent-fail", "agent", int64(1))
	var queueCount, failureCount int
	if err := f.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM event_queue WHERE connector='escalation'`).Scan(&queueCount); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_items WHERE alert_kind='run_failed' AND run_id='agent-fail'`).Scan(&failureCount); err != nil {
		t.Fatal(err)
	}
	if queueCount != 0 || failureCount != 1 {
		t.Fatalf("queue=%d failures=%d want 0/1", queueCount, failureCount)
	}
}

// A daily job fails once a day, so a 24-hour window never fills. The limit
// counts the hand-offs since the last complete run, and a complete run
// starts the count again.
func TestDailyJobFailureRaisesAfterTwoHandoffs(t *testing.T) {
	ctx := context.Background()
	f := newEscalationFixture(t)
	day := func(n int) string {
		return time.Now().UTC().Add(time.Duration(-n) * 24 * time.Hour).Format(time.RFC3339Nano)
	}
	if _, err := f.st.DB().Exec(`INSERT INTO runs (id,workspace_id,job_id,kind,reason,status,end_reason,trace_id,started_at,ended_at,updated_at)
		VALUES ('job-ok','`+f.wsID+`','`+f.jobID+`','job','schedule','complete','sentinel','t',?,?,?)`, day(4), day(4), day(4)); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := f.st.DB().QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for i, id := range []string{"day-1", "day-2"} {
		insertRunFailure(t, f, id, "job", int64(i+1))
		// The agent marks the hand-off done, and the job fails the next day.
		if _, err := f.st.DB().Exec(`UPDATE event_queue SET status='done', created_at=? WHERE event_id=?`,
			day(3-i), "escalation:"+f.jobID+":"+id); err != nil {
			t.Fatal(err)
		}
	}
	insertRunFailure(t, f, "day-3", "job", 3)
	if n := count(`SELECT COUNT(*) FROM inbox_items WHERE alert_kind='run_failed' AND run_id='day-3'`); n != 1 {
		t.Fatalf("third daily failure: run_failed=%d want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM event_queue WHERE connector='escalation'`); n != 2 {
		t.Fatalf("hand-offs=%d want 2", n)
	}

	if _, err := f.st.DB().Exec(`INSERT INTO runs (id,workspace_id,job_id,kind,reason,status,end_reason,trace_id,started_at,ended_at,updated_at)
		VALUES ('job-ok-2','`+f.wsID+`','`+f.jobID+`','job','manual','complete','sentinel','t',?,?,?)`,
		time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	insertRunFailure(t, f, "day-4", "job", 4)
	if n := count(`SELECT COUNT(*) FROM event_queue WHERE event_id=?`, "escalation:"+f.jobID+":day-4"); n != 1 {
		t.Fatalf("after a complete run: hand-off=%d want 1", n)
	}
}
