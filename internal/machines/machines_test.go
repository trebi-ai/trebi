package machines

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func testHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("tools:\n  claude: {enabled: true}\n  codex: {enabled: true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func busCount(t *testing.T, st *store.Store, topic string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic = ?`, topic).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type fakeLink struct{ id, name string }

func (f *fakeLink) DeviceID() string   { return f.id }
func (f *fakeLink) DeviceName() string { return f.name }

// fakeLocator finds the named harnesses.
type fakeLocator map[string]bool

func (l fakeLocator) Locate(name string) string {
	if l[name] {
		return "/bin/" + name
	}
	return ""
}

func (fakeLocator) TerminalLogin(string) bool { return false }

type fakeAuth map[string]harness.State

func (a fakeAuth) States(context.Context) (map[string]harness.State, error) { return a, nil }

func authState(ok int64) harness.State {
	return harness.State{AuthOK: sql.NullInt64{Int64: ok, Valid: true}}
}

// fakeConns resolves connections from a table: an error, or a resolved row.
type fakeConns struct {
	rows []map[string]any
	res  map[string]connectors.Resolved
	errs map[string]error
}

func (f fakeConns) List(string) []map[string]any { return f.rows }

func (f fakeConns) Resolve(ref, _ string) (connectors.Resolved, error) {
	if err := f.errs[ref]; err != nil {
		return connectors.Resolved{}, err
	}
	if r, ok := f.res[ref]; ok {
		return r, nil
	}
	return connectors.Resolved{}, connectors.ErrNotFound
}

func TestSelfRow(t *testing.T) {
	testHome(t)
	ctx := context.Background()
	st := openStore(t)
	link := &fakeLink{}
	svc := New(st, nil, nil, nil, link)

	ws, err := workspaces.New(st).Add(ctx, t.TempDir(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
	self := list[0]
	if self.Name != SelfName || !self.IsSelf || self.DeviceID != nil || self.LastSeen == nil || self.OS != runtime.GOOS || self.Arch != runtime.GOARCH || self.Revision != 1 {
		t.Fatalf("self = %+v", self)
	}
	if _, err := svc.List(ctx); err != nil {
		t.Fatal(err)
	}
	if n := busCount(t, st, "machine.created"); n != 1 {
		t.Fatalf("machine.created = %d, want 1", n)
	}

	// A workspace made before the self row reads as on the self machine.
	got, err := workspaces.New(st).Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Machine == nil || got.Machine.ID != self.ID || got.Machine.Name != SelfName || !got.Machine.IsSelf {
		t.Fatalf("workspace machine = %+v", got.Machine)
	}

	link.id, link.name = "dev_123", "Studio Mac"
	linked, err := svc.Get(ctx, SelfRef)
	if err != nil {
		t.Fatal(err)
	}
	if linked.ID != self.ID || linked.Name != "Studio Mac" || linked.DeviceID == nil || *linked.DeviceID != "dev_123" || linked.Revision != 2 {
		t.Fatalf("linked self = %+v", linked)
	}
	if n := busCount(t, st, "machine.updated"); n != 1 {
		t.Fatalf("machine.updated = %d, want 1", n)
	}

	link.id, link.name = "", ""
	if m, err := svc.Get(ctx, self.ID); err != nil || m.Name != SelfName || m.DeviceID != nil {
		t.Fatalf("unlinked self = %+v, %v", m, err)
	}
	if _, err := svc.Get(ctx, "mch_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing machine err = %v", err)
	}
}

func TestCapabilities(t *testing.T) {
	testHome(t)
	ctx := context.Background()
	st := openStore(t)
	conns := fakeConns{
		rows: []map[string]any{
			{"id": "con_b", "slug": "slack", "type": "slack", "title": "Slack", "version": "1.4.0", "provenance": "managed", "login": map[string]any{"state": "auth_required"}},
			{"id": "con_a", "slug": "github", "type": "github", "title": "GitHub", "version": "2.0.0", "provenance": "managed"},
			{"id": "mcp_x", "slug": "files", "provenance": "inherited"},
			{"id": "con_c", "slug": "", "provenance": "managed", "load_error": "bad manifest"},
		},
		res: map[string]connectors.Resolved{"con_b": {ID: "con_b"}, "con_a": {ID: "con_a"}},
	}
	svc := New(st, fakeAuth{"claude": authState(1), "codex": authState(0)}, fakeLocator{"claude": true, "codex": true}, conns, nil).WithVersion("v1.2.3")
	caps, err := svc.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, h := range caps.Harnesses {
		states[h.Name] = h.State
	}
	if states["claude"] != StateReady || states["codex"] != StateLoggedOut {
		t.Fatalf("harness states = %v", states)
	}
	for name, st := range states {
		if name != "claude" && name != "codex" && st != StateNotInstalled {
			t.Errorf("harness %s state = %s", name, st)
		}
	}
	if len(caps.Connectors) != 3 || caps.Connectors[0].ID != "con_c" || caps.Connectors[0].State != StateError ||
		caps.Connectors[1].Name != "github" || caps.Connectors[1].State != StateReady ||
		caps.Connectors[2].Name != "slack" || caps.Connectors[2].State != StateLoggedOut {
		t.Fatalf("connectors = %+v", caps.Connectors)
	}
	cloud, err := svc.CloudCapabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cloud.Version != "v1.2.3" || cloud.OS != runtime.GOOS || len(cloud.Connectors) != 3 || cloud.Connectors[2].Type != "slack" {
		t.Fatalf("cloud capabilities = %+v", cloud)
	}
}
