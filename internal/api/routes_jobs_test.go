package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestJobsListByWorkspaceName(t *testing.T) {
	_, base, _ := startAPI(t)
	ops := t.TempDir()
	staging := t.TempDir()
	writeJobFile(t, ops, "report")
	writeJobFile(t, staging, "report")
	_ = addWorkspace(t, base, ops, "ops")
	stID := addWorkspace(t, base, staging, "staging")

	code, body, _ := doJSON(t, "GET", base+"/v1/jobs?workspace=staging", nil)
	if code != 200 {
		t.Fatalf("list by name %d %v", code, body)
	}
	jobs, _ := body["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("jobs=%v", body)
	}
	row, _ := jobs[0].(map[string]any)
	if row["workspace_id"] != stID || row["name"] != "report" {
		t.Fatalf("row=%v want workspace %s", row, stID)
	}
}

// TestJobDeleteAmbiguousAcrossWorkspaces covers A4/B4: a duplicate job name
// without ?workspace= must 400, never delete the first match.
func TestJobDeleteAmbiguousAcrossWorkspaces(t *testing.T) {
	_, base, _ := startAPI(t)
	ws1 := t.TempDir()
	ws2 := t.TempDir()
	p1 := writeJobFile(t, ws1, "dup")
	p2 := writeJobFile(t, ws2, "dup")
	id1 := addWorkspace(t, base, ws1, "amb-a")
	_ = addWorkspace(t, base, ws2, "amb-b")

	code, body, _ := doJSON(t, "DELETE", base+"/v1/jobs/dup?confirm=1", nil)
	if code != 400 || errCode(body) != "ambiguous" {
		t.Fatalf("want 400 ambiguous, got %d %v", code, body)
	}
	if _, err := os.Stat(p1); err != nil {
		t.Fatalf("ws1 job removed on ambiguous delete: %v", err)
	}
	if _, err := os.Stat(p2); err != nil {
		t.Fatalf("ws2 job removed on ambiguous delete: %v", err)
	}

	code, body, _ = doJSON(t, "DELETE", base+"/v1/jobs/dup?workspace="+id1+"&confirm=1", nil)
	if code != 200 {
		t.Fatalf("scoped delete %d %v", code, body)
	}
	if _, err := os.Stat(p1); !os.IsNotExist(err) {
		t.Fatalf("ws1 job still present: %v", err)
	}
	if _, err := os.Stat(p2); err != nil {
		t.Fatalf("ws2 job removed by scoped delete: %v", err)
	}
}

// TestJobIfMatchPrecondition covers B2/D1 on job PUT.
func TestJobIfMatchPrecondition(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "ifm-ws")

	md := "---\nharness: fake\n---\n\nOriginal.\n"
	code, created, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "name": "editme", "markdown": md,
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	code, got, _ := doJSON(t, "GET", base+"/v1/jobs/editme?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, got)
	}
	hash, _ := got["hash"].(string)
	if hash == "" {
		t.Fatalf("no hash: %v", got)
	}

	// Stale If-Match → 412 precondition_failed
	code, body := doReq(t, "PUT", base+"/v1/jobs/editme?workspace="+wsID,
		map[string]string{"If-Match": "deadbeef"},
		map[string]any{"markdown": "---\nharness: fake\n---\n\nStale write.\n"})
	if code != 412 || errCode(body) != "precondition_failed" {
		t.Fatalf("want 412 precondition_failed, got %d %v", code, body)
	}

	// Fresh If-Match → 200
	code, body = doReq(t, "PUT", base+"/v1/jobs/editme?workspace="+wsID,
		map[string]string{"If-Match": hash},
		map[string]any{"markdown": "---\nharness: fake\n---\n\nFresh write.\n"})
	if code != 200 {
		t.Fatalf("fresh put %d %v", code, body)
	}

	// Absent If-Match → proceed (D1)
	code, body = doReq(t, "PUT", base+"/v1/jobs/editme?workspace="+wsID, nil,
		map[string]any{"markdown": "---\nharness: fake\n---\n\nUnconditional.\n"})
	if code != 200 {
		t.Fatalf("absent put %d %v", code, body)
	}
}

