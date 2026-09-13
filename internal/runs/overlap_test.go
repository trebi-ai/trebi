package runs_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestIfRunningSkip(t *testing.T) {
	svc, ctx := openRuns(t)
	live, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "demo", Harness: "fake", Reason: runs.ReasonManual,
		TmuxSession: "trebi-live-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if live.Status != runs.StatusRunning {
		t.Fatalf("%s", live.Status)
	}

	r2, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "demo", Harness: "fake", Reason: runs.ReasonTrigger,
			PromptPreview: "b",
		},
		Prompt: "b", IfRunning: "skip",
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "skip" {
		t.Fatalf("outcome=%s", outcome)
	}
	if r2.Status != runs.StatusSkipped {
		t.Fatalf("status=%s want skipped", r2.Status)
	}
	if r2.EndReason != runs.EndSkip {
		t.Fatalf("end_reason=%s want skip", r2.EndReason)
	}
	// original still running
	got, _ := svc.Get(ctx, live.ID)
	if got.Status != runs.StatusRunning {
		t.Fatalf("live status=%s", got.Status)
	}
}

func TestIfRunningReplace(t *testing.T) {
	svc, ctx := openRuns(t)
	live, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "demo", Harness: "fake", TmuxSession: "trebi-live-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "demo", Harness: "fake", Reason: runs.ReasonManual,
		},
		Prompt: "x", IfRunning: "replace",
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "replace" {
		t.Fatalf("outcome=%s", outcome)
	}
	got, _ := svc.Get(ctx, live.ID)
	if got.Status != runs.StatusExited || got.EndReason != runs.EndReplace {
		t.Fatalf("replaced run: status=%s end=%s", got.Status, got.EndReason)
	}
	if r2.Status != runs.StatusComplete && r2.Status != runs.StatusRunning {
		t.Fatalf("new run status=%s", r2.Status)
	}
}

func TestIfRunningCoalesce(t *testing.T) {
	svc, ctx := openRuns(t)
	live, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "demo", Harness: "fake", TmuxSession: "trebi-live-3",
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "demo", Harness: "fake", Reason: runs.ReasonTrigger,
		},
		Prompt: "coalesce me", IfRunning: "coalesce",
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "coalesce" {
		t.Fatalf("outcome=%s", outcome)
	}
	if r2.ID != live.ID {
		t.Fatalf("coalesce should return existing id %s got %s", live.ID, r2.ID)
	}
	// timeline event coalesce
	tl, err := svc.Timeline(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range tl.Events {
		if e.Kind == "coalesce" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected coalesce event in timeline: %+v", tl.Events)
	}
}

func TestIfRunningSkipAllowsNewAssistantConversation(t *testing.T) {
	svc, ctx := openRuns(t)
	live, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "nightly", Kind: runs.KindAssistant, Harness: "fake",
		ConversationKey: "conv-a", TmuxSession: "trebi-live-a",
	})
	if err != nil {
		t.Fatal(err)
	}

	r2, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "nightly", Kind: runs.KindAssistant, Harness: "fake",
			ConversationKey: "conv-b", Reason: runs.ReasonTrigger, PromptPreview: "hi",
		},
		Prompt: "hi", IfRunning: "skip",
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "" {
		t.Fatalf("new conversation should launch, outcome=%s", outcome)
	}
	if r2.ID == live.ID || r2.Status == runs.StatusSkipped {
		t.Fatalf("new conversation skipped: id=%s status=%s", r2.ID, r2.Status)
	}
	got, _ := svc.Get(ctx, live.ID)
	if got.Status != runs.StatusRunning {
		t.Fatalf("original conversation should stay live: %s", got.Status)
	}

	r3, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "nightly", Kind: runs.KindAssistant, Harness: "fake",
			ConversationKey: "conv-a", Reason: runs.ReasonTrigger, PromptPreview: "again",
		},
		Prompt: "again", IfRunning: "skip",
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "skip" || r3.Status != runs.StatusSkipped {
		t.Fatalf("same conversation should skip: outcome=%s status=%s", outcome, r3.Status)
	}
}

func TestIfRunningAllow(t *testing.T) {
	svc, ctx := openRuns(t)
	_, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "demo", Harness: "fake", TmuxSession: "trebi-live-4",
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "demo", Harness: "fake", Reason: runs.ReasonManual,
		},
		Prompt: "parallel", IfRunning: "allow",
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "allow" {
		t.Fatalf("outcome=%s", outcome)
	}
	if r2.Status != runs.StatusComplete && r2.Status != runs.StatusRunning {
		t.Fatalf("status=%s", r2.Status)
	}
	// two rows for job demo
	list, err := svc.List(ctx, runs.ListOpts{Workspace: "ws", Job: "demo", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 2 {
		t.Fatalf("want ≥2 runs got %d", len(list))
	}
}

func openRuns(t *testing.T) (*runs.Service, context.Context) {
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
	return runs.New(st, nil), context.Background()
}
