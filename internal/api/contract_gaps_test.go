package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestEventsJoinOutcomesFromQueue proves B15 read-time derivation.
func TestEventsJoinOutcomesFromQueue(t *testing.T) {
	_, base, _ := startAPI(t)
	client := &http.Client{Timeout: 5 * time.Second}
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "demo")
	wsID := addWorkspace(t, base, wsDir, "ws1")

	// rules/fire creates event + outcome + queue pending
	payload := `{"connector":"src","workspace":"` + wsID + `","job":"demo","match":"true","event":{"n":1}}`
	req, _ := http.NewRequest("POST", base+"/v1/rules/fire", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var fire map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&fire)
	resp.Body.Close()
	eid, _ := fire["event_id"].(string)
	qid, _ := fire["queue_id"].(string)
	if eid == "" || qid == "" {
		t.Fatalf("fire: %v", fire)
	}

	// GET event — outcomes with derived queued from pending queue
	code, got, _ := doJSON(t, "GET", base+"/v1/events/"+eid, nil)
	if code != 200 {
		t.Fatalf("get event %d %v", code, got)
	}
	outs, _ := got["outcomes"].([]any)
	if len(outs) < 1 {
		t.Fatalf("expected outcomes: %v", got)
	}
	o0, _ := outs[0].(map[string]any)
	if o0["outcome"] != "queued" {
		t.Fatalf("pending queue should derive outcome=queued, got %v", o0)
	}
	if o0["queue_id"] != qid {
		t.Fatalf("queue_id=%v want %s", o0["queue_id"], qid)
	}

	// Mark queue done with coalesced → outcome should become coalesced
	st := mustOpenStore(t, home.DBPath())
	defer st.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = st.DB().Exec(`UPDATE event_queue SET status='done', result=?, done_at=?, updated_at=? WHERE id=?`,
		string(dispatch.ResultCoalesced), now, now, qid)

	code, got, _ = doJSON(t, "GET", base+"/v1/events/"+eid, nil)
	if code != 200 {
		t.Fatal(code)
	}
	outs, _ = got["outcomes"].([]any)
	o0, _ = outs[0].(map[string]any)
	if o0["outcome"] != "coalesced" {
		t.Fatalf("done+coalesced should derive outcome=coalesced, got %v", o0)
	}

	// List includes derived outcome
	code, listBody, _ := doJSON(t, "GET", base+"/v1/events", nil)
	if code != 200 {
		t.Fatal(code)
	}
	evs, _ := listBody["events"].([]any)
	found := false
	for _, raw := range evs {
		m, _ := raw.(map[string]any)
		if m["id"] == eid && m["outcome"] == "coalesced" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("list missing coalesced row: %v", listBody)
	}
}

// TestDryRunEvaluatesWhenGate proves B25 dry_run path.
func TestDryRunEvaluatesWhenGate(t *testing.T) {
	s, base, dir := startAPI(t)
	wsPath := filepath.Join(dir, "ws")
	jobDir := filepath.Join(wsPath, "gated")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := `---
harness: fake
triggers:
  - every: 1h
    when:
      run: "echo hello"
      match: "event.stdout.includes('hello')"
---

Do it.
`
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	code, ws, _ := doJSON(t, "POST", base+"/v1/workspaces", map[string]string{
		"path": wsPath, "name": "dry-ws",
	})
	if code != 201 {
		t.Fatalf("ws %d %v", code, ws)
	}
	wsID, _ := ws["id"].(string)

	code, body, _ := doJSON(t, "POST", base+"/v1/jobs/gated/run", map[string]any{
		"workspace": wsID, "dry_run": true,
	})
	if code != 200 {
		t.Fatalf("dry_run status %d %v", code, body)
	}
	if body["dry_run"] != true {
		t.Fatalf("%v", body)
	}
	if body["fire"] != true {
		t.Fatalf("expected fire=true: %v", body)
	}
	when, _ := body["when"].([]any)
	if len(when) < 1 {
		t.Fatalf("expected when verdicts: %v", body)
	}

	// Skip path
	md2 := `---
harness: fake
triggers:
  - every: 1h
    when:
      run: "echo no"
      match: "event.stdout.includes('yes')"
---

Skip.
`
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md2), 0o644); err != nil {
		t.Fatal(err)
	}
	// rebuild index after file rewrite
	wsObj, err := s.Workspaces().Get(t.Context(), wsID)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Workspaces().RebuildIndex(t.Context(), wsObj)

	code, body, _ = doJSON(t, "POST", base+"/v1/jobs/gated/run", map[string]any{
		"workspace": wsID, "dry_run": true,
	})
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if body["fire"] != false {
		t.Fatalf("expected fire=false: %v", body)
	}
}

