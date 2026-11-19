package connectors_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
	"gopkg.in/yaml.v3"
)

// TestScopeForms proves the scope reads global or a list and checks values.
func TestScopeForms(t *testing.T) {
	t.Parallel()
	var c connectors.Connection
	if err := yaml.Unmarshal([]byte("scope: global\n"), &c); err != nil || !c.Scope.IsGlobal() {
		t.Fatalf("yaml global %v %v", c.Scope, err)
	}
	if err := yaml.Unmarshal([]byte("scope: [ws_a, ws_b]\n"), &c); err != nil || !reflect.DeepEqual(c.Scope, connectors.Scope{"ws_a", "ws_b"}) {
		t.Fatalf("yaml list %v %v", c.Scope, err)
	}
	out, err := yaml.Marshal(connectors.Connection{Scope: connectors.Scope{connectors.ScopeGlobal}})
	if err != nil || !strings.Contains(string(out), "scope: global\n") {
		t.Fatalf("yaml out %q %v", out, err)
	}
	var sc connectors.Scope
	if err := json.Unmarshal([]byte(`"global"`), &sc); err != nil || !sc.IsGlobal() {
		t.Fatalf("json global %v %v", sc, err)
	}
	if !reflect.DeepEqual(connectors.Scope(nil).Wire(), []string{"global"}) || !connectors.Scope(nil).Includes("ws_x") {
		t.Fatal("no scope is not global")
	}
	if (connectors.Scope{"ws_a"}).Includes("ws_b") || !(connectors.Scope{"ws_a"}).Includes("") {
		t.Fatal("list scope")
	}
	for _, bad := range []connectors.Scope{{"ops"}, {"global", "ws_a"}, {"ws_a", "ws_a"}} {
		e := inline("feed", func(m *manifest.Manifest) {})
		e.Scope = bad
		var ve *connectors.ValidationError
		if err := e.Validate(); !errors.As(err, &ve) || !slices.ContainsFunc(ve.Fields, func(f connectors.FieldError) bool { return f.Field == "scope" }) {
			t.Fatalf("scope %v: %v", bad, err)
		}
	}
}

// TestScopeHidesConnection proves a workspace outside the scope cannot see,
// run, trigger, or call the connection, and that global restores it.
func TestScopeHidesConnection(t *testing.T) {
	s := newScopeStack(t, "src")
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "op")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho {}\n"), 0o755); err != nil { //nolint:gosec // test program
		t.Fatal(err)
	}
	e := inline("src", func(m *manifest.Manifest) {
		m.Events = &manifest.Events{Command: "true", Format: manifest.FormatJSON}
		m.Actions = &manifest.Actions{
			CLI:        &manifest.CLIAction{Commands: []string{"src"}},
			Operations: []manifest.Operation{{Name: "rows", Label: "Rows", ReadOnly: true, Default: manifest.OpOn, Command: []string{bin}}},
		}
	})
	e.Scope = connectors.Scope{s.a.ID}
	c, err := s.svc.Create(ctx, e)
	if err != nil {
		t.Fatal(err)
	}

	if got := s.svc.ConnectionFor("src", s.b.ID); got != "" {
		t.Fatalf("out of scope connection %q", got)
	}
	if got := s.svc.ConnectionFor("src", s.a.ID); got != c.ID {
		t.Fatalf("in scope connection %q", got)
	}
	if _, err := s.svc.Resolve("src", s.b.ID); !errors.Is(err, connectors.ErrNotInScope) {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := s.svc.ResolveActions(ctx, s.b.ID, []string{"src"}); !errors.Is(err, connectors.ErrNotInScope) {
		t.Fatalf("job run actions: %v", err)
	}
	if _, err := s.svc.ResolveActions(ctx, s.a.ID, []string{"src"}); err != nil {
		t.Fatalf("job run actions in scope: %v", err)
	}
	row, err := s.svc.GetFor("src", s.b.ID)
	if err != nil || row["in_scope"] != false || row["disabled"] != nil || !reflect.DeepEqual(row["scope"], []string{s.a.ID}) {
		t.Fatalf("row %v %v", row, err)
	}
	if row, _ := s.svc.GetFor("src", s.a.ID); row["in_scope"] != true {
		t.Fatalf("row in scope %v", row)
	}

	// Operations: a live source and an operation call are refused.
	ops := connectors.NewOperations(s.svc, nil, nil)
	if _, err := ops.List(ctx, s.b.ID, "src"); !errors.Is(err, connectors.ErrNotInScope) {
		t.Fatalf("operation list: %v", err)
	}
	if _, err := ops.Call(ctx, connectors.CallOpts{Workspace: s.b.ID, Connection: "src", Operation: "rows"}); !errors.Is(err, connectors.ErrNotInScope) {
		t.Fatalf("operation call: %v", err)
	}
	if res, err := ops.Call(ctx, connectors.CallOpts{Workspace: s.a.ID, Connection: "src", Operation: "rows"}); err != nil {
		t.Fatalf("operation call in scope: %+v %v", res, err)
	}

	// Triggers: only the job of A gets the event.
	if _, err := s.svc.InjectEvent(ctx, "src", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if a, b := s.queued(t, s.a.ID), s.queued(t, s.b.ID); a != 1 || b != 0 {
		t.Fatalf("queued a=%d b=%d", a, b)
	}

	// An empty list is refused; global restores B.
	if _, err := s.svc.Patch(ctx, "src", connectors.ConnectionPatch{Scope: json.RawMessage(`[]`)}); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("empty scope: %v", err)
	}
	if _, err := s.svc.Patch(ctx, "src", connectors.ConnectionPatch{Scope: json.RawMessage(`"global"`)}); err != nil {
		t.Fatal(err)
	}
	if got := s.svc.ConnectionFor("src", s.b.ID); got != c.ID {
		t.Fatalf("global connection %q", got)
	}
	if conn, _ := s.svc.Lookup("src"); conn.Scope != nil {
		t.Fatalf("global stored as %v", conn.Scope)
	}
}

