package launch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/visor"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// TestMain lets the test binary run as the holder of a visor session.
func TestMain(m *testing.M) {
	if code, ok := visor.Hold(os.Args); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// TestFreshNewRun: Fresh makes a new run with a new session that follows the
// old run, and leaves the old run as it was.
func TestFreshNewRun(t *testing.T) {
	wss, rs, ws, row := launchSetup(t, "---\nharness: fake\n---\n\nDo work.\n")
	ctx := context.Background()
	prev, err := rs.Create(ctx, runs.CreateOpts{WorkspaceID: ws.ID, JobID: row.ID, JobName: "x", Kind: runs.KindJob, Harness: "fake", SessionID: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.Terminate(ctx, prev.ID, runs.StatusExited, runs.EndKill, nil); err != nil {
		t.Fatal(err)
	}
	run, err := NewLauncher(wss, rs).Fresh(ctx, prev.ID, runs.ResumeOpts{Prompt: "start again", Reason: runs.ReasonRequest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.WaitFor(ctx, run.ID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if run.ID == prev.ID || run.PreviousRunID != prev.ID || run.TurnCount != 1 || run.SessionID == "sess-1" {
		t.Fatalf("fresh run %s previous %q turns %d session %q", run.ID, run.PreviousRunID, run.TurnCount, run.SessionID)
	}
	if run.JobID != row.ID {
		t.Fatalf("fresh run job_id %q want %q", run.JobID, row.ID)
	}
	if old, _ := rs.Get(ctx, prev.ID); old.TurnCount != 1 || old.Status != runs.StatusExited {
		t.Fatalf("old run turns %d status %s", old.TurnCount, old.Status)
	}
}

// TestFromRunBatch: a rerun of a batch run gets the same events, and the
// events block when the job prompt has no {{events}}.
func TestFromRunBatch(t *testing.T) {
	vars := map[string]string{"event_count": "2", "events": "- a\n- b", "events_file": "/runs/r1/events.json"}
	cases := []struct{ prompt, want string }{
		{"Sum up.", "Sum up.\n\nNew events (2):\n- a\n- b\nAll events: /runs/r1/events.json"},
		{"Sum up {{event_count}}:\n{{events}}", "Sum up 2:\n- a\n- b"},
	}
	for _, tc := range cases {
		wss, rs, ws, row := launchSetup(t, "---\nharness: fake\n---\n\n"+tc.prompt+"\n")
		prev := &runs.Run{ID: "r1", Workspace: ws.ID, JobID: row.ID, JobName: "x", Kind: runs.KindJob, Trigger: &runs.TriggerCapture{
			Connector: "feed", Vars: vars, Batch: &runs.BatchCapture{Count: 2, EventIDs: []string{"e1", "e2"}},
		}}
		lopts, err := NewLauncher(wss, rs).FromRun(context.Background(), prev, runs.ReasonReplay)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(lopts.Prompt) != tc.want {
			t.Fatalf("prompt=%q want %q", lopts.Prompt, tc.want)
		}
	}
}

// launchSetup opens a store and a workspace with the job x of md.
func launchSetup(t *testing.T, md string) (*workspaces.Service, *runs.Service, *workspaces.Workspace, *workspaces.JobRow) {
	t.Helper()
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
	if err := os.WriteFile(filepath.Join(wsDir, "x", "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	wss := workspaces.New(st)
	ws, err := wss.Add(ctx, wsDir, "fresh-ws")
	if err != nil {
		t.Fatal(err)
	}
	row, err := wss.JobByName(ctx, ws.ID, "x")
	if err != nil {
		t.Fatal(err)
	}
	return wss, runs.New(st, nil, nil), ws, row
}