// TestJobPauseIfMatch covers If-Match on pause/unpause.
func TestJobPauseIfMatch(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "pausable")
	wsID := addWorkspace(t, base, wsDir, "pause-ws")

	// Stale If-Match → 412, file untouched.
	code, body := doReq(t, "POST", base+"/v1/jobs/pausable/pause?workspace="+wsID,
		map[string]string{"If-Match": "deadbeef"}, nil)
	if code != 412 || errCode(body) != "precondition_failed" {
		t.Fatalf("stale pause: want 412 precondition_failed got %d %v", code, body)
	}

	// Fresh If-Match → 200 and job disabled.
	code, got, _ := doJSON(t, "GET", base+"/v1/jobs/pausable?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, got)
	}
	hash, _ := got["hash"].(string)
	code, body = doReq(t, "POST", base+"/v1/jobs/pausable/pause?workspace="+wsID,
		map[string]string{"If-Match": hash}, nil)
	if code != 200 || body["disabled"] != true {
		t.Fatalf("fresh pause: %d %v", code, body)
	}

	// Absent If-Match → unconditional unpause (D1).
	code, body, _ = doJSON(t, "POST", base+"/v1/jobs/pausable/unpause?workspace="+wsID, nil)
	if code != 200 || body["disabled"] != false {
		t.Fatalf("unpause: %d %v", code, body)
	}
}

// TestQueueCancelAndRequeue covers B6 status transitions over HTTP.
func TestQueueCancelAndRequeue(t *testing.T) {
	_, base, _ := startAPI(t)
	fire := map[string]any{
		"connector": "qtest", "workspace": fireWorkspace(t, base), "job": "j",
		"match": "true", "event": map[string]any{"x": 1},
	}
	code, res, _ := doJSON(t, "POST", base+"/v1/rules/fire", fire)
	if code != 200 || res["outcome"] != "queued" {
		t.Fatalf("fire %d %v", code, res)
	}
	qid, _ := res["queue_id"].(string)
	if qid == "" {
		t.Fatalf("no queue_id: %v", res)
	}

	code, body, _ := doJSON(t, "POST", base+"/v1/queue/"+qid+"/cancel", nil)
	if code != 200 || body["status"] != "done" || body["result"] != "skipped" {
		t.Fatalf("cancel want done/skipped got %d %v", code, body)
	}

	// Requeue of a done row → 409 conflict
	code, body, _ = doJSON(t, "POST", base+"/v1/queue/"+qid+"/requeue", nil)
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("requeue done want 409 conflict got %d %v", code, body)
	}

	// Unknown id → 404
	code, body, _ = doJSON(t, "POST", base+"/v1/queue/nope/cancel", nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("cancel missing want 404 got %d %v", code, body)
	}
}

// doReq is doJSON plus custom request headers (If-Match).
func doReq(t *testing.T, method, url string, headers map[string]string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

// errCode extracts error.code from an API error body.
func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	return fmt.Sprint(e["code"])
}

// A full replace and a patch both change frontmatter, so job_index must
// follow. Without a rebuild, `job list` keeps reporting the old harness.
func TestJobWriteRefreshesIndex(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "idx-ws")
	code, created, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "name": "sweep", "markdown": "---\nharness: claude\n---\n\nDo work.\n",
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	code, body, _ := doJSON(t, "PUT", base+"/v1/jobs/sweep?workspace="+wsID, map[string]any{
		"markdown": "---\nharness: codex\n---\n\nDo work.\n",
	})
	if code != 200 {
		t.Fatalf("put %d %v", code, body)
	}
	if got := jobHarness(t, base, wsID, "sweep"); got != "codex" {
		t.Fatalf("after put harness = %q, want codex", got)
	}
	code, body, _ = doJSON(t, "PATCH", base+"/v1/jobs/sweep?workspace="+wsID, map[string]any{
		"harness": "claude",
	})
	if code != 200 {
		t.Fatalf("patch %d %v", code, body)
	}
	if got := jobHarness(t, base, wsID, "sweep"); got != "claude" {
		t.Fatalf("after patch harness = %q, want claude", got)
	}
}

// TestJobGetReturnsIndexName: an agent-owned job's GET name is the
func TestJobGetInputKeysFromJobMarkdownOnly(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "input-keys-ws")

	md := "---\nharness: fake\n---\n\nEmail {{to}}. Template slots `{{PREHEADER}}`, `{{DATE_RANGE}}`.\n"
	code, created, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "name": "digest", "markdown": md,
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	tpl := filepath.Join(wsDir, "digest", "templates", "email.html")
	if err := os.MkdirAll(filepath.Dir(tpl), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tpl, []byte("<div>{{PREHEADER}} {{SECTIONS}} {{ITEM_TITLE}}</div>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, got, _ := doJSON(t, "GET", base+"/v1/jobs/digest?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, got)
	}
	keys, _ := got["input_keys"].([]any)
	if len(keys) != 1 || keys[0] != "to" {
		t.Fatalf("input_keys=%v want [to] (template-file placeholders must not appear)", got["input_keys"])
	}
}