// TestScopeWebhookWorkspace proves a hosted webhook fires only the triggers
// of the workspace of its hook.
func TestScopeWebhookWorkspace(t *testing.T) {
	s := newScopeStack(t, "webhook")
	ctx := context.Background()
	if _, err := s.svc.InjectNative(ctx, "webhook", map[string]any{"workspace_id": s.a.ID}, connectors.NativeMeta{SourceID: "f1"}); err != nil {
		t.Fatal(err)
	}
	if a, b := s.queued(t, s.a.ID), s.queued(t, s.b.ID); a != 1 || b != 0 {
		t.Fatalf("queued a=%d b=%d", a, b)
	}
}

// TestScopeLoadsGlobal proves a connection file with no scope loads as
// global, so an older connection keeps its behavior.
func TestScopeLoadsGlobal(t *testing.T) {
	_, _, svc := openStack(t)
	c, err := svc.Create(context.Background(), eventsDef("feed", "echo").entry())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(connectors.NewFolder(c.ID).Dir(), "connection.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if _, ok := file["scope"]; ok {
		t.Fatalf("file has scope: %s", raw)
	}
	fresh := connectors.New(nil, nil, nil)
	if err := fresh.Load(); err != nil {
		t.Fatal(err)
	}
	row, err := fresh.GetFor("feed", "ws_any")
	if err != nil || row["in_scope"] != true || !reflect.DeepEqual(row["scope"], []string{"global"}) {
		t.Fatalf("row %v %v", row, err)
	}
}

// TestScopeDefault proves a connection with an account gets the scope of
// the workspace, and one without an account stays global.
func TestScopeDefault(t *testing.T) {
	_, _, _ = openStack(t)
	account := inline("chat", func(m *manifest.Manifest) {
		m.Setup = &manifest.Setup{Inputs: []manifest.Input{{Name: "TOKEN", Secret: true}}}
	})
	if got := account.WithDefaultScope("ws_a").Scope; !reflect.DeepEqual(got, connectors.Scope{"ws_a"}) {
		t.Fatalf("account scope %v", got)
	}
	if got := account.WithDefaultScope("").Scope; got != nil {
		t.Fatalf("no workspace scope %v", got)
	}
	account.Scope = connectors.Scope{connectors.ScopeGlobal}
	if got := account.WithDefaultScope("ws_a").Scope; !got.IsGlobal() {
		t.Fatalf("explicit scope changed %v", got)
	}
	plain := inline("feed", func(m *manifest.Manifest) {})
	if got := plain.WithDefaultScope("ws_a").Scope; got != nil {
		t.Fatalf("plain scope %v", got)
	}
}

// scopeStack has two workspaces, A and B, and one connection whose scope
// lists only A.
type scopeStack struct {
	st   *store.Store
	svc  *connectors.Service
	a, b workspaces.Workspace
}

func newScopeStack(t *testing.T, connection string) scopeStack {
	t.Helper()
	st, ws, svc := openStack(t)
	ctx := context.Background()
	add := func(name string) workspaces.Workspace {
		wsPath := filepath.Join(home.Base(), name)
		routeAgent(t, wsPath, "job", connection, job.TriggerYAML{Match: "true"})
		w, err := ws.Add(ctx, wsPath, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := ws.RebuildIndex(ctx, w); err != nil {
			t.Fatal(err)
		}
		return *w
	}
	return scopeStack{st: st, svc: svc, a: add("wsa"), b: add("wsb")}
}

// queued counts the queue rows of the job in a workspace.
func (s scopeStack) queued(t *testing.T, ws string) int {
	t.Helper()
	var n int
	if err := s.st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE job_id IN (SELECT id FROM job_index WHERE workspace_id=?)`, ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
