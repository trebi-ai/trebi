package daemon_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/pauses"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

type pauseEnv struct {
	st     *store.Store
	cp     *daemon.ControlPlane
	ws     *workspaces.Workspace
	jobID  string
	jobDir string
}

func (e pauseEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.st.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e pauseEnv) makeDue(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	j, err := job.ParseFile(filepath.Join(e.jobDir, "JOB.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cp.Schedule.RecomputeFromJob(ctx, e.ws.ID, j, time.Time{}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := e.st.DB().Exec(`UPDATE schedules SET next_due_at=? WHERE job_id=?`, past, e.jobID); err != nil {
		t.Fatal(err)
	}
}

// TestPauseSkipsScheduleFire: a held fire makes no run and counts the skip.
// With missed once, the resume makes the job due, and it runs once.
func TestPauseSkipsScheduleFire(t *testing.T) {
	e := newPauseEnv(t, daemon.Options{})
	ctx := context.Background()
	p, _, err := e.cp.Pauses.Set(ctx, pauses.SetOpts{Scope: pauses.ScopeWorkspace, Target: e.ws.ID, Missed: pauses.MissedOnce})
	if err != nil {
		t.Fatal(err)
	}
	e.makeDue(t)
	e.cp.FireDue(ctx)
	if n := e.count(t, `SELECT COUNT(*) FROM runs WHERE job_id=?`, e.jobID); n != 0 {
		t.Fatalf("runs while paused = %d", n)
	}
	got, err := e.cp.Pauses.Get(ctx, p.ID)
	if err != nil || got.SkippedFires != 1 {
		t.Fatalf("skipped = %+v err=%v", got, err)
	}
	// The job switch is not touched.
	if n := e.count(t, `SELECT COUNT(*) FROM job_index WHERE id=? AND active=1`, e.jobID); n != 1 {
		t.Fatal("pause changed the job switch")
	}
	if _, err := e.cp.Pauses.Resume(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	e.cp.FireDue(ctx)
	if n := e.count(t, `SELECT COUNT(*) FROM runs WHERE job_id=? AND reason='schedule'`, e.jobID); n != 1 {
		t.Fatalf("runs after resume = %d, want 1", n)
	}
}

// TestPauseSkipDropsMissedFire: with missed skip, the resume does not run
// the job.
func TestPauseSkipDropsMissedFire(t *testing.T) {
	e := newPauseEnv(t, daemon.Options{})
	ctx := context.Background()
	p, _, err := e.cp.Pauses.Set(ctx, pauses.SetOpts{Scope: pauses.ScopeAll})
	if err != nil {
		t.Fatal(err)
	}
	e.makeDue(t)
	e.cp.FireDue(ctx)
	if _, err := e.cp.Pauses.Resume(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	e.cp.FireDue(ctx)
	if n := e.count(t, `SELECT COUNT(*) FROM runs WHERE job_id=?`, e.jobID); n != 0 {
		t.Fatalf("runs = %d, want 0", n)
	}
}

// TestPauseHoldsQueueRow: an event row waits while paused and runs after
// the resume.
func TestPauseHoldsQueueRow(t *testing.T) {
	e := newPauseEnv(t, daemon.Options{QueueInterval: 50 * time.Millisecond})
	ctx := context.Background()
	p, _, err := e.cp.Pauses.Set(ctx, pauses.SetOpts{Scope: pauses.ScopeHarness, Target: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	item := &dispatch.QueueItem{Connector: "test-src", WorkspaceID: e.ws.ID, JobID: e.jobID, JobName: "agents/ops/tick",
		Kind: runs.KindJob, EventRaw: `{"n":1}`, Status: dispatch.QueuePending}
	if err := dispatch.NewQueue(e.st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.cp.Start(runCtx)
	defer e.cp.Stop()
	held := func() bool {
		return e.count(t, `SELECT COUNT(*) FROM event_queue WHERE id=? AND status='pending' AND error_message=?`,
			item.ID, dispatch.DeferPaused) == 1
	}
	waitFor(t, held)
	time.Sleep(200 * time.Millisecond)
	if !held() || e.count(t, `SELECT COUNT(*) FROM runs WHERE job_id=?`, e.jobID) != 0 {
		t.Fatal("paused row ran")
	}
	if _, err := e.cp.Pauses.Resume(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return e.count(t, `SELECT COUNT(*) FROM event_queue WHERE id=? AND status='done'`, item.ID) == 1
	})
}

// TestPauseGateOnLaunch: a launch by a person passes; a launch by a
// trigger is refused.
func TestPauseGateOnLaunch(t *testing.T) {
	e := newPauseEnv(t, daemon.Options{})
	ctx := context.Background()
	if _, _, err := e.cp.Pauses.Set(ctx, pauses.SetOpts{Scope: pauses.ScopeAll}); err != nil {
		t.Fatal(err)
	}
	opts := func(reason runs.RunReason) runs.LaunchOpts {
		return runs.LaunchOpts{CreateOpts: runs.CreateOpts{WorkspaceID: e.ws.ID, JobID: e.jobID, JobName: "agents/ops/tick",
			Kind: runs.KindJob, Harness: "fake", Reason: reason, WorkingDir: e.jobDir}, Prompt: "Tick."}
	}
	if _, _, err := e.cp.Runs.CreateAndLaunch(ctx, opts(runs.ReasonTrigger)); !errors.Is(err, runs.ErrPaused) {
		t.Fatalf("trigger launch: %v", err)
	}
	if _, _, err := e.cp.Runs.CreateAndLaunch(ctx, opts(runs.ReasonManual)); err != nil {
		t.Fatalf("manual launch: %v", err)
	}
}

// TestAutoPauseNearLimit: the pause worker pauses a harness with a window
// at its pause_at percent, and an alert names it.
func TestAutoPauseNearLimit(t *testing.T) {
	e := newPauseEnv(t, daemon.Options{PauseInterval: 50 * time.Millisecond})
	ctx := context.Background()
	reset := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	if _, err := e.st.DB().Exec(`INSERT INTO harness_usage (harness, window_key, scope, minutes, used_percent, resets_at, plan, source, observed_at)
		VALUES ('fake','five_hour','session',300,99.5,?,'max','test',?), ('codex','five_hour','session',300,50,?,'pro','test',?)`,
		reset, time.Now().UTC().Format(time.RFC3339), reset, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB().Exec(`INSERT INTO harness_state (name, limit_exceeded, updated_at) VALUES ('grok', 1, ?)`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.cp.Start(runCtx)
	defer e.cp.Stop()
	waitFor(t, func() bool {
		return e.count(t, `SELECT COUNT(*) FROM pauses WHERE source='limit' AND ended_at IS NULL`) == 2
	})
	if n := e.count(t, `SELECT COUNT(*) FROM pauses WHERE target='fake' AND until_at=?`, reset); n != 1 {
		t.Fatalf("fake pause until %s missing", reset)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM pauses WHERE target='codex'`); n != 0 {
		t.Fatal("codex paused below its threshold")
	}
}

func newPauseEnv(t *testing.T, opts daemon.Options) pauseEnv {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })
	wsPath := filepath.Join(dir, "ws")
	jobDir := opsJobDir(t, wsPath, "tick")
	writeFileAt(t, filepath.Join(jobDir, "JOB.md"), "---\nharness: fake\ntriggers:\n  - every: 1h\n---\n\nTick.\n")
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // test cleanup
	cp := daemon.New(st, opts)
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "pause-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Workspaces.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	return pauseEnv{st: st, cp: cp, ws: ws, jobID: indexID(t, st, ws.ID, "agents/ops/tick"), jobDir: jobDir}
}
