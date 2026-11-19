package workspaces_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func TestSetRouting(t *testing.T) {
	ctx := context.Background()
	svc, st, ws := routingWS(t)
	// A new route in a missing file starts off.
	f, err := svc.SetRouting(ctx, ws.ID, "gmail", routing.Patch{Routes: []map[string]any{{"agent": "ops"}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Routes) != 1 || f.Routes[0].Gets != routing.GetsOff || !strings.HasPrefix(f.Routes[0].ID, "rt_") {
		t.Fatalf("routes = %+v", f.Routes)
	}
	id := f.Routes[0].ID
	if routingUpdates(t, st) != 1 {
		t.Fatalf("updates = %d, want 1", routingUpdates(t, st))
	}
	// A route to a missing agent is refused and writes nothing.
	hash := f.Hash
	_, err = svc.SetRouting(ctx, ws.ID, "gmail", routing.Patch{Routes: []map[string]any{{"agent": "nope"}}}, "")
	var ve *routing.ValidationError
	if !errors.As(err, &ve) || ve.Problems[0].Code != routing.CodeRouteTargetMissing {
		t.Fatalf("err = %v", err)
	}
	if got, _ := svc.Routing(ctx, ws.ID, "gmail"); got.Hash != hash {
		t.Fatal("a refused patch changed the file")
	}
	// A stale hash gives ErrPrecondition.
	if _, err := svc.SetRouting(ctx, ws.ID, "gmail", routing.Patch{Notes: str("x")}, "stale"); !errors.Is(err, workspaces.ErrPrecondition) {
		t.Fatalf("err = %v", err)
	}
	// Merge by id with the current hash.
	f, err = svc.SetRouting(ctx, ws.ID, "gmail", routing.Patch{Mode: str("on"), Routes: []map[string]any{{"id": id, "gets": "chosen"}}}, hash)
	if err != nil || f.Mode != routing.ModeOn || f.Routes[0].Gets != routing.GetsChosen {
		t.Fatalf("merge = %+v %v", f, err)
	}
	// A new route while routing is on gets chosen messages.
	f, err = svc.SetRouting(ctx, ws.ID, "gmail", routing.Patch{Routes: []map[string]any{{"job": "ops/digest"}}}, "")
	if err != nil || f.Routes[1].Gets != routing.GetsChosen {
		t.Fatalf("add on = %+v %v", f.Routes, err)
	}
	// A patch with no change writes nothing and emits nothing.
	n := routingUpdates(t, st)
	if _, err := svc.SetRouting(ctx, ws.ID, "gmail", routing.Patch{Mode: str("on")}, ""); err != nil {
		t.Fatal(err)
	}
	if routingUpdates(t, st) != n {
		t.Fatal("a no-op patch emitted routing.updated")
	}
	// A patch that leaves an old fault in place passes.
	routingtest.WriteRaw(t, filepath.Join(ws.Path), "slack", "mode: off\nroutes:\n  - id: rt_aaaaaaaaaa\n    agent: gone\n    gets: off\n")
	if _, err := svc.SetRouting(ctx, ws.ID, "slack", routing.Patch{Notes: str("note")}, ""); err != nil {
		t.Fatalf("old fault blocked a patch: %v", err)
	}
}

func TestRouteRefs(t *testing.T) {
	ctx := context.Background()
	svc, _, ws := routingWS(t)
	routingtest.Write(t, ws.Path, "gmail", routing.File{Routes: []routing.Route{
		{ID: "rt_aaaaaaaaaa", Agent: "ops", Gets: routing.GetsOff},
		{ID: "rt_bbbbbbbbbb", Job: "ops/digest", Gets: routing.GetsOff},
		{ID: "rt_cccccccccc", Agent: "cmo", Gets: routing.GetsOff},
	}, Rules: []routing.Rule{{Field: routing.RuleSenderIs, Value: "a@b.c", To: "ops/digest"}, {Field: routing.RuleSenderIs, Value: "d@b.c", To: "cmo"}}})
	refs, err := svc.RouteRefs(ctx, ws.ID, []string{"ops"})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs.Routes) != 2 || len(refs.Rules) != 1 {
		t.Fatalf("agent refs = %+v", refs)
	}
	refs, err = svc.RouteRefs(ctx, ws.ID, []string{"ops/digest"})
	if err != nil || len(refs.Routes) != 1 || refs.Routes[0].Route != "rt_bbbbbbbbbb" || len(refs.Rules) != 1 {
		t.Fatalf("job refs = %+v %v", refs, err)
	}
	if refs, _ := svc.RouteRefs(ctx, ws.ID, []string{"none"}); !refs.Empty() {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestRemoveRouteRefsRestore(t *testing.T) {
	ctx := context.Background()
	svc, _, ws := routingWS(t)
	path := routingtest.Write(t, ws.Path, "gmail", routing.File{Routes: []routing.Route{
		{ID: "rt_aaaaaaaaaa", Agent: "ops", Gets: routing.GetsOff},
		{ID: "rt_cccccccccc", Agent: "cmo", Gets: routing.GetsOff},
	}, Rules: []routing.Rule{{Field: routing.RuleSenderIs, Value: "a@b.c", To: "ops"}}})
	before, _ := os.ReadFile(path)
	refs, err := svc.RouteRefs(ctx, ws.ID, []string{"ops"})
	if err != nil {
		t.Fatal(err)
	}
	restore, err := svc.RemoveRouteRefs(ctx, ws.ID, refs)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := svc.Routing(ctx, ws.ID, "gmail")
	if len(f.Routes) != 1 || f.Routes[0].Agent != "cmo" || len(f.Rules) != 0 {
		t.Fatalf("after remove = %+v", f)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("restore differs:\n%s\nwant\n%s", after, before)
	}
}

func TestRenameRouteTarget(t *testing.T) {
	ctx := context.Background()
	svc, _, ws := routingWS(t)
	path := routingtest.Write(t, ws.Path, "gmail", routing.File{Routes: []routing.Route{
		{ID: "rt_aaaaaaaaaa", Agent: "ops", Gets: routing.GetsOff},
		{ID: "rt_bbbbbbbbbb", Job: "ops/digest", Gets: routing.GetsOff},
		{ID: "rt_cccccccccc", Agent: "cmo", Gets: routing.GetsOff},
	}, Rules: []routing.Rule{{Field: routing.RuleSenderIs, Value: "a@b.c", To: "ops/digest"}}})
	before, _ := os.ReadFile(path)
	restore, err := svc.RenameRouteTarget(ctx, ws.ID, "ops", "ops2")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	for _, want := range []string{"agent: ops2", "job: ops2/digest", "to: ops2/digest", "agent: cmo"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("restore differs")
	}
	// A job rename touches only that job.
	if _, err := svc.RenameRouteTarget(ctx, ws.ID, "ops/digest", "ops/brief"); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if !strings.Contains(string(got), "job: ops/brief") || !strings.Contains(string(got), "agent: ops\n") {
		t.Fatalf("job rename:\n%s", got)
	}
}

// routingWS makes a workspace with the agents ops and cmo and the job
// ops/digest.
func routingWS(t *testing.T) (*workspaces.Service, *store.Store, *workspaces.Workspace) {
	t.Helper()
	st := openTest(t)
	svc := workspaces.New(st)
	root := t.TempDir()
	for _, a := range []string{"ops", "cmo"} {
		writeFile(t, filepath.Join(root, "agents", a, "AGENT.md"), "---\ndescription: Test agent.\nharness: fake\n---\n\nRun.\n")
	}
	writeFile(t, filepath.Join(root, "agents", "ops", "digest", "JOB.md"), "---\nharness: fake\n---\n\nDo work.\n")
	ws, err := svc.Add(context.Background(), root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RebuildIndex(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	return svc, st, ws
}

func routingUpdates(t *testing.T, st *store.Store) int {
	t.Helper()
	n := 0
	for _, topic := range busTopics(t, st, 0) {
		if strings.HasPrefix(topic, "routing.updated:") {
			n++
		}
	}
	return n
}

func str(s string) *string { return &s }
