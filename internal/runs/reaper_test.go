package runs_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestDetectCompletion(t *testing.T) {
	t.Parallel()
	id := "01TESTRUNID00000000000000001"
	sent := runs.CompletionSentinel(id)
	if !runs.DetectCompletion("done\n"+sent+"\n", id) {
		t.Fatal("expected detect")
	}
	// echo of instruction should not count
	echo := "When done, output exactly this: " + sent
	if runs.DetectCompletion(echo, id) {
		t.Fatal("echo should not count")
	}
}

func TestReaperMaxDuration(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()
	run, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "j", Harness: "fake",
		MaxDurationMS: 1, // 1ms
	})
	if err != nil {
		t.Fatal(err)
	}
	// backdate started_at
	_, _ = st.DB().Exec(`UPDATE runs SET started_at=? WHERE id=?`,
		time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), run.ID)

	r := runs.NewReaper(svc)
	r.Interval = time.Hour // we call Sweep manually
	r.Sweep(ctx)

	got, err := svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runs.StatusTimedOut {
		t.Fatalf("status=%s want timed_out", got.Status)
	}
	if got.EndReason != runs.EndMaxDuration {
		t.Fatalf("end=%s want max_duration", got.EndReason)
	}
}

func TestFakeHarnessSentinelViaCreateAndLaunch(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	_ = os.Setenv("TREBI_HOME", dir)
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st)
	ctx := context.Background()
	run, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "demo", Harness: "fake",
			PromptPreview: "hi",
		},
		Prompt:      "hi",
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != runs.StatusComplete {
		t.Fatalf("status=%s", run.Status)
	}
	if run.EndReason != runs.EndSentinel {
		t.Fatalf("end=%s", run.EndReason)
	}
}