// TestJobMarkdownCreatePutGetPatch covers D1 job edit surface.
func TestJobMarkdownCreatePutGetPatch(t *testing.T) {
	_, base, dir := startAPI(t)
	wsPath := filepath.Join(dir, "ws")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	code, ws, _ := doJSON(t, "POST", base+"/v1/workspaces", map[string]string{
		"path": wsPath, "name": "edit-ws",
	})
	if code != 201 {
		t.Fatalf("%d %v", code, ws)
	}
	wsID, _ := ws["id"].(string)

	// Create with markdown field (not content)
	md := "---\nharness: fake\ntitle: Hello\n---\n\nOriginal body.\n"
	code, created, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "name": "editme", "markdown": md,
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	onDisk, err := os.ReadFile(filepath.Join(wsPath, "editme", "JOB.md"))
	if err != nil || !strings.Contains(string(onDisk), "Original body") {
		t.Fatalf("disk: %v %s", err, onDisk)
	}

	// GET returns markdown + mtime + hash
	code, got, _ := doJSON(t, "GET", base+"/v1/jobs/editme?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, got)
	}
	if got["markdown"] == nil || !strings.Contains(fmt.Sprint(got["markdown"]), "Original body") {
		t.Fatalf("missing markdown: %v", got)
	}
	if got["mtime"] == nil || got["mtime"] == "" {
		t.Fatalf("missing mtime: %v", got)
	}
	hash, _ := got["hash"].(string)
	if hash == "" {
		t.Fatal("empty hash")
	}

	// PUT without markdown must 400 (not wipe)
	req, _ := http.NewRequest("PUT", base+"/v1/jobs/editme?workspace="+wsID,
		bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", hash)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("empty put want 400 got %d", resp.StatusCode)
	}
	resp.Body.Close()
	// file intact
	onDisk, _ = os.ReadFile(filepath.Join(wsPath, "editme", "JOB.md"))
	if !strings.Contains(string(onDisk), "Original body") {
		t.Fatalf("file wiped: %s", onDisk)
	}

	// PUT with markdown
	newMD := "---\nharness: fake\ntitle: Updated\n---\n\nNew body.\n"
	req, _ = http.NewRequest("PUT", base+"/v1/jobs/editme?workspace="+wsID,
		bytes.NewBufferString(`{"markdown":`+mustJSON(newMD)+`}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", hash)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("put %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	onDisk, _ = os.ReadFile(filepath.Join(wsPath, "editme", "JOB.md"))
	if !strings.Contains(string(onDisk), "New body") {
		t.Fatalf("put failed: %s", onDisk)
	}

	// PATCH harness + session.if_running
	code, got, _ = doJSON(t, "GET", base+"/v1/jobs/editme?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("reget %d %v", code, got)
	}
	hash, _ = got["hash"].(string)
	req, _ = http.NewRequest("PATCH", base+"/v1/jobs/editme?workspace="+wsID,
		bytes.NewBufferString(`{"harness":"claude","session":{"if_running":"replace"},"disabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", hash)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	onDisk, _ = os.ReadFile(filepath.Join(wsPath, "editme", "JOB.md"))
	if !strings.Contains(string(onDisk), "harness: claude") {
		t.Fatalf("patch harness: %s", onDisk)
	}
	if !strings.Contains(string(onDisk), "if_running: replace") {
		t.Fatalf("patch session: %s", onDisk)
	}
	// Re-parse
	j, err := job.ParseFile(filepath.Join(wsPath, "editme", "JOB.md"))
	if err != nil || j.Harness != "claude" {
		t.Fatalf("parse after patch: %v %+v", err, j)
	}
	if j.Session.IfRunning != "replace" {
		t.Fatalf("session.if_running=%q", j.Session.IfRunning)
	}
}

// TestRetryPolicyByEndReason covers D7 dispatch.
func TestRetryPolicyByEndReason(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "x")
	pathY := writeJobFile(t, wsDir, "y")
	wsID := addWorkspace(t, base, wsDir, "retry-ws")

	// Create completed fake run then force end_reason
	code, run, _ := doJSON(t, "POST", base+"/v1/jobs/x/run", map[string]any{
		"workspace": wsID, "harness": "fake", "prompt_override": "hi",
	})
	if code != 201 {
		t.Fatalf("%d %v", code, run)
	}
	id, _ := run["id"].(string)
	st := mustOpenStore(t, home.DBPath())
	defer st.Close()

	// auth → park
	_, _ = st.DB().Exec(`UPDATE runs SET status='failed', end_reason='auth' WHERE id=?`, id)
	code, body, _ := doJSON(t, "POST", base+"/v1/runs/"+id+"/retry", nil)
	if code != 200 {
		t.Fatalf("retry auth %d %v", code, body)
	}
	if body["action"] != "parked" {
		t.Fatalf("want parked: %v", body)
	}
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue WHERE resume_of=? AND status='blocked'`, id).Scan(&n)
	if n < 1 {
		t.Fatal("expected blocked resume intent")
	}

	// session_gone → fresh rerun (creates new run)
	code, run2, _ := doJSON(t, "POST", base+"/v1/jobs/y/run", map[string]any{
		"workspace": wsID, "harness": "fake", "prompt_override": "hi2",
	})
	if code != 201 {
		t.Fatal(code)
	}
	id2, _ := run2["id"].(string)
	_, _ = st.DB().Exec(`UPDATE runs SET status='failed', end_reason='session_gone' WHERE id=?`, id2)
	code, body, _ = doJSON(t, "POST", base+"/v1/runs/"+id2+"/retry", nil)
	if code != 201 {
		t.Fatalf("retry session_gone %d %v", code, body)
	}

	// session_gone with the job file gone → 404, never the preview.
	if err := os.Remove(pathY); err != nil {
		t.Fatal(err)
	}
	_, _ = st.DB().Exec(`UPDATE runs SET status='failed', end_reason='session_gone' WHERE id=?`, id2)
	code, body, _ = doJSON(t, "POST", base+"/v1/runs/"+id2+"/retry", nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("retry gone job: want 404 not_found got %d %v", code, body)
	}
}

// TestRetryReopensOrReruns: a retry with a session reopens the run as its
// next turn; without a session it makes a new run that follows the old one.
func TestRetryReopensOrReruns(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "x")
	wsID := addWorkspace(t, base, wsDir, "retry-turn-ws")
	st := mustOpenStore(t, home.DBPath())
	defer st.Close()
	failed := func(session string) string {
		t.Helper()
		code, run, _ := doJSON(t, "POST", base+"/v1/jobs/x/run", map[string]any{
			"workspace": wsID, "harness": "fake", "prompt_override": "hi",
		})
		if code != 201 {
			t.Fatalf("launch %d %v", code, run)
		}
		id, _ := run["id"].(string)
		waitFor(t, 10*time.Second, "run ends", func() bool {
			_, got, _ := doJSON(t, "GET", base+"/v1/runs/"+id, nil)
			return got["status"] != "running"
		})
		if _, err := st.DB().Exec(`UPDATE runs SET status='failed', end_reason='failed', session_id=? WHERE id=?`, store.NullStr(session), id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	id := failed("sess-1")
	code, body, _ := doJSON(t, "POST", base+"/v1/runs/"+id+"/retry", nil)
	run, _ := body["run"].(map[string]any)
	if code != 201 || body["action"] != "resume" || run["id"] != id || run["turn_count"] != float64(2) {
		t.Fatalf("retry with session %d %v", code, body)
	}

	id = failed("")
	code, body, _ = doJSON(t, "POST", base+"/v1/runs/"+id+"/retry", nil)
	run, _ = body["run"].(map[string]any)
	if code != 201 || body["action"] != "rerun_with_note" || run["id"] == id || run["previous_run_id"] != id || run["turn_count"] != float64(1) {
		t.Fatalf("retry without session %d %v", code, body)
	}
}

// TestDrainFlagPersistsAcrossRestart proves the launch gate honors the
// persisted drain flag when in-memory drain is off (API restart survival).
func TestDrainFlagPersistsAcrossRestart(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "dj")
	wsID := addWorkspace(t, base, wsDir, "drain-ws")

	st := mustOpenStore(t, home.DBPath())
	defer st.Close()
	if err := st.SetFlag("drain", "1"); err != nil {
		t.Fatal(err)
	}

	// Auto producer blocked by the persisted flag alone.
	code, body, _ := doJSON(t, "POST", base+"/v1/jobs/dj/run", map[string]any{
		"workspace": wsID, "reason": "trigger",
	})
	if code != 503 || errCode(body) != "draining" {
		t.Fatalf("trigger under flag: want 503 draining got %d %v", code, body)
	}

	// Manual carve-out still launches.
	code, body, _ = doJSON(t, "POST", base+"/v1/jobs/dj/run", map[string]any{
		"workspace": wsID, "reason": "manual", "harness": "fake",
	})
	if code != 201 {
		t.Fatalf("manual under flag: want 201 got %d %v", code, body)
	}
}

func TestWorkspaceRenameAtomicWithBus(t *testing.T) {
	_, base, dir := startAPI(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "orig")
	code, body, _ := doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID, map[string]any{"name": "renamed"})
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if fmt.Sprint(body["name"]) != "renamed" {
		t.Fatalf("name %v", body["name"])
	}
	rev := int64(0)
	switch n := body["revision"].(type) {
	case float64:
		rev = int64(n)
	case json.Number:
		rev, _ = n.Int64()
	}
	if rev < 2 {
		t.Fatalf("revision %v want >=2", body["revision"])
	}
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	defer st.Close()
	var dbRev int64
	var dbName string
	if err := st.DB().QueryRow(`SELECT name, revision FROM workspaces WHERE id=?`, wsID).Scan(&dbName, &dbRev); err != nil {
		t.Fatal(err)
	}
	if dbName != "renamed" || dbRev != rev {
		t.Fatalf("db name=%s rev=%d", dbName, dbRev)
	}
	evs, err := st.ListBus(t.Context(), 0, []string{"workspace.updated"}, 10)
	if err != nil || len(evs) < 1 {
		t.Fatalf("bus %d %v", len(evs), err)
	}
	last := evs[len(evs)-1]
	if last.Revision != rev || last.ID != wsID {
		t.Fatalf("frame %+v want rev=%d id=%s", last, rev, wsID)
	}
}

func TestListRoutesCursorAndLimit(t *testing.T) {
	_, base, dir := startAPI(t)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	defer st.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ctx := t.Context()
	for i := 0; i < 5; i++ {
		id := ulid.Make().String()
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at)
			VALUES (?,?,?,?,?,?)`, id, "ws", fmt.Sprintf("k%d", i), "t", now, now); err != nil {
			t.Fatal(err)
		}
		eid := ulid.Make().String()
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO events (id, connector, received_at, summary, trace_id)
			VALUES (?,?,?,?,?)`, eid, "src", now, "s", eid); err != nil {
			t.Fatal(err)
		}
	}
	q := dispatch.NewQueue(st)
	for i := 0; i < 5; i++ {
		if err := q.Enqueue(ctx, &dispatch.QueueItem{Connector: "src", WorkspaceID: "ws", JobID: "j", JobName: "j"}); err != nil {
			t.Fatal(err)
		}
	}

	assertPage := func(path, key string) {
		t.Helper()
		code, body, _ := doJSON(t, "GET", base+path+"?limit=2", nil)
		if code != 200 {
			t.Fatalf("%s %d %v", path, code, body)
		}
		items, _ := body[key].([]any)
		if len(items) != 2 {
			t.Fatalf("%s page size %d want 2 (%v)", path, len(items), body)
		}
		next, _ := body["next_cursor"].(string)
		if next == "" {
			t.Fatalf("%s missing next_cursor", path)
		}
		code, body, _ = doJSON(t, "GET", base+path+"?limit=2&cursor="+next, nil)
		if code != 200 {
			t.Fatalf("%s page2 %d %v", path, code, body)
		}
		code, body, _ = doJSON(t, "GET", base+path+"?cursor=@@bad@@", nil)
		if code != 400 || errCode(body) != "validation" {
			t.Fatalf("%s bad cursor %d %v", path, code, body)
		}
	}
	assertPage("/v1/conversations", "conversations")
	assertPage("/v1/events", "events")
	assertPage("/v1/queue", "items")

	code, body, _ := doJSON(t, "GET", base+"/v1/skills?limit=1", nil)
	if code != 200 {
		t.Fatalf("skills %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/approvals?cursor=@@bad@@", nil)
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("approvals bad cursor %d %v", code, body)
	}
}

func startAPI(t *testing.T) (*api.Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, api.Options{})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	return s, "http://" + addr, dir
}

func doJSON(t *testing.T, method, url string, body any) (int, map[string]any, http.Header) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, resp.Header
}

func mustOpenStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// indexedJobID returns the static ID of the live indexed job or agent name.
func indexedJobID(t *testing.T, st *store.Store, wsID, name string) string {
	t.Helper()
	var id string
	if err := st.DB().QueryRow(`SELECT id FROM job_index WHERE workspace_id=? AND name=? AND retired_at IS NULL`,
		wsID, name).Scan(&id); err != nil {
		t.Fatalf("job_index %s/%s: %v", wsID, name, err)
	}
	return id
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
