package runs_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestActorForTokenAndTerminalExpiry(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "actor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	const token = "0123456789abcdef0123456789abcdef"
	const agentID = "agt_00000000c1"
	seedIndex(t, st, agentID, "ws1", "research", "agent", "")
	run, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws1", JobID: agentID, JobName: "research", Kind: "agent",
		Harness: "claude", ActorToken: token,
	})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := svc.ActorForToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if actor.RunID != run.ID || actor.Kind != "agent" || actor.Job != "research" || actor.JobID != agentID || actor.WorkspaceID != "ws1" {
		t.Fatalf("actor=%+v", actor)
	}
	svc.SetSessionState(ctx, run.ID, runs.SessionWaiting, "test")
	actor, err = svc.ActorForToken(ctx, token)
	if err != nil {
		t.Fatalf("waiting session state: %v", err)
	}
	if actor.RunID != run.ID {
		t.Fatalf("waiting actor=%+v", actor)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusFailed, runs.EndFailed, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ActorForToken(ctx, token); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("terminal token err=%v", err)
	}
}
