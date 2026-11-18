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

	"github.com/trebi-ai/trebi/internal/hometest"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/visor"
	"github.com/trebi-ai/trebi/internal/waittest"
)

// TestMain lets the test binary run as the holder of a visor session.
func TestMain(m *testing.M) {
	if code, ok := visor.Hold(os.Args); ok {
		os.Exit(code)
	}
	hometest.Main(m)
}

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
	jobDir := opsJobDir(t, wsPath, "demo")
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

	cp := daemon.New(st, daemon.Options{QueueInterval: 30 * time.Millisecond})
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
		JobID:       indexID(t, st, ws.ID, "agents/ops/demo"), JobName: "agents/ops/demo",
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

	waittest.UntilWake(t, st.BusWake, "the queue item to be done", func() bool {
		var status string
		err := st.DB().QueryRow(`SELECT status FROM event_queue WHERE id=?`, item.ID).Scan(&status)
		return err == nil && status == "done"
	})
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
	jobDir := opsJobDir(t, wsPath, "tick")
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
	jobDir := opsJobDir(t, wsPath, "gated")
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
	jobDir := opsJobDir(t, wsPath, "blocked")
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
	if got.PaneSession != "" {
		t.Fatalf("pane %q; harness must not launch", got.PaneSession)
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
	waittest.Until(t, "the shadow alerts worker to consume", func() bool {
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM inbox_items WHERE subject=?`, "connector/mail").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n >= 1
	})
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
	waittest.Until(t, "the connector to start", func() bool {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		return pid != 0
	})
	start := time.Now()
	cp.Stop()
	if d := time.Since(start); d > waittest.D(5*time.Second) {
		t.Fatalf("Stop took %v", d)
	}
	// The group is gone once every member is reaped; allow a short delay.
	waittest.Until(t, "a connector child to end after Stop", func() bool { return syscall.Kill(-pid, 0) != nil })
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
	jobDir := opsJobDir(t, wsPath, "acts")
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

// TestControlPlane_RestartResumesRun: Stop detaches a run that is mid-turn.
// The visor of the daemon process closes with it, so the next ControlPlane on
// the same store finds no live session, parks the run, and resumes it as
// turn 2.
func TestControlPlane_RestartResumesRun(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("tools:\n  claude: {enabled: true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	wsPath := filepath.Join(dir, "ws")
	jobDir := opsJobDir(t, wsPath, "demo")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte("---\nharness: claude\n---\n\nDo work.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Turn 1 works until the daemon stops; the restart prompt finishes the task.
	drv := agentwire.FakeDriver{Script: `read -r l; echo '{"type":"init","session_id":"s1"}'; case "$l" in ` +
		`*"Trebi restarted"*) echo '{"type":"result","subtype":"success","text":"done"}';; ` +
		`*) echo '{"type":"assistant","text":"working"}'; sleep 60;; esac`}
	ctx := context.Background()
	cp := daemon.New(st, daemon.Options{})
	cp.Runs.SetCLIDriver(drv)
	ws, err := cp.Workspaces.Add(ctx, wsPath, "demo-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Workspaces.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	cp.Start(ctx)
	run, _, err := cp.Runs.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: ws.ID, JobID: indexID(t, st, ws.ID, "agents/ops/demo"), JobName: "agents/ops/demo", Kind: runs.KindJob,
			Harness: "claude", Reason: runs.ReasonManual, WorkingDir: jobDir,
		},
		Prompt: "Do work.",
	})
	if err != nil {
		cp.Stop()
		t.Fatal(err)
	}
	waittest.Until(t, "the run session to be live", func() bool {
		got, err := cp.Runs.Get(ctx, run.ID)
		return err == nil && got.SessionID == "s1" && cp.Runs.SessionState(run.ID) == runs.SessionRunning
	})
	cp.Stop()
	got, err := cp.Runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runs.StatusRunning {
		t.Fatalf("after stop: %s/%s", got.Status, got.EndReason)
	}

	cp2 := daemon.New(st, daemon.Options{})
	cp2.Runs.SetCLIDriver(drv)
	cp2.Start(ctx)
	defer cp2.Stop()
	// Recover runs in the background, so poll.
	waittest.Until(t, "the second turn after boot", func() bool {
		turns, err := cp2.Runs.Turns(ctx, run.ID)
		return err == nil && len(turns) == 2
	})
	done, err := cp2.Runs.WaitFor(ctx, run.ID, waittest.D(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != runs.StatusComplete || done.TurnCount != 2 {
		t.Fatalf("after boot: %s/%s turn %d", done.Status, done.EndReason, done.TurnCount)
	}
	turns, err := cp2.Runs.Turns(ctx, run.ID)
	if err != nil || len(turns) != 2 || turns[0].EndReason != runs.EndRestart || turns[1].Reason != runs.ReasonResume {
		t.Fatalf("turns %+v err %v", turns, err)
	}
}

// TestJobOutsideAgentDoesNotRun proves that a schedule row and a queue item
// of a job outside agents/ launch no run. The queue item is blocked.
func TestJobOutsideAgentDoesNotRun(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	wsPath := filepath.Join(dir, "ws")
	jobPath := filepath.Join(wsPath, "reports", "digest", "JOB.md")
	writeFileAt(t, jobPath, "---\nharness: fake\ntriggers:\n  - every: 1h\n---\n\nDigest.\n")

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cp := daemon.New(st, daemon.Options{QueueInterval: 30 * time.Millisecond})
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Workspaces.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	jobID := indexID(t, st, ws.ID, "reports/digest")

	// A schedule row from before the rule still exists: it must not fire.
	j, err := job.ParseFile(jobPath)
	if err != nil || j == nil || j.ID != jobID {
		t.Fatalf("parse: %+v %v", j, err)
	}
	if err := cp.Schedule.RecomputeFromJob(ctx, ws.ID, j, time.Time{}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := st.DB().Exec(`UPDATE schedules SET next_due_at=? WHERE job_id=?`, past, jobID); err != nil {
		t.Fatal(err)
	}
	cp.FireDue(ctx)
	if n := runCount(t, st, jobID); n != 0 {
		t.Fatalf("schedule fired %d runs for a job outside an agent", n)
	}

	item := &dispatch.QueueItem{
		Connector: "x", WorkspaceID: ws.ID, JobID: jobID, JobName: "reports/digest",
		Kind: runs.KindJob, Status: dispatch.QueuePending, EventRaw: `{}`,
	}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()
	status, _, _ := waitQueue(t, st, item.ID, func(status, _, _ string) bool { return status == "blocked" })
	var reason string
	_ = st.DB().QueryRow(`SELECT COALESCE(block_reason,'') FROM event_queue WHERE id=?`, item.ID).Scan(&reason)
	if status != "blocked" || reason != string(dispatch.BlockJobMissing) {
		t.Fatalf("queue status=%s block_reason=%s want blocked/job_missing", status, reason)
	}
	if n := runCount(t, st, jobID); n != 0 {
		t.Fatalf("queue launched %d runs for a job outside an agent", n)
	}
}

// TestJobTakesAgentHarness proves that a job with no harness launches with
// the harness and the model of its agent.
func TestJobTakesAgentHarness(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	// The run stays live, so the fake run does not replace the launch model.
	t.Setenv("TREBI_FAKE_HARNESS_CMD", "sleep 5")
	t.Cleanup(func() { home.SetBase("") })

	wsPath := filepath.Join(dir, "ws")
	writeFileAt(t, filepath.Join(wsPath, "agents", "lead", "AGENT.md"),
		"---\ndescription: Test agent.\nharness:\n  name: fake\n  model: m1\n---\n\nLead.\n")
	writeFileAt(t, filepath.Join(wsPath, "agents", "lead", "digest", "JOB.md"), "---\ntitle: Digest\n---\n\nDigest.\n")

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cp := daemon.New(st, daemon.Options{QueueInterval: 30 * time.Millisecond})
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Workspaces.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	item := &dispatch.QueueItem{
		Connector: "x", WorkspaceID: ws.ID, JobID: indexID(t, st, ws.ID, "agents/lead/digest"), JobName: "agents/lead/digest",
		Kind: runs.KindJob, Status: dispatch.QueuePending, EventRaw: `{}`,
	}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()
	var runID string
	waittest.Until(t, "a run", func() bool {
		_ = st.DB().QueryRow(`SELECT id FROM runs WHERE job_id=?`, item.JobID).Scan(&runID)
		return runID != ""
	})
	run, err := cp.Runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Harness != "fake" || run.Model != "m1" {
		t.Fatalf("run harness=%q model=%q want fake/m1", run.Harness, run.Model)
	}
}

// stopHolders stops the visor sessions under dir when the test ends. A
// stopped daemon leaves its session holders alive on purpose, and a holder
// of a test would otherwise run after the test binary.
func stopHolders(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waittest.D(10*time.Second))
		defer cancel()
		o := agentvisor.Options{Home: filepath.Join(dir, "visor")}
		visor.Configure(&o)
		v, err := agentvisor.New(ctx, o)
		if err != nil {
			t.Errorf("open the visor: %v", err)
			return
		}
		defer func() { _ = v.Close(ctx) }()
		sessions, err := v.Sessions(ctx, agentvisor.Filter{})
		if err != nil {
			t.Errorf("list the visor sessions: %v", err)
			return
		}
		for _, s := range sessions {
			if s.State.Live() {
				if err := v.Stop(ctx, s.ID); err != nil {
					t.Errorf("stop session %s: %v", s.ID, err)
				}
			}
		}
	})
}

func runCount(t *testing.T, st *store.Store, jobID string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM runs WHERE job_id=?`, jobID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
