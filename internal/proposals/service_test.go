package proposals_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/proposals"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

type fakeBus struct{ topics []string }

func (f *fakeBus) Append(_ context.Context, ev store.BusEvent) (int64, error) {
	f.topics = append(f.topics, ev.Topic)
	return 1, nil
}

type fakeRuns struct{ messages []string }

func (f *fakeRuns) InjectMessage(_ context.Context, _ string, text string) error {
	f.messages = append(f.messages, text)
	return nil
}

func newTestService(t *testing.T) (*proposals.Service, *workspaces.Workspace, *fakeBus, *fakeRuns, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenPath(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := &workspaces.Workspace{ID: "ws1", Name: "one", Path: dir}
	fs := files.New(0)
	bus := &fakeBus{}
	runs := &fakeRuns{}
	return proposals.NewService(st, fs, fakeIndex{ws}, bus, runs), ws, bus, runs, dir
}

type fakeIndex struct{ ws *workspaces.Workspace }

func (f fakeIndex) Get(_ context.Context, _ string) (*workspaces.Workspace, error) { return f.ws, nil }
func (fakeIndex) RebuildIndex(context.Context, *workspaces.Workspace) error        { return nil }
func (fakeIndex) JobByName(_ context.Context, ws, name string) (*workspaces.JobRow, error) {
	if name != "agents/research" {
		return nil, sql.ErrNoRows
	}
	return &workspaces.JobRow{ID: agentID, WorkspaceID: ws, Name: name, Kind: "agent"}, nil
}

const agentID = "agt_0000000001"

const baseAgent = "---\nid: " + agentID + "\ntitle: Research\nemail:\n  accept: []\n---\n\nRead only.\n"
const nextAgent = "---\nid: " + agentID + "\ntitle: Research\nemail:\n  accept:\n    - \"ops@example.com\"\n---\n\nRead less.\n"

func seedFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "agents", "research", "AGENT.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProposalLifecycle(t *testing.T) {
	ctx := context.Background()
	svc, ws, bus, runMessenger, dir := newTestService(t)
	seedFile(t, dir, baseAgent)

	p, err := svc.Create(ctx, proposals.CreateOpts{WorkspaceID: ws.ID, Agent: "agents/research", RunID: "run1", Proposed: []byte(nextAgent), Note: "trust ops"})
	if err != nil {
		t.Fatal(err)
	}
	if p.BaseHash == "" || p.BaseContent != baseAgent || len(p.ProtectedKeys) != 1 || p.ProtectedKeys[0] != "email.accept" {
		t.Fatalf("proposal=%+v", p)
	}
	if len(bus.topics) != 1 || bus.topics[0] != "proposal.created" {
		t.Fatalf("bus=%v", bus.topics)
	}

	same, err := svc.Create(ctx, proposals.CreateOpts{WorkspaceID: ws.ID, Agent: "agents/research", Proposed: []byte(baseAgent)})
	if !errors.Is(err, proposals.ErrNoChange) || same.ID != "" {
		t.Fatalf("same create: %v %+v", err, same)
	}

	second, err := svc.Create(ctx, proposals.CreateOpts{WorkspaceID: ws.ID, Agent: "agents/research", Proposed: []byte(nextAgent + "\nMore.\n")})
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.Get(ctx, p.ID)
	if err != nil || first.Status != proposals.StatusSuperseded {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err := svc.Decide(ctx, first.ID, proposals.Decision{Allow: true, By: "ui"}); !errors.Is(err, proposals.ErrNotPending) {
		t.Fatalf("superseded decide err=%v", err)
	}

	seedFile(t, dir, baseAgent+"\n\nChanged behind the reviewer.\n")
	if _, err := svc.Decide(ctx, second.ID, proposals.Decision{Allow: true, By: "ui"}); !errors.Is(err, proposals.ErrStale) {
		t.Fatalf("stale decide err=%v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "agents", "research", "AGENT.md")); string(got) == nextAgent {
		t.Fatal("stale proposal was applied")
	}

	third, err := svc.Create(ctx, proposals.CreateOpts{WorkspaceID: ws.ID, Agent: "agents/research", RunID: "run2", Proposed: []byte(nextAgent)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Decide(ctx, third.ID, proposals.Decision{Allow: true, By: "ui"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "agents", "research", "AGENT.md"))
	if err != nil || string(got) != nextAgent {
		t.Fatalf("after allow=%q err=%v", got, err)
	}
	if _, err := svc.Decide(ctx, third.ID, proposals.Decision{Allow: true, By: "ui"}); !errors.Is(err, proposals.ErrNotPending) {
		t.Fatalf("applied decide err=%v", err)
	}

	denied, err := svc.Create(ctx, proposals.CreateOpts{WorkspaceID: ws.ID, Agent: "agents/research", RunID: "run3", Proposed: []byte("---\n---\n\nchanged\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Decide(ctx, denied.ID, proposals.Decision{Allow: false, By: "ui"}); err != nil {
		t.Fatal(err)
	}
	if len(runMessenger.messages) != 2 {
		t.Fatalf("run messages=%v", runMessenger.messages)
	}
}

func TestProposalInvalid(t *testing.T) {
	ctx := context.Background()
	svc, ws, _, _, dir := newTestService(t)
	seedFile(t, dir, baseAgent)
	if _, err := svc.Create(ctx, proposals.CreateOpts{WorkspaceID: ws.ID, Agent: "agents/research", Proposed: []byte("---\nname: wrong\n---\n\nBad\n")}); !errors.Is(err, proposals.ErrInvalid) {
		t.Fatalf("invalid err=%v", err)
	}
	if _, err := svc.Create(ctx, proposals.CreateOpts{WorkspaceID: ws.ID, Agent: "agents/bad id", Proposed: []byte(baseAgent)}); !errors.Is(err, proposals.ErrInvalid) {
		t.Fatalf("bad id err=%v", err)
	}
}
