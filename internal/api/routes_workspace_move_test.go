package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/moves"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
)

func TestWorkspaceMoveToMachine(t *testing.T) {
	g := newMoveRig(t, false)

	// The confirm gate and the target checks come first.
	if code, body, _ := doJSON(t, "POST", g.srcBase+"/v1/workspaces/"+g.wsID+"/move", map[string]any{"machine": "Studio"}); code != 400 || !strings.Contains(body["error"].(map[string]any)["message"].(string), "Studio") {
		t.Fatalf("confirm gate: %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "POST", g.srcBase+"/v1/workspaces/"+g.wsID+"/move?confirm=true", map[string]any{"machine": "self"}); code != 409 {
		t.Fatalf("self: %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "POST", g.srcBase+"/v1/workspaces/"+g.wsID+"/move?confirm=true", map[string]any{"machine": "Nope"}); code != 404 {
		t.Fatalf("unknown machine: %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "POST", g.tgtBase+"/v1/workspaces/import", nil); code != 403 {
		t.Fatalf("import from a person: %d %v", code, body)
	}

	mv := g.move(t)
	if mv["phase"] != moves.PhaseDone || mv["restored"] != true {
		t.Fatalf("move: %v", mv)
	}

	// Target: same id, new path under its root, rows and files there.
	code, ws, _ := doJSON(t, "GET", g.tgtBase+"/v1/workspaces/"+g.wsID, nil)
	newPath, _ := ws["path"].(string)
	if code != 200 || !strings.HasPrefix(newPath, g.tgtRoot) || ws["archived_at"] != nil || ws["move_id"] != nil {
		t.Fatalf("target workspace: %d %v", code, ws)
	}
	job, err := os.ReadFile(filepath.Join(newPath, "agents", "ops", "demo", "JOB.md"))
	if err != nil || !strings.Contains(string(job), newPath) || strings.Contains(string(job), g.wsDir) {
		t.Fatalf("job path not rewritten: %v %s", err, job)
	}
	if b, err := os.ReadFile(filepath.Join(newPath, "agents", "ops", "demo", "MEMORY.md")); err != nil || !strings.Contains(string(b), "moved memory") {
		t.Fatalf("memory: %v %s", err, b)
	}
	if _, err := os.Stat(filepath.Join(g.tgtHome, "logs", "runs", g.runID+".log")); err != nil {
		t.Fatalf("run log: %v", err)
	}
	if n := g.count(t, g.tgtSt, `SELECT COUNT(*) FROM runs WHERE id = ? AND workspace_id = ?`, g.runID, g.wsID); n != 1 {
		t.Fatalf("run row: %d", n)
	}
	if n := g.count(t, g.tgtSt, `SELECT COUNT(*) FROM inbox_items WHERE id = 'inb_move' AND workspace_id = ?`, g.wsID); n != 1 {
		t.Fatalf("inbox row: %d", n)
	}
	if v, err := g.tgtSecrets.Get("token@" + g.wsID); err != nil || v != "fake-connector-token" {
		t.Fatalf("connector secret not copied: %v", err)
	}
	if v, err := g.tgtSecrets.Get("api_key"); err != nil || v != "fake-target-key" {
		t.Fatalf("target secret overwritten: %v", err)
	}
	code, run, _ := doJSON(t, "POST", g.tgtBase+"/v1/jobs/agents%2Fops%2Fdemo/run", map[string]any{"workspace": g.wsID, "harness": "fake", "prompt_override": "hi"})
	if code != 201 || run["status"] != "complete" {
		t.Fatalf("target run: %d %v", code, run)
	}

	// Source: archived, read-only, cloud rows moved.
	_, ws, _ = doJSON(t, "GET", g.srcBase+"/v1/workspaces/"+g.wsID, nil)
	if ws["archived_at"] == nil || ws["moved_to"].(map[string]any)["name"] != "Studio" {
		t.Fatalf("source not archived: %v", ws)
	}
	code, body, _ := doJSON(t, "POST", g.srcBase+"/v1/jobs/agents%2Fops%2Fdemo/run", map[string]any{"workspace": g.wsID, "harness": "fake", "prompt_override": "hi"})
	if code != 409 || errCode(body) != "archived" || !strings.Contains(body["error"].(map[string]any)["message"].(string), "Studio") {
		t.Fatalf("archived launch: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PATCH", g.srcBase+"/v1/workspaces/"+g.wsID, map[string]any{"name": "x"})
	if code != 409 || errCode(body) != "archived" {
		t.Fatalf("archived patch: %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "POST", g.srcBase+"/v1/workspaces/"+g.wsID+"/move?confirm=true", map[string]any{"machine": "Studio"}); code != 409 || errCode(body) != "archived" {
		t.Fatalf("second move: %d %v", code, body)
	}
	if len(g.cloud.calls) != 1 || g.cloud.calls[0].DeviceID != "dev_tgt" {
		t.Fatalf("cloud calls: %v", g.cloud.calls)
	}
	if n := g.count(t, g.srcSt, `SELECT COUNT(*) FROM event_log WHERE topic = 'workspace.move'`); n < 4 {
		t.Fatalf("workspace.move events: %d", n)
	}
}

func TestWorkspaceMoveFailedRestore(t *testing.T) {
	g := newMoveRig(t, true)
	mv := g.move(t)
	if mv["phase"] != moves.PhaseFailed || mv["restored"] != false || mv["error"] == "" {
		t.Fatalf("move: %v", mv)
	}
	_, ws, _ := doJSON(t, "GET", g.srcBase+"/v1/workspaces/"+g.wsID, nil)
	if ws["archived_at"] != nil || ws["move_id"] != mv["id"] {
		t.Fatalf("source archived after a failed restore, or no move id: %v", ws)
	}
	if code, run, _ := doJSON(t, "POST", g.srcBase+"/v1/jobs/agents%2Fops%2Fdemo/run", map[string]any{"workspace": g.wsID, "harness": "fake", "prompt_override": "hi"}); code != 201 {
		t.Fatalf("source launch: %d %v", code, run)
	}
	if code, _, _ := doJSON(t, "GET", g.tgtBase+"/v1/workspaces/"+g.wsID, nil); code != 404 {
		t.Fatalf("target kept the workspace: %d", code)
	}
	if ents, _ := os.ReadDir(g.tgtRoot); len(ents) != 0 {
		t.Fatalf("target kept files: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(g.tgtHome, "logs", "runs", g.runID+".log")); !os.IsNotExist(err) {
		t.Fatalf("target kept the run log: %v", err)
	}
	if g.tgtSecrets.Has("token@" + g.wsID) {
		t.Fatal("target kept the connector secret")
	}
	for _, tbl := range []string{"runs", "inbox_items", "job_index"} {
		if n := g.count(t, g.tgtSt, `SELECT COUNT(*) FROM `+tbl+` WHERE workspace_id = ?`, g.wsID); n != 0 {
			t.Fatalf("target kept %s rows: %d", tbl, n)
		}
	}
	if len(g.cloud.calls) != 0 {
		t.Fatalf("cloud called: %v", g.cloud.calls)
	}
}

func TestWorkspaceMoveCloudRetry(t *testing.T) {
	g := newMoveRig(t, false)
	g.cloud.fail = true
	mv := g.move(t)
	if mv["phase"] != moves.PhaseFailed || mv["restored"] != true {
		t.Fatalf("move: %v", mv)
	}
	_, ws, _ := doJSON(t, "GET", g.srcBase+"/v1/workspaces/"+g.wsID, nil)
	if ws["archived_at"] == nil || ws["move_id"] != mv["id"] {
		t.Fatalf("source not archived after a restore, or no move id: %v", ws)
	}
	g.cloud.mu.Lock()
	g.cloud.fail = false
	g.cloud.mu.Unlock()
	again := g.move(t)
	if again["phase"] != moves.PhaseDone || again["id"] != mv["id"] {
		t.Fatalf("retry: %v (first %v)", again, mv)
	}
	if len(g.cloud.calls) != 2 {
		t.Fatalf("cloud calls: %d", len(g.cloud.calls))
	}
}

type fakeMoveCloud struct {
	mu    sync.Mutex
	calls []cloudlink.WorkspaceMoveRequest
	fail  bool
}

func (c *fakeMoveCloud) MoveWorkspace(_ context.Context, wsID string, req cloudlink.WorkspaceMoveRequest) (*cloudlink.WorkspaceMoveResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, req)
	if c.fail {
		return nil, errors.New("cloud down")
	}
	return &cloudlink.WorkspaceMoveResult{WorkspaceID: wsID, DeviceID: req.DeviceID, Index: 1}, nil
}

// peerTransport serves a request with the handler of another server, as
// the relay does: the actor is the sending machine.
type peerTransport struct{ h http.Handler }

func (p peerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	actor := tools.Actor{Kind: tools.ActorMobile, Name: "machine:src", Scopes: []string{"full"}}
	p.h.ServeHTTP(rec, r.WithContext(tools.WithActor(r.Context(), actor)))
	return rec.Result(), nil
}

type moveRig struct {
	srcBase, tgtBase string
	srcSt, tgtSt     *store.Store
	tgtHome, tgtRoot string
	tgtSecrets       secrets.Dir
	cloud            *fakeMoveCloud
	wsID, wsDir      string
	runID            string
}

// newMoveRig starts a source and a target server. The source has a
// workspace with a job, a memory file, a finished run, an inbox item, a
// referenced secret, and a workspace connector secret.
func newMoveRig(t *testing.T, failRestore bool) *moveRig {
	t.Helper()
	g := &moveRig{cloud: &fakeMoveCloud{}}
	src, srcBase := startTestServerSeeded(t, api.Options{}, func(st *store.Store) {
		g.srcSt = st
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := st.DB().Exec(`INSERT INTO machines (id, name, device_id, is_self, created_at, updated_at) VALUES ('mch_tgt', 'Studio', 'dev_tgt', 0, ?, ?)`, now, now); err != nil {
			t.Fatal(err)
		}
	})
	g.srcBase = srcBase

	g.tgtHome = t.TempDir()
	g.tgtRoot = filepath.Join(g.tgtHome, "workspaces")
	g.tgtSecrets = secrets.Dir(filepath.Join(g.tgtHome, "secrets"))
	tst, err := store.OpenPath(filepath.Join(g.tgtHome, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tst.Close() })
	g.tgtSt = tst
	tgt := api.New(tst, api.Options{WorkspacesRoot: g.tgtRoot, LookupLatest: func() (string, string, string, error) { return api.Version, "", "", nil }})
	tgt.SetMoves(moves.Options{Home: g.tgtHome, Secrets: g.tgtSecrets, FailRestore: failRestore})
	addr, err := tgt.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tgt.Shutdown(t.Context()) })
	g.tgtBase = "http://" + addr

	src.SetMoves(moves.Options{
		Dial: func(context.Context, string) (http.RoundTripper, error) {
			return peerTransport{h: tgt.Root()}, nil
		},
		Cloud: g.cloud,
	})

	g.wsDir, err = filepath.EvalSymlinks(t.TempDir()) // the registry keeps the real path
	if err != nil {
		t.Fatal(err)
	}
	writeJobFile(t, g.wsDir, "agents/ops/demo")
	job := "---\nharness: fake\nenv:\n  API_KEY: secret:api_key\n---\n\nRead " + g.wsDir + "/notes.txt\n"
	if err := os.WriteFile(filepath.Join(g.wsDir, "agents", "ops", "demo", "JOB.md"), []byte(job), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.wsDir, "agents", "ops", "demo", "MEMORY.md"), []byte("## Context\n\nmoved memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.wsID = addWorkspace(t, srcBase, g.wsDir, "move-ws")
	if err := secrets.Set("api_key", "fake-source-key"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Set("token@"+g.wsID, "fake-connector-token"); err != nil {
		t.Fatal(err)
	}
	if err := g.tgtSecrets.Set("api_key", "fake-target-key"); err != nil {
		t.Fatal(err)
	}

	code, run, _ := doJSON(t, "POST", srcBase+"/v1/jobs/agents%2Fops%2Fdemo/run", map[string]any{"workspace": g.wsID, "harness": "fake", "prompt_override": "hi"})
	if code != 201 || run["status"] != "complete" {
		t.Fatalf("source run: %d %v", code, run)
	}
	g.runID, _ = run["id"].(string)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := g.srcSt.DB().Exec(`INSERT INTO inbox_items (id, kind, title, workspace_id, run_id, created_at, updated_at) VALUES ('inb_move', 'alert', 'Moved item', ?, ?, ?, ?)`, g.wsID, g.runID, now, now); err != nil {
		t.Fatal(err)
	}
	return g
}

// move starts a move and waits for its last phase.
func (g *moveRig) move(t *testing.T) map[string]any {
	t.Helper()
	code, mv, _ := doJSON(t, "POST", g.srcBase+"/v1/workspaces/"+g.wsID+"/move?confirm=true", map[string]any{"machine": "Studio"})
	if code != 202 {
		t.Fatalf("move: %d %v", code, mv)
	}
	id, _ := mv["id"].(string)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, got, _ := doJSON(t, "GET", g.srcBase+"/v1/workspaces/"+g.wsID+"/moves/"+id, nil)
		if p := got["phase"]; p == moves.PhaseDone || p == moves.PhaseFailed {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("move %s did not finish", id)
	return nil
}

func (g *moveRig) count(t *testing.T, st *store.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
