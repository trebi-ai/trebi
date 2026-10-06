package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
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
		Connector:   "test-src",
		WorkspaceID: ws.ID,
		JobID:       "demo", JobName: "demo",
		Kind:     runs.KindJob,
		EventRaw: `{"n":1}`,
		Status:   dispatch.QueuePending,
		Vars:     map[string]string{"x": "1"},
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
	res, err := st.DB().Exec(`UPDATE schedules SET next_due_at=? WHERE workspace_id=? AND job_id=?`, past, ws.ID, j.ID)
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
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM runs WHERE job_id=? AND reason='schedule'`, j.ID).Scan(&n)
	if n < 1 {
		// one more tick path
		cp.FireDue(ctx)
		_ = st.DB().QueryRow(`SELECT COUNT(*) FROM runs WHERE job_id=? AND reason='schedule'`, j.ID).Scan(&n)
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
	if _, err := st.DB().Exec(`UPDATE schedules SET next_due_at=? WHERE workspace_id=? AND job_id=?`, past, ws.ID, j.ID); err != nil {
		t.Fatal(err)
	}

	cp.FireDue(ctx)

	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM runs WHERE job_id=?`, j.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("gated job launched %d run(s); want 0", n)
	}
	var nextDue string
	if err := st.DB().QueryRow(`SELECT next_due_at FROM schedules WHERE workspace_id=? AND job_id=?`, ws.ID, j.ID).Scan(&nextDue); err != nil {
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

// TestScheduleFireDueAuthMissingCreatesFailedRun proves a due cron with a
// blocked harness still creates a run so the attempt is visible.
func TestScheduleFireDueAuthMissingCreatesFailedRun(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	// Do NOT set TREBI_FAKE_HARNESS so auth gate applies.
	t.Cleanup(func() {
		home.SetBase("")
		_ = os.Unsetenv("TREBI_FAKE_HARNESS")
	})
	_ = os.Unsetenv("TREBI_FAKE_HARNESS")

	wsPath := filepath.Join(dir, "ws")
	jobDir := filepath.Join(wsPath, "blocked")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jobMD := "---\nharness: claude\ntriggers:\n  - every: 1h\n---\n\nBlocked.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(jobMD), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB().Exec(`INSERT INTO harness_state (name, auth_ok, limit_exceeded, updated_at) VALUES ('claude', 0, 0, ?)`, now); err != nil {
		t.Fatal(err)
	}

	cp := daemon.New(st, daemon.Options{})
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "auth-sched-ws")
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
	if _, err := st.DB().Exec(`UPDATE schedules SET next_due_at=? WHERE workspace_id=? AND job_id=?`, past, ws.ID, j.ID); err != nil {
		t.Fatal(err)
	}

	cp.FireDue(ctx)

	var id, status, end, reason, lastFired, nextDue string
	err = st.DB().QueryRow(`SELECT id, status, end_reason, reason FROM runs WHERE job_id=?`, j.ID).
		Scan(&id, &status, &end, &reason)
	if err != nil {
		t.Fatalf("expected a schedule run: %v", err)
	}
	if reason != string(runs.ReasonSchedule) || status != string(runs.StatusFailed) || end != string(runs.EndAuth) {
		t.Fatalf("run reason=%s status=%s end=%s want schedule/failed/auth", reason, status, end)
	}
	got, err := cp.Runs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.TmuxSession != "" {
		t.Fatalf("tmux session %q; harness must not launch", got.TmuxSession)
	}
	tl, err := cp.Runs.Timeline(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range tl.Events {
		if ev.Kind == "error" && strings.Contains(string(ev.Detail), "auth_missing") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("timeline missing auth_missing error: %+v", tl.Events)
	}
	logb, err := os.ReadFile(filepath.Join(home.RunsLogDir(), id+".log"))
	if err != nil {
		t.Fatalf("run log: %v", err)
	}
	if !strings.Contains(string(logb), "auth_missing") {
		t.Fatalf("run log %q want auth_missing", logb)
	}
	if err := st.DB().QueryRow(`SELECT COALESCE(last_fired_at,''), next_due_at FROM schedules WHERE workspace_id=? AND job_id=?`, ws.ID, j.ID).
		Scan(&lastFired, &nextDue); err != nil {
		t.Fatal(err)
	}
	if lastFired == "" {
		t.Fatal("last_fired_at empty after auth skip")
	}
	nd, err := time.Parse(time.RFC3339, nextDue)
	if err != nil {
		t.Fatal(err)
	}
	if !nd.After(time.Now().UTC()) {
		t.Fatalf("schedule did not advance: next_due_at=%s", nextDue)
	}
}

// TestControlPlane_ShadowSkipsAutoWorkers ensures shadow does not start connectors/queue/schedule.
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
		Connector: "x", WorkspaceID: "ws", JobID: "j", JobName: "j", Status: dispatch.QueuePending,
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
		Topic: "connector.unhealthy", Entity: "connector", EntityID: "mail", Action: "updated",
		Attrs: map[string]string{"connector": "mail", "status": "down"},
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM inbox_items WHERE subject=?`, "connector/mail").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("shadow alerts worker did not consume")
}

// TestControlPlane_StopEndsConnectorGroups proves Stop waits for the
// connector supervisors and that no child of a connector outlives it.
func TestControlPlane_StopEndsConnectorGroups(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := daemon.New(st, daemon.Options{})
	pidFile := filepath.Join(dir, "pid")
	d := connectors.Connection{Type: "hold", Slug: "hold", From: connectors.FromInline, Manifest: &manifest.Manifest{
		Schema: manifest.SchemaID, Name: "hold", Title: "Hold", Description: "Test connector.", Version: "1.0.0",
		Events: &manifest.Events{Command: "echo $$ > " + pidFile + "; sleep 30 | cat", Format: "json"},
	}}
	if _, err := cp.Connectors.Create(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	cp.Start(context.Background())

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && pid == 0 {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("the connector did not start")
	}
	start := time.Now()
	cp.Stop()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Stop took %v", d)
	}
	// The group is gone once every member is reaped; allow a short delay.
	deadline = time.Now().Add(3 * time.Second)
	for syscall.Kill(-pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("a connector child outlived Stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestScheduleActionErrorOpensOneInboxItem proves a schedule fire that fails
// on its connector actions writes no failed run and opens one inbox item per
// job per hour (P1-15).
func TestScheduleActionErrorOpensOneInboxItem(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "")
	_ = os.Unsetenv("TREBI_FAKE_HARNESS")
	t.Cleanup(func() { home.SetBase("") })

	wsPath := filepath.Join(dir, "ws")
	jobDir := filepath.Join(wsPath, "acts")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jobMD := "---\nharness: claude\nconnectors: [ghost]\ntriggers:\n  - every: 1h\n---\n\nGo.\n"
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
	ws, err := cp.Workspaces.Add(ctx, wsPath, "acts-ws")
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
	for i := 0; i < 2; i++ {
		past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
		if _, err := st.DB().Exec(`UPDATE schedules SET next_due_at=? WHERE workspace_id=? AND job_id=?`, past, ws.ID, j.ID); err != nil {
			t.Fatal(err)
		}
		cp.FireDue(ctx)
	}
	var runsN, items int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM runs WHERE job_id=?`, j.ID).Scan(&runsN)
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM inbox_items WHERE alert_kind='actions_invalid' AND job_id=?`, j.ID).Scan(&items)
	if runsN != 0 {
		t.Fatalf("runs=%d, want 0: an action error writes no failed run", runsN)
	}
	if items != 1 {
		t.Fatalf("inbox items=%d, want 1", items)
	}
}
