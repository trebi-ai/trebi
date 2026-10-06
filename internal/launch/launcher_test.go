package launch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// TestFreshNewRun: Fresh makes a new run with a new session that follows the
// old run, and leaves the old run as it was.
func TestFreshNewRun(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	wsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsDir, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "x", "JOB.md"), []byte("---\nharness: fake\n---\n\nDo work.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	wss := workspaces.New(st)
	ws, err := wss.Add(ctx, wsDir, "fresh-ws")
	if err != nil {
		t.Fatal(err)
	}
	rs := runs.New(st, nil, nil)
	prev, err := rs.Create(ctx, runs.CreateOpts{WorkspaceID: ws.ID, JobID: "x", JobName: "x", Kind: runs.KindJob, Harness: "fake", SessionID: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.Terminate(ctx, prev.ID, runs.StatusExited, runs.EndKill, nil); err != nil {
		t.Fatal(err)
	}
	run, err := NewLauncher(wss, rs).Fresh(ctx, prev.ID, "start again", runs.ReasonRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.WaitFor(ctx, run.ID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if run.ID == prev.ID || run.PreviousRunID != prev.ID || run.TurnCount != 1 || run.SessionID == "sess-1" {
		t.Fatalf("fresh run %s previous %q turns %d session %q", run.ID, run.PreviousRunID, run.TurnCount, run.SessionID)
	}
	if old, _ := rs.Get(ctx, prev.ID); old.TurnCount != 1 || old.Status != runs.StatusExited {
		t.Fatalf("old run turns %d status %s", old.TurnCount, old.Status)
	}
}
