package runs_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestFakeHarnessSentinel(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()

	run, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws",
			Job:         "demo",
			Harness:     "fake",
			Reason:      runs.ReasonManual,
		},
		Prompt:      "do it",
		WorkingDir:  t.TempDir(),
		FakeCommand: []string{"sh", "-c", `echo working; echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != runs.StatusComplete || run.EndReason != runs.EndSentinel {
		t.Fatalf("%+v", run)
	}
	tl, err := svc.Timeline(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Segments) < 1 {
		t.Fatal("no segments")
	}
}

func TestFakeHarnessFailure(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	run, _, err := svc.CreateAndLaunch(context.Background(), runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "demo", Harness: "fake", Reason: runs.ReasonManual,
		},
		WorkingDir:  t.TempDir(),
		FakeCommand: []string{"sh", "-c", "echo no sentinel; exit 1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != runs.StatusFailed {
		t.Fatalf("%+v", run)
	}
}
