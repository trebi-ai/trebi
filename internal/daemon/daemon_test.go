package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestControlPlane_QueueWorkerDrainsPending proves the daemon queue worker
// claims a pending event_queue row and launches a run (fake harness).
func TestControlPlane_QueueWorkerDrainsPending(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	// Workspace + job on disk so drain can resolve the job prompt.
	wsPath := filepath.Join(dir, "ws")
	jobDir := filepath.Join(wsPath, "demo")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jobMD := "---\nharness: fake\n---\n\nDo work.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(jobMD), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := daemon.New(st, daemon.Options{})
	ctx := context.Background()

	ws, err := cp.Workspaces.Add(ctx, wsPath, "demo-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Workspaces.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}

	item := &dispatch.QueueItem{
		Integration: "test-src",
		WorkspaceID: ws.ID,
		Job:         "demo",
		Kind:        runs.KindJob,
		EventRaw:    `{"n":1}`,
		Status:      dispatch.QueuePending,
		Vars:        map[string]string{"x": "1"},
	}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}

	// Start workers and wait for drain.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		err := st.DB().QueryRow(`SELECT status FROM event_queue WHERE id=?`, item.ID).Scan(&status)
		if err == nil && status == "done" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	var status, runID string
	if err := st.DB().QueryRow(`SELECT status, COALESCE(run_id,'') FROM event_queue WHERE id=?`, item.ID).
		Scan(&status, &runID); err != nil {
		t.Fatal(err)
	}
	if status != "done" {
		t.Fatalf("queue status=%s want done", status)
	}
	if runID == "" {
		t.Fatal("expected run_id on completed queue row")
	}
	run, err := cp.Runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != runs.StatusComplete && run.Status != runs.StatusRunning {
		t.Fatalf("run status=%s", run.Status)
	}
	if run.Reason != runs.ReasonTrigger {
		t.Fatalf("reason=%s want trigger", run.Reason)
	}
}

// TestControlPlane_ScheduleFireDue advances a due schedule row into a run.
func TestControlPlane_ScheduleFireDue(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	wsPath := filepath.Join(dir, "ws")
	jobDir := filepath.Join(wsPath, "tick")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Cron-like every trigger via frontmatter
	jobMD := "---\nharness: fake\ntriggers:\n  - every: 1h\n---\n\nTick.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(jobMD), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := daemon.New(st, daemon.Options{
		ScheduleInterval: 50 * time.Millisecond,
		QueueInterval:    50 * time.Millisecond,
	})
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "sched-ws")
	if err != nil {
		t.Fatal(err)
	}
	_ = cp.Workspaces.RebuildIndex(ctx, ws)

	j, err := job.ParseFile(filepath.Join(jobDir, "JOB.md"))
	if err != nil || j == nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cp.Schedule.RecomputeFromJob(ctx, ws.ID, j, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// Force due now
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	res, err := st.DB().Exec(`UPDATE schedules SET next_due_at=? WHERE workspace_id=? AND job=?`, past, ws.ID, j.Name)
	if err != nil {
		t.Fatal(err)
	}
	nUp, _ := res.RowsAffected()
	if nUp < 1 {
		t.Fatal("no schedule row to advance — RecomputeFromJob produced none")
	}

	// Drive FireDue directly (also covered by ticker at 50ms).
	cp.FireDue(ctx)

	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM runs WHERE job=? AND reason='schedule'`, j.Name).Scan(&n)
	if n < 1 {
		// one more tick path
		cp.FireDue(ctx)
		_ = st.DB().QueryRow(`SELECT COUNT(*) FROM runs WHERE job=? AND reason='schedule'`, j.Name).Scan(&n)
	}
	if n < 1 {
		t.Fatal("expected at least one schedule-fired run")
	}
}

// TestScheduleWhenGateSkips proves a gated-off due entry advances the
// schedule and does not launch (A1/B25 regression).
func TestScheduleWhenGateSkips(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	wsPath := filepath.Join(dir, "ws")
	jobDir := filepath.Join(wsPath, "gated")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Gate always evaluates false → the entry must not fire.
	jobMD := "---\nharness: fake\ntriggers:\n  - every: 1h\n    when:\n      run: [\"true\"]\n      match: \"false\"\n---\n\nGated.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(jobMD), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := daemon.New(st, daemon.Options{})
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "gate-ws")
	if err != nil {
		t.Fatal(err)
	}
	_ = cp.Workspaces.RebuildIndex(ctx, ws)

	j, err := job.ParseFile(filepath.Join(jobDir, "JOB.md"))
	if err != nil || j == nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cp.Schedule.RecomputeFromJob(ctx, ws.ID, j, time.Time{}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := st.DB().Exec(`UPDATE schedules SET next_due_at=? WHERE workspace_id=? AND job=?`, past, ws.ID, j.Name); err != nil {
		t.Fatal(err)
	}

	cp.FireDue(ctx)

	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM runs WHERE job=?`, j.Name).Scan(&n)
	if n != 0 {
		t.Fatalf("gated job launched %d run(s); want 0", n)
	}
	var nextDue string
	if err := st.DB().QueryRow(`SELECT next_due_at FROM schedules WHERE workspace_id=? AND job=?`, ws.ID, j.Name).Scan(&nextDue); err != nil {
		t.Fatal(err)
	}
	nd, err := time.Parse(time.RFC3339, nextDue)
	if err != nil {
		t.Fatal(err)
	}
	if !nd.After(time.Now().UTC()) {
		t.Fatalf("schedule did not advance: next_due_at=%s", nextDue)
	}
}

// TestControlPlane_ShadowSkipsAutoWorkers ensures shadow does not start integrations/queue/schedule.
func TestControlPlane_ShadowSkipsAutoWorkers(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := daemon.New(st, daemon.Options{Shadow: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	// Enqueue a pending row — shadow must not drain it.
	item := &dispatch.QueueItem{
		Integration: "x", WorkspaceID: "ws", Job: "j", Status: dispatch.QueuePending,
	}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	// Poll: the row must stay pending across several would-be worker ticks.
	for i := 0; i < 5; i++ {
		time.Sleep(100 * time.Millisecond)
		var status string
		if err := st.DB().QueryRow(`SELECT status FROM event_queue WHERE id=?`, item.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "pending" {
			t.Fatalf("shadow drained queue: status=%s", status)
		}
	}
}

func TestControlPlane_ShadowStillConsumesAlerts(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := daemon.New(st, daemon.Options{Shadow: true, AlertsInterval: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	if _, err := st.Append(ctx, store.BusEvent{
		Topic: "integration.unhealthy", Entity: "integration", EntityID: "mail", Action: "updated",
		Attrs: map[string]string{"integration": "mail", "status": "down"},
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM inbox_items WHERE subject=?`, "integration/mail").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("shadow alerts worker did not consume")
}