// workspace-relative path, the same key runs and list use — not the folder
// basename ParseFile would return.
func TestJobGetReturnsIndexName(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "job-get-index")
	name := "agents/cmo/duty"
	code, body, _ := doJSON(t, "GET", base+"/v1/jobs/agents%2Fcmo%2Fduty?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, body)
	}
	if body["name"] != name {
		t.Fatalf("name=%v want %s", body["name"], name)
	}
}

// TestJobPayloadEnvSets covers plan H.3.5: env_sets is the list and env_set
// stays as the first name for one release.
func TestJobPayloadEnvSets(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "envsets-payload")

	for name, md := range map[string]string{
		"listjob":   "---\nharness: fake\nenvset: [one, two]\n---\n\nBody.\n",
		"scalarjob": "---\nharness: fake\nenvset: prod\n---\n\nBody.\n",
		"noenvset":  "---\nharness: fake\n---\n\nBody.\n",
	} {
		code, created, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{
			"workspace": wsID, "name": name, "markdown": md,
		})
		if code != 201 {
			t.Fatalf("%s create %d %v", name, code, created)
		}
	}

	code, body, _ := doJSON(t, "GET", base+"/v1/jobs/listjob?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("get listjob %d %v", code, body)
	}
	if sets, _ := body["env_sets"].([]any); len(sets) != 2 || sets[0] != "one" || sets[1] != "two" {
		t.Fatalf("env_sets = %v", body["env_sets"])
	}
	if body["env_set"] != "one" {
		t.Fatalf("env_set = %v", body["env_set"])
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/jobs/scalarjob?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("get scalarjob %d %v", code, body)
	}
	if sets, _ := body["env_sets"].([]any); len(sets) != 1 || sets[0] != "prod" {
		t.Fatalf("scalar env_sets = %v", body["env_sets"])
	}
	if body["env_set"] != "prod" {
		t.Fatalf("scalar env_set = %v", body["env_set"])
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/jobs/noenvset?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("get noenvset %d %v", code, body)
	}
	if sets, _ := body["env_sets"].([]any); sets == nil || len(sets) != 0 {
		t.Fatalf("unset env_sets = %v (want [])", body["env_sets"])
	}
	if body["env_set"] != "" {
		t.Fatalf("unset env_set = %v", body["env_set"])
	}
}

func writeJobFile(t *testing.T, wsDir, jobName string) string {
	t.Helper()
	dir := filepath.Join(wsDir, jobName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "JOB.md")
	if err := os.WriteFile(path, []byte("---\nharness: fake\n---\n\nDo work.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func addWorkspace(t *testing.T, base, path, name string) string {
	t.Helper()
	code, ws, _ := doJSON(t, "POST", base+"/v1/workspaces", map[string]any{"path": path, "name": name})
	if code != 201 {
		t.Fatalf("workspace add %d %v", code, ws)
	}
	id, _ := ws["id"].(string)
	if id == "" {
		t.Fatalf("no workspace id: %v", ws)
	}
	return id
}

func jobHarness(t *testing.T, base, wsID, name string) string {
	t.Helper()
	code, body, _ := doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("list %d %v", code, body)
	}
	jobs, _ := body["jobs"].([]any)
	for _, j := range jobs {
		row, _ := j.(map[string]any)
		if row["name"] == name {
			h, _ := row["harness"].(string)
			return h
		}
	}
	t.Fatalf("job %s not listed: %v", name, body)
	return ""
}

// TestJobKitSourceJobOverride proves a job's own skills and connectors list
// is labeled "job" next to the owner kit (P1-26).
func TestJobKitSourceJobOverride(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	files := map[string]string{
		filepath.Join(wsDir, "agents", "cmo", "AGENT.md"):       "---\ndescription: Test agent.\nharness: fake\nkit:\n  skills: [a]\n  connectors: [c]\n  envset: [e]\n---\n\nRun growth.\n",
		filepath.Join(wsDir, "agents", "cmo", "duty", "JOB.md"): "---\nharness: fake\nskills: [b]\nconnectors: [d]\n---\n\nDo the duty.\n",
	}
	for path, md := range files {
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wsID := addWorkspace(t, base, wsDir, "kit-source")
	code, body, _ := doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID+"&owner=cmo", nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	jobs, _ := body["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %v", body)
	}
	src, _ := jobs[0].(map[string]any)["kit_source"].(map[string]any)
	if src["skills"] != "job" || src["connectors"] != "job" || src["envset"] != "agent" {
		t.Fatalf("kit_source = %v", src)
	}
}

// fireWorkspace adds a workspace with the job "j" for rules/fire.
func fireWorkspace(t *testing.T, base string) string {
	t.Helper()
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "j")
	return addWorkspace(t, base, wsDir, "ws")
}

