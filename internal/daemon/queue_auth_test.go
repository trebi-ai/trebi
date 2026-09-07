package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestQueueDrain_AuthMissingBlocks proves readiness gate sets status=blocked.
func TestQueueDrain_AuthMissingBlocks(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	// Do NOT set TREBI_FAKE_HARNESS so auth gate applies
	t.Cleanup(func() {
		home.SetBase("")
		_ = os.Unsetenv("TREBI_FAKE_HARNESS")
	})
	_ = os.Unsetenv("TREBI_FAKE_HARNESS")

	wsPath := filepath.Join(dir, "ws")
	jobDir := filepath.Join(wsPath, "gated")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"),
		[]byte("---\nharness: claude\n---\n\nGated.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = st.DB().Exec(`INSERT INTO harness_state (name, auth_ok, limit_exceeded, updated_at) VALUES ('claude', 0, 0, ?)`, now)
	if err != nil {
		t.Fatal(err)
	}

	cp := daemon.New(st, daemon.Options{QueueInterval: 50 * time.Millisecond})
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "auth-ws")
	if err != nil {
		t.Fatal(err)
	}
	_ = cp.Workspaces.RebuildIndex(ctx, ws)

	item := &dispatch.QueueItem{
		Connector: "x", WorkspaceID: ws.ID, Job: "gated",
		Status: dispatch.QueuePending, EventRaw: `{}`,
	}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	deadline := time.Now().Add(5 * time.Second)
	var status, br string
	for time.Now().Before(deadline) {
		_ = st.DB().QueryRow(`SELECT status, COALESCE(block_reason,'') FROM event_queue WHERE id=?`, item.ID).
			Scan(&status, &br)
		if status == "blocked" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status != "blocked" || br != string(dispatch.BlockAuthMissing) {
		t.Fatalf("status=%s block_reason=%s want blocked/auth_missing", status, br)
	}

	if _, err := harness.New(st, nil, harness.Options{}).Stamp(ctx, harness.AuthStatus{Tool: "claude", Status: harness.AuthOK}, harness.SourceProbe); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = st.DB().QueryRow(`SELECT status FROM event_queue WHERE id=?`, item.ID).Scan(&status)
		if status != "blocked" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status == "blocked" {
		t.Fatal("queue should leave blocked after auth recovery")
	}
}

// TestQueueDrain_IfRunningSkipResult marks queue result=skipped when live run exists.
func TestQueueDrain_IfRunningSkipResult(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	wsPath := filepath.Join(dir, "ws")
	jobDir := filepath.Join(wsPath, "skipme")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\nsession:\n  if_running: skip\n---\n\nSkip.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := daemon.New(st, daemon.Options{QueueInterval: 50 * time.Millisecond})
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "skip-ws")
	if err != nil {
		t.Fatal(err)
	}
	_ = cp.Workspaces.RebuildIndex(ctx, ws)

	// Seed a live run for the same job
	_, err = cp.Runs.Create(ctx, runs.CreateOpts{
		WorkspaceID: ws.ID, Job: "skipme", Harness: "fake", TmuxSession: "trebi-live-skip",
	})
	if err != nil {
		t.Fatal(err)
	}

	item := &dispatch.QueueItem{
		Connector: "x", WorkspaceID: ws.ID, Job: "skipme",
		Status: dispatch.QueuePending, EventRaw: `{}`,
	}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	deadline := time.Now().Add(5 * time.Second)
	var status, result string
	for time.Now().Before(deadline) {
		_ = st.DB().QueryRow(`SELECT status, COALESCE(result,'') FROM event_queue WHERE id=?`, item.ID).
			Scan(&status, &result)
		if status == "done" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status != "done" || result != string(dispatch.ResultSkipped) {
		t.Fatalf("status=%s result=%s want done/skipped", status, result)
	}
}
