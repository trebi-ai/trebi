package runs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestDetectCompletion(t *testing.T) {
	t.Parallel()
	id := "01TESTRUNID00000000000000001"
	sent := CompletionSentinel(id)
	if !DetectCompletion("done\n"+sent+"\n", id) {
		t.Fatal("expected detect")
	}
	// echo of instruction should not count
	echo := "When done, output exactly this: " + sent
	if DetectCompletion(echo, id) {
		t.Fatal("echo should not count")
	}
}

func TestCountSentinels(t *testing.T) {
	t.Parallel()
	id := "01TESTRUNID00000000000000001"
	sent := CompletionSentinel(id)
	screen := "working\n" + sent + "\n" +
		"Fix MEMORY.md, then print the completion line again: `" + sent + "`\n" +
		"  " + sent + "  \n"
	if n := countSentinels(screen, id); n != 2 {
		t.Fatalf("countSentinels = %d, want 2 (exact lines only)", n)
	}
	if n := countSentinels(screen, "other"); n != 0 {
		t.Fatalf("countSentinels other run = %d, want 0", n)
	}
}

func TestReaperMaxDuration(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil, nil)
	ctx := context.Background()
	run, err := svc.Create(ctx, CreateOpts{
		WorkspaceID: "ws", Job: "j", Harness: "fake",
		MaxDurationMS: 1, // 1ms
	})
	if err != nil {
		t.Fatal(err)
	}
	// backdate started_at
	_, _ = st.DB().Exec(`UPDATE runs SET started_at=? WHERE id=?`,
		time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), run.ID)

	r := NewReaper(svc)
	r.interval = time.Hour // we call Sweep manually
	r.Sweep(ctx)

	got, err := svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusTimedOut {
		t.Fatalf("status=%s want timed_out", got.Status)
	}
	if got.EndReason != EndMaxDuration {
		t.Fatalf("end=%s want max_duration", got.EndReason)
	}
}

func TestFakeHarnessSentinelViaCreateAndLaunch(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	_ = os.Setenv("TREBI_HOME", dir)
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil, nil)
	ctx := context.Background()
	run, _, err := svc.CreateAndLaunch(ctx, LaunchOpts{
		CreateOpts: CreateOpts{
			WorkspaceID: "ws", Job: "demo", Harness: "fake",
			PromptPreview: "hi",
		},
		Prompt:      "hi",
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusComplete {
		t.Fatalf("status=%s", run.Status)
	}
	if run.EndReason != EndSentinel {
		t.Fatalf("end=%s", run.EndReason)
	}
}

func TestPendingPromptRoundtrip(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil, nil)
	svc.markLaunching("r1", true)
	if !svc.IsLaunching("r1") {
		t.Fatal("launching")
	}
	svc.markLaunching("r1", false)
	if svc.IsLaunching("r1") {
		t.Fatal("cleared")
	}
	svc.stashPendingPrompt("r1", pendingPrompt{session: "s", prompt: "hi", harness: "claude"})
	p, ok := svc.takePendingPrompt("r1")
	if !ok || p.prompt != "hi" || p.session != "s" {
		t.Fatalf("%v %v", ok, p)
	}
	if _, ok := svc.takePendingPrompt("r1"); ok {
		t.Fatal("second take")
	}
}