// TestJobRenameRoute: the flat route takes a loose name, the subtree takes an
// owned name, and each refusal has its own status.
func TestJobRenameRoute(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "report")
	writeJobFile(t, wsDir, "other")
	writeJobFile(t, wsDir, "agents/keeper/build")
	if err := os.WriteFile(filepath.Join(wsDir, "agents", "keeper", "AGENT.md"), []byte("---\ndescription: Keeper.\n---\n\nKeep.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "rename-jobs")

	code, body, _ := doJSON(t, "POST", base+"/v1/jobs/report/rename?workspace="+wsID, map[string]any{"to": "weekly"})
	if code != http.StatusOK || body["from"] != "report" || body["to"] != "weekly" || body["id"] == "" {
		t.Fatalf("loose rename: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/jobs/agents/keeper/build/rename?workspace="+wsID, map[string]any{"to": "ship"})
	if code != http.StatusOK || body["to"] != "agents/keeper/ship" {
		t.Fatalf("owned rename: %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "keeper", "ship", "JOB.md")); err != nil {
		t.Fatalf("owned folder: %v", err)
	}
	cases := []struct {
		path, to string
		want     int
	}{
		{"weekly", "Bad Name", http.StatusBadRequest},
		{"weekly", "other", http.StatusConflict},
		{"missing", "x", http.StatusNotFound},
	}
	for _, c := range cases {
		code, body, _ := doJSON(t, "POST", base+"/v1/jobs/"+c.path+"/rename?workspace="+wsID, map[string]any{"to": c.to})
		if code != c.want {
			t.Fatalf("%s -> %s: %d %v, want %d", c.path, c.to, code, body, c.want)
		}
	}
}

// TestJobFolder covers the folder key on list, get, patch, create, and validate.
func TestJobFolder(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "folders")
	duty := base + "/v1/jobs/agents%2Fcmo%2Fduty?workspace=" + wsID

	folders := func() map[string]any {
		t.Helper()
		code, body, _ := doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID, nil)
		if code != 200 {
			t.Fatalf("list %d %v", code, body)
		}
		out := map[string]any{}
		jobs, _ := body["jobs"].([]any)
		for _, j := range jobs {
			row, _ := j.(map[string]any)
			out[row["name"].(string)] = row["folder"]
		}
		return out
	}

	if code, body, _ := doJSON(t, "PATCH", duty, map[string]any{"folder": "SEO"}); code != 200 {
		t.Fatalf("patch %d %v", code, body)
	}
	if got := folders(); got["agents/cmo/duty"] != "SEO" || got["alpha"] != "" {
		t.Fatalf("folders=%v", got)
	}
	if code, body, _ := doJSON(t, "GET", duty, nil); code != 200 || body["folder"] != "SEO" {
		t.Fatalf("get %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "PATCH", duty, map[string]any{"folder": "a/b"}); code != 400 || errCode(body) != "validation" {
		t.Fatalf("patch a/b %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "PATCH", base+"/v1/jobs/alpha?workspace="+wsID, map[string]any{"folder": "SEO"}); code != 400 || errCode(body) != "validation" {
		t.Fatalf("patch loose %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "PATCH", duty, map[string]any{"folder": ""}); code != 200 {
		t.Fatalf("patch empty %d %v", code, body)
	}
	if got := folders(); got["agents/cmo/duty"] != "" {
		t.Fatalf("folders after clear=%v", got)
	}

	code, body, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "name": "audit", "agent": "cmo", "prompt": "Audit.", "folder": "Growth",
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, body)
	}
	if got := folders(); got["agents/cmo/audit"] != "Growth" {
		t.Fatalf("folders after create=%v", got)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "name": "loose", "prompt": "Go.", "folder": "Growth",
	})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("create loose %d %v", code, body)
	}

	for _, tc := range []struct {
		body map[string]any
		ok   bool
	}{
		{map[string]any{"name": "reports/daily", "prompt": "Go.", "harness": "fake", "folder": "SEO"}, false},
		{map[string]any{"name": "agents/cmo/daily", "prompt": "Go.", "harness": "fake", "folder": "SEO"}, true},
		{map[string]any{"name": "daily", "agent": "cmo", "prompt": "Go.", "harness": "fake", "folder": "SEO"}, true},
	} {
		code, body, _ := doJSON(t, "POST", base+"/v1/jobs/validate", tc.body)
		if code != 200 || body["ok"] != tc.ok {
			t.Fatalf("validate %v: %d %v", tc.body, code, body)
		}
	}
}
