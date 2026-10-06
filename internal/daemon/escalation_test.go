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
	for name, id := range map[string]*string{"agents/keeper/build": &f.jobID, "agents/keeper": &f.ownerID} {
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
		"workspace": f.wsID, "job_id": f.jobID, "job_name": "agents/keeper/build", "kind": kind,
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
	for i := 0; i < 3; i++ {
		insertRunFailure(t, f, "job-fail-"+string(rune('a'+i)), "job", int64(i+1))
	}
	var queueCount, failureCount int
	if err := f.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM event_queue WHERE connector='escalation'`).Scan(&queueCount); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_items WHERE alert_kind='run_failed'`).Scan(&failureCount); err != nil {
		t.Fatal(err)
	}
	if queueCount != 3 || failureCount != 0 {
		t.Fatalf("escalations=%d failures=%d want 3/0", queueCount, failureCount)
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
	if queueCount != 3 || failureCount != 1 {
		t.Fatalf("after fourth escalations=%d failures=%d want 3/1", queueCount, failureCount)
	}
	var reason string
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := f.st.DB().QueryRowContext(ctx, `SELECT reason FROM runs WHERE job_id=? AND kind='agent'`, f.ownerID).Scan(&reason)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owner escalation run did not launch")
		}
		time.Sleep(50 * time.Millisecond)
	}
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
