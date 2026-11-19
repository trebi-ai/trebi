package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
)

func TestJobsListByWorkspaceName(t *testing.T) {
	_, base, _ := startAPI(t)
	ops := t.TempDir()
	staging := t.TempDir()
	writeJobFile(t, ops, "agents/ops/report")
	writeJobFile(t, staging, "agents/ops/report")
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
	if row["workspace_id"] != stID || row["name"] != "agents/ops/report" {
		t.Fatalf("row=%v want workspace %s", row, stID)
	}
}

// TestJobDeleteAmbiguousAcrossWorkspaces covers A4/B4: a duplicate job name
// without ?workspace= must 400, never delete the first match.
func TestJobDeleteAmbiguousAcrossWorkspaces(t *testing.T) {
	_, base, _ := startAPI(t)
	ws1 := t.TempDir()
	ws2 := t.TempDir()
	p1 := writeJobFile(t, ws1, "agents/ops/dup")
	p2 := writeJobFile(t, ws2, "agents/ops/dup")
	id1 := addWorkspace(t, base, ws1, "amb-a")
	_ = addWorkspace(t, base, ws2, "amb-b")

	code, body, _ := doJSON(t, "DELETE", base+"/v1/jobs/agents%2Fops%2Fdup?confirm=1", nil)
	if code != 400 || errCode(body) != "ambiguous" {
		t.Fatalf("want 400 ambiguous, got %d %v", code, body)
	}
	if _, err := os.Stat(p1); err != nil {
		t.Fatalf("ws1 job removed on ambiguous delete: %v", err)
	}
	if _, err := os.Stat(p2); err != nil {
		t.Fatalf("ws2 job removed on ambiguous delete: %v", err)
	}

	code, body, _ = doJSON(t, "DELETE", base+"/v1/jobs/agents%2Fops%2Fdup?workspace="+id1+"&confirm=1", nil)
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
	writeAgentFile(t, wsDir, "ops")
	wsID := addWorkspace(t, base, wsDir, "ifm-ws")

	md := "---\nharness: fake\n---\n\nOriginal.\n"
	code, created, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "name": "agents/ops/editme", "markdown": md,
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	code, got, _ := doJSON(t, "GET", base+"/v1/jobs/agents%2Fops%2Feditme?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, got)
	}
	hash, _ := got["hash"].(string)
	if hash == "" {
		t.Fatalf("no hash: %v", got)
	}

	// Stale If-Match → 412 precondition_failed
	code, body := doReq(t, "PUT", base+"/v1/jobs/agents%2Fops%2Feditme?workspace="+wsID,
		map[string]string{"If-Match": "deadbeef"},
		map[string]any{"markdown": "---\nharness: fake\n---\n\nStale write.\n"})
	if code != 412 || errCode(body) != "precondition_failed" {
		t.Fatalf("want 412 precondition_failed, got %d %v", code, body)
	}

	// Fresh If-Match → 200
	code, body = doReq(t, "PUT", base+"/v1/jobs/agents%2Fops%2Feditme?workspace="+wsID,
		map[string]string{"If-Match": hash},
		map[string]any{"markdown": "---\nharness: fake\n---\n\nFresh write.\n"})
	if code != 200 {
		t.Fatalf("fresh put %d %v", code, body)
	}

	// Absent If-Match → proceed (D1)
	code, body = doReq(t, "PUT", base+"/v1/jobs/agents%2Fops%2Feditme?workspace="+wsID, nil,
		map[string]any{"markdown": "---\nharness: fake\n---\n\nUnconditional.\n"})
	if code != 200 {
		t.Fatalf("absent put %d %v", code, body)
	}
}

// TestJobPauseIfMatch covers If-Match on pause/unpause.
func TestJobPauseIfMatch(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "agents/ops/pausable")
	wsID := addWorkspace(t, base, wsDir, "pause-ws")

	// Stale If-Match → 412, file untouched.
	code, body := doReq(t, "POST", base+"/v1/jobs/agents%2Fops%2Fpausable/pause?workspace="+wsID,
		map[string]string{"If-Match": "deadbeef"}, nil)
	if code != 412 || errCode(body) != "precondition_failed" {
		t.Fatalf("stale pause: want 412 precondition_failed got %d %v", code, body)
	}

	// Fresh If-Match → 200 and job disabled.
	code, got, _ := doJSON(t, "GET", base+"/v1/jobs/agents%2Fops%2Fpausable?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, got)
	}
	hash, _ := got["hash"].(string)
	code, body = doReq(t, "POST", base+"/v1/jobs/agents%2Fops%2Fpausable/pause?workspace="+wsID,
		map[string]string{"If-Match": hash}, nil)
	if code != 200 || body["disabled"] != true {
		t.Fatalf("fresh pause: %d %v", code, body)
	}

	// Absent If-Match → unconditional unpause (D1).
	code, body, _ = doJSON(t, "POST", base+"/v1/jobs/agents%2Fops%2Fpausable/unpause?workspace="+wsID, nil)
	if code != 200 || body["disabled"] != false {
		t.Fatalf("unpause: %d %v", code, body)
	}
}

// TestQueueCancelAndRequeue covers B6 status transitions over HTTP.
func TestQueueCancelAndRequeue(t *testing.T) {
	_, base, _ := startAPI(t)
	fire := map[string]any{
		"connector": "qtest", "workspace": fireWorkspace(t, base), "job": "agents/ops/j",
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

// TestJobNotFoundNamesOwnedJob gives the full name of an owned job when a
// caller asks for its short name.
func TestJobNotFoundNamesOwnedJob(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	addWorkspace(t, base, wsDir, "short-names")
	for _, name := range []string{"duty", "cmo/duty"} {
		code, body, _ := doJSON(t, "POST", base+"/v1/jobs/"+url.PathEscape(name)+"/validate", nil)
		e, _ := body["error"].(map[string]any)
		msg, _ := e["message"].(string)
		if code != http.StatusNotFound || !strings.Contains(msg, "agents/cmo/duty") {
			t.Fatalf("%s: %d %v", name, code, body)
		}
	}
	code, body, _ := doJSON(t, "POST", base+"/v1/jobs/nothing/validate", nil)
	if code != http.StatusNotFound || errCode(body) != "not_found" {
		t.Fatalf("unknown: %d %v", code, body)
	}
}

// A full replace and a patch both change frontmatter, so job_index must
// follow. Without a rebuild, `job list` keeps reporting the old harness.
func TestJobWriteRefreshesIndex(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFile(t, wsDir, "ops")
	wsID := addWorkspace(t, base, wsDir, "idx-ws")
	code, created, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "name": "agents/ops/sweep", "markdown": "---\nharness: claude\n---\n\nDo work.\n",
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	code, body, _ := doJSON(t, "PUT", base+"/v1/jobs/agents%2Fops%2Fsweep?workspace="+wsID, map[string]any{
		"markdown": "---\nharness: codex\n---\n\nDo work.\n",
	})
	if code != 200 {
		t.Fatalf("put %d %v", code, body)
	}
	if got := jobHarness(t, base, wsID, "agents/ops/sweep"); got != "codex" {
		t.Fatalf("after put harness = %q, want codex", got)
	}
	code, body, _ = doJSON(t, "PATCH", base+"/v1/jobs/agents%2Fops%2Fsweep?workspace="+wsID, map[string]any{
		"harness": "claude",
	})
	if code != 200 {
		t.Fatalf("patch %d %v", code, body)
	}
	if got := jobHarness(t, base, wsID, "agents/ops/sweep"); got != "claude" {
		t.Fatalf("after patch harness = %q, want claude", got)
	}
}

// TestJobGetReturnsIndexName: an agent-owned job's GET name is the
func TestJobGetInputKeysFromJobMarkdownOnly(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFile(t, wsDir, "ops")
	wsID := addWorkspace(t, base, wsDir, "input-keys-ws")

	md := "---\nharness: fake\n---\n\nEmail {{to}}. Template slots `{{PREHEADER}}`, `{{DATE_RANGE}}`.\n"
	code, created, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "name": "agents/ops/digest", "markdown": md,
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	tpl := filepath.Join(wsDir, "agents", "ops", "digest", "templates", "email.html")
	if err := os.MkdirAll(filepath.Dir(tpl), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tpl, []byte("<div>{{PREHEADER}} {{SECTIONS}} {{ITEM_TITLE}}</div>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, got, _ := doJSON(t, "GET", base+"/v1/jobs/agents%2Fops%2Fdigest?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, got)
	}
	if _, ok := got["input_keys"]; ok {
		t.Fatalf("input_keys is removed; got %v", got["input_keys"])
	}
	inputs, _ := got["inputs"].([]any)
	if len(inputs) != 1 {
		t.Fatalf("inputs=%v want one entry (template-file placeholders must not appear)", got["inputs"])
	}
	in, _ := inputs[0].(map[string]any)
	if in["name"] != "to" || in["type"] != "string" || in["declared"] != false {
		t.Fatalf("inputs[0]=%v want undeclared string input to", in)
	}
}

func TestJobGetAndValidateTypedInputs(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFile(t, wsDir, "ops")
	wsID := addWorkspace(t, base, wsDir, "typed-inputs-ws")

	md := "---\nharness: fake\ninputs:\n  body: {type: string, label: Email text, format: multiline, required: true}\n" +
		"  tone: {type: string, enum: [friendly, short], default: friendly}\n" +
		"triggers:\n  - every: 1h\n    inputs: {tone: short}\n---\n\nReply to {{body}} in a {{tone}} way. {{extra}}\n"
	code, created, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{"workspace": wsID, "name": "agents/ops/reply", "markdown": md})
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	code, got, _ := doJSON(t, "GET", base+"/v1/jobs/agents%2Fops%2Freply?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, got)
	}
	inputs, _ := got["inputs"].([]any)
	var names []string
	for _, e := range inputs {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "body,tone,extra" {
		t.Fatalf("input order = %v, want body,tone,extra", names)
	}
	first := inputs[0].(map[string]any)
	if first["label"] != "Email text" || first["format"] != "multiline" || first["required"] != true || first["declared"] != true {
		t.Fatalf("inputs[0] = %v", first)
	}

	code, v, _ := doJSON(t, "POST", base+"/v1/jobs/agents%2Fops%2Freply/validate?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("validate %d %v", code, v)
	}
	if v["ok"] != false {
		t.Fatalf("validate ok = %v, want false: the schedule does not fill body", v["ok"])
	}
	codes := map[string]string{}
	for _, e := range v["issues"].([]any) {
		is := e.(map[string]any)
		codes[is["path"].(string)] = is["code"].(string) + "/" + is["severity"].(string)
	}
	want := map[string]string{
		"triggers[0].inputs.body": "required_unfilled/error",
		"prompt":                  "placeholder_undeclared/warning",
	}
	for path, c := range want {
		if codes[path] != c {
			t.Errorf("issue %s = %q, want %q (all %v)", path, codes[path], c, codes)
		}
	}

	code, v, _ = doJSON(t, "POST", base+"/v1/jobs/validate", map[string]any{
		"prompt": "Do {{body}}.", "harness": "fake", "agent": "ops",
		"inputs": []any{map[string]any{"name": "body", "type": "string", "required": true}},
	})
	if code != 200 || v["ok"] != true {
		t.Fatalf("validate text %d %v", code, v)
	}
	if !strings.Contains(v["markdown"].(string), "body: {type: string, required: true}") {
		t.Fatalf("markdown = %s", v["markdown"])
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

// writeJobFile writes a job under an agent: agents/ops/<name> when the name
// has no agent, plus the AGENT.md of the agent when it is missing.
func writeJobFile(t *testing.T, wsDir, jobName string) string {
	t.Helper()
	if !strings.HasPrefix(jobName, "agents/") {
		jobName = "agents/ops/" + jobName
	}
	writeAgentFile(t, wsDir, strings.Split(jobName, "/")[1])
	return writeLooseJobFile(t, wsDir, jobName)
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
		filepath.Join(wsDir, "agents", "cmo", "AGENT.md"):       "---\ndescription: Test agent.\nharness: fake\nkit:\n  skills: [a]\n  connectors: [c]\n  env:\n    E: e\n---\n\nRun growth.\n",
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
	if src["skills"] != "job" || src["connectors"] != "job" || src["env"] != "agent" {
		t.Fatalf("kit_source = %v", src)
	}
}

// TestJobGetWorkingDir: job get gives the resolved working folder and its
// source for an owned job and a loose job.
func TestJobGetWorkingDir(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	agentMD := "---\ndescription: Test agent.\nharness: fake\nworking_dir: clients/acme\n---\n\nRun growth.\n"
	if err := os.WriteFile(filepath.Join(wsDir, "agents", "cmo", "AGENT.md"), []byte(agentMD), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "working-dir")
	agentDir := filepath.Join(wsDir, "agents", "cmo", "clients", "acme")
	for _, tc := range []struct{ name, dir, source string }{
		{"agents/cmo/duty", agentDir, "agent"},
		{"agents/ops/alpha", filepath.Join(wsDir, "agents", "ops", "alpha"), "default"},
	} {
		code, body, _ := doJSON(t, "GET", base+request.BuildJobGetPath(tc.name, wsID), nil)
		if code != 200 {
			t.Fatalf("get %s: %d %v", tc.name, code, body)
		}
		if body["working_dir"] != tc.dir || body["working_dir_source"] != tc.source {
			t.Fatalf("%s: working_dir = %v, source = %v", tc.name, body["working_dir"], body["working_dir_source"])
		}
	}
}

// fireWorkspace adds a workspace with the job "j" for rules/fire.
func fireWorkspace(t *testing.T, base string) string {
	t.Helper()
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "agents/ops/j")
	return addWorkspace(t, base, wsDir, "ws")
}

// TestJobRenameRoute: the flat route takes a loose name, the subtree takes an
// owned name, and each refusal has its own status.
func TestJobRenameRoute(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "agents/ops/report")
	writeJobFile(t, wsDir, "agents/ops/other")
	writeJobFile(t, wsDir, "agents/keeper/build")
	if err := os.WriteFile(filepath.Join(wsDir, "agents", "keeper", "AGENT.md"), []byte("---\ndescription: Keeper.\n---\n\nKeep.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "rename-jobs")

	code, body, _ := doJSON(t, "POST", base+"/v1/jobs/agents%2Fops%2Freport/rename?workspace="+wsID, map[string]any{"to": "weekly"})
	if code != http.StatusOK || body["from"] != "agents/ops/report" || body["to"] != "agents/ops/weekly" || body["id"] == "" {
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
		{"agents/ops/weekly", "Bad Name", http.StatusBadRequest},
		{"agents/ops/weekly", "other", http.StatusConflict},
		{"missing", "x", http.StatusNotFound},
	}
	for _, c := range cases {
		code, body, _ := doJSON(t, "POST", base+"/v1/jobs/"+c.path+"/rename?workspace="+wsID, map[string]any{"to": c.to})
		if code != c.want {
			t.Fatalf("%s -> %s: %d %v, want %d", c.path, c.to, code, body, c.want)
		}
	}
}

// TestJobMoveRoute covers a move to another agent, a folder-only move, the
// folder rename, and the error map.
func TestJobMoveRoute(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "agents/cmo/renewals")
	writeJobFile(t, wsDir, "agents/cmo/digest")
	for _, a := range []string{"cmo", "cs"} {
		if err := os.MkdirAll(filepath.Join(wsDir, "agents", a), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wsDir, "agents", a, "AGENT.md"), []byte("---\ndescription: Agent.\n---\n\nWork.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wsID := addWorkspace(t, base, wsDir, "move-jobs")
	move := func(name string, body map[string]any) (int, map[string]any) {
		t.Helper()
		code, out, _ := doJSON(t, "POST", base+"/v1/jobs/"+name+"/move?workspace="+wsID, body)
		return code, out
	}

	code, body := move("agents/cmo/renewals", map[string]any{"agent": "cs", "folder": "Health", "dry_run": true})
	if code != http.StatusOK || body["moved"] != false || body["to"] != "agents/cs/renewals" || body["changes"] == nil {
		t.Fatalf("dry run: %d %v", code, body)
	}
	code, body = move("agents/cmo/renewals", map[string]any{"agent": "cs", "folder": "Health"})
	if code != http.StatusOK || body["moved"] != true || body["folder"] != "Health" {
		t.Fatalf("move: %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cs", "renewals", "JOB.md")); err != nil {
		t.Fatalf("moved folder: %v", err)
	}
	code, body = move("agents/cmo/digest", map[string]any{"folder": "Content"})
	if code != http.StatusOK || body["to"] != "agents/cmo/digest" {
		t.Fatalf("folder only: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID, nil)
	jobs, _ := body["jobs"].([]any)
	folders := map[string]any{}
	for _, j := range jobs {
		row, _ := j.(map[string]any)
		folders[row["name"].(string)] = row["folder"]
	}
	if code != http.StatusOK || folders["agents/cmo/digest"] != "Content" || folders["agents/cs/renewals"] != "Health" {
		t.Fatalf("list: %d %v", code, folders)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/agents/cmo/folders/rename", map[string]any{"from": "Content", "to": "Posts"})
	if code != http.StatusOK || body["jobs"] != float64(1) {
		t.Fatalf("folder rename: %d %v", code, body)
	}
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"agents/cmo/digest", map[string]any{"agent": "nope"}, http.StatusNotFound},
		{"agents/cmo/digest", map[string]any{"folder": "a/b"}, http.StatusBadRequest},
		{"agents/cmo/digest", map[string]any{"agent": "", "folder": "X"}, http.StatusBadRequest},
		{"agents/cmo/digest", map[string]any{"agent": ""}, http.StatusBadRequest},
		{"missing", map[string]any{"agent": "cs"}, http.StatusNotFound},
	}
	for _, c := range cases {
		if code, body := move(c.name, c.body); code != c.want {
			t.Fatalf("%s %v: %d %v, want %d", c.name, c.body, code, body, c.want)
		}
	}
	if code, body := move("agents/cmo/digest", map[string]any{"agent": ""}); !strings.Contains(errMsg(body), "Choose the agent that gets the job") {
		t.Fatalf("move to no agent: %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/agents/cmo/folders/rename", map[string]any{"from": "Missing", "to": "X"}); code != http.StatusNotFound {
		t.Fatalf("missing folder: %d %v", code, body)
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
	if got := folders(); got["agents/cmo/duty"] != "SEO" || got["agents/ops/alpha"] != "" {
		t.Fatalf("folders=%v", got)
	}
	if code, body, _ := doJSON(t, "GET", duty, nil); code != 200 || body["folder"] != "SEO" {
		t.Fatalf("get %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "PATCH", duty, map[string]any{"folder": "a/b"}); code != 400 || errCode(body) != "validation" {
		t.Fatalf("patch a/b %d %v", code, body)
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

func TestJobValidateBatch(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFile(t, wsDir, "cmo")
	writeTestFile(t, filepath.Join(wsDir, "agents", "cmo", "digest", "JOB.md"), "---\nharness: fake\n---\n\nSum up {{events}}.\n")
	writeTestFile(t, filepath.Join(wsDir, "agents", "cmo", "single", "JOB.md"), "---\nharness: fake\n---\n\nAnswer {{text}}.\n")
	wsID := addWorkspace(t, base, wsDir, "batch-ws")
	for _, tc := range []struct {
		name   string
		job    string
		filter job.TriggerYAML
		ok     bool
		warn   string
	}{
		{"batch", "cmo/digest", job.TriggerYAML{Batch: "15m"}, true, ""},
		{"single var", "cmo/single", job.TriggerYAML{Batch: "15m"}, true, "{{text}} is empty in a batch run"},
		{"too short", "cmo/digest", job.TriggerYAML{Batch: "30s"}, false, ""},
		{"with replies", "cmo/digest", job.TriggerYAML{Batch: "15m", Replies: "continue"}, false, ""},
	} {
		routingtest.Write(t, wsDir, "chat", routing.File{Routes: []routing.Route{{Job: tc.job, Gets: routing.GetsEveryMessage, Filter: tc.filter}}})
		code, body, _ := doJSON(t, "POST", base+"/v1/jobs/"+url.PathEscape("agents/"+tc.job)+"/validate?workspace="+wsID, nil)
		warn, _ := body["warnings"].([]any)
		if code != 200 || body["ok"] != tc.ok || (tc.warn != "" && (len(warn) != 1 || !strings.Contains(fmt.Sprint(warn[0]), tc.warn))) {
			t.Fatalf("%s: %d %v", tc.name, code, body)
		}
	}
}

// TestJobGetFolderEnv: job and agent get give the working dir .env key names
// and never a value; env_file: false turns the load off.
func TestJobGetFolderEnv(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	const secret = "very-secret-value"
	for _, dir := range []string{wsDir, filepath.Join(wsDir, "agents", "ops", "alpha")} {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("TOKEN="+secret+"\nPEM=\""+secret+"\nline 2\"\nbad\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wsID := addWorkspace(t, base, wsDir, "folder-env")
	folderEnv := func(path string) map[string]any {
		t.Helper()
		code, body, _ := doJSON(t, "GET", base+path, nil)
		if code != 200 {
			t.Fatalf("get %s: %d %v", path, code, body)
		}
		if raw, _ := json.Marshal(body); strings.Contains(string(raw), secret) {
			t.Fatalf("%s leaks a value: %s", path, raw)
		}
		fe, _ := body["folder_env"].(map[string]any)
		return fe
	}

	fe := folderEnv(request.BuildJobGetPath("agents/ops/alpha", wsID))
	if fe["found"] != true || fe["enabled"] != true || fmt.Sprint(fe["keys"]) != "[TOKEN PEM]" || fe["path"] != filepath.Join(wsDir, "agents", "ops", "alpha", ".env") {
		t.Fatalf("alpha folder_env = %v", fe)
	}
	if sk, _ := fe["skipped"].([]any); len(sk) != 1 {
		t.Fatalf("skipped = %v", fe["skipped"])
	}
	if fe := folderEnv(request.BuildJobGetPath("agents/cmo/duty", wsID)); fe["found"] != false {
		t.Fatalf("duty folder_env = %v", fe)
	}
	if fe := folderEnv(request.BuildAgentGetPath(wsID, "cmo")); fe["found"] != true || fmt.Sprint(fe["keys"]) != "[TOKEN PEM]" {
		t.Fatalf("agent folder_env = %v", fe)
	}

	req := request.BuildJobPatch("agents/ops/alpha", wsID, map[string]any{"env_file": false}, "")
	if code, body, _ := doJSON(t, req.Method, base+req.Path, req.Body); code != 200 {
		t.Fatalf("patch: %d %v", code, body)
	}
	if fe := folderEnv(request.BuildJobGetPath("agents/ops/alpha", wsID)); fe["found"] != true || fe["enabled"] != false {
		t.Fatalf("alpha folder_env after off = %v", fe)
	}
}

// TestJobCreateNeedsAgent: a new job lands under an agent. A name with the
// agents/<name>/ prefix sets the agent.
func TestJobCreateNeedsAgent(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFile(t, wsDir, "cmo")
	writeAgentFile(t, wsDir, "cs")
	wsID := addWorkspace(t, base, wsDir, "create-agent")
	create := func(body map[string]any) (int, map[string]any) {
		t.Helper()
		body["workspace"], body["prompt"] = wsID, "Go."
		code, out, _ := doJSON(t, "POST", base+"/v1/jobs", body)
		return code, out
	}

	code, body := create(map[string]any{"name": "solo"})
	if code != http.StatusBadRequest || errCode(body) != "validation" || !strings.Contains(errMsg(body), job.ErrNoOwner.Error()) {
		t.Fatalf("no agent: %d %v", code, body)
	}
	code, body = create(map[string]any{"name": "agents/cmo/full"})
	if code != http.StatusCreated || body["name"] != "agents/cmo/full" {
		t.Fatalf("full name: %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cmo", "full", "JOB.md")); err != nil {
		t.Fatalf("full name file: %v", err)
	}
	code, body = create(map[string]any{"name": "agents/cmo/mixed", "agent": "cs"})
	if code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("agent does not match the name: %d %v", code, body)
	}
	code, body = create(map[string]any{"name": "lost", "agent": "nope"})
	if code != http.StatusNotFound || errCode(body) != "not_found" {
		t.Fatalf("unknown agent: %d %v", code, body)
	}
	code, body = create(map[string]any{"name": "short", "agent": "cs"})
	if code != http.StatusCreated || body["name"] != "agents/cs/short" {
		t.Fatalf("short name with agent: %d %v", code, body)
	}
}

// TestJobOutsideAgent: a JOB.md outside agents/ gets a doctor finding and
// does not run. A move into an agent clears the finding.
func TestJobOutsideAgent(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFile(t, wsDir, "ops")
	writeLooseJobFile(t, wsDir, "jobs/x")
	writeTestFile(t, filepath.Join(wsDir, "JOB.md"), "---\nharness: fake\n---\n\nTop.\n")
	wsID := addWorkspace(t, base, wsDir, "outside")

	findings := func() map[string]string {
		t.Helper()
		code, body, _ := doJSON(t, "GET", base+"/v1/system/doctor", nil)
		if code != 200 {
			t.Fatalf("doctor: %d %v", code, body)
		}
		out := map[string]string{}
		rows, _ := body["findings"].([]any)
		for _, r := range rows {
			if m, _ := r.(map[string]any); m["code"] == job.CodeJobOutsideAgent {
				out[fmt.Sprint(m["id"])] = fmt.Sprint(m["message"])
			}
		}
		return out
	}
	got := findings()
	nested := job.CodeJobOutsideAgent + ":" + filepath.Join(wsDir, "jobs", "x", "JOB.md")
	if len(got) != 2 || !strings.Contains(got[nested], "Job jobs/x is not in an agent") {
		t.Fatalf("findings = %v", got)
	}
	top := job.CodeJobOutsideAgent + ":" + filepath.Join(wsDir, "JOB.md")
	if !strings.Contains(got[top], "at the top of its workspace") {
		t.Fatalf("top finding = %q (all %v)", got[top], got)
	}

	code, body, _ := doJSON(t, "POST", base+"/v1/runs", map[string]any{"workspace": wsID, "job": "jobs/x"})
	if code != http.StatusBadRequest || errCode(body) != "validation" || !strings.Contains(errMsg(body), job.ErrNoOwner.Error()) {
		t.Fatalf("run outside an agent: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/jobs/jobs%2Fx/move?workspace="+wsID, map[string]any{"agent": "ops"})
	if code != http.StatusOK || body["to"] != "agents/ops/x" {
		t.Fatalf("move into agent: %d %v", code, body)
	}
	if got := findings(); len(got) != 1 || got[nested] != "" {
		t.Fatalf("findings after move = %v", got)
	}
}

// writeAgentFile writes agents/<name>/AGENT.md when it is missing.
func writeAgentFile(t *testing.T, wsDir, name string) {
	t.Helper()
	path := filepath.Join(wsDir, "agents", name, "AGENT.md")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		writeTestFile(t, path, "---\ndescription: Test agent.\nharness: fake\n---\n\nRun.\n")
	}
}

// writeLooseJobFile writes a job at the name as is, also outside an agent.
func writeLooseJobFile(t *testing.T, wsDir, jobName string) string {
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

// TestJobReplyRoomTaken: one chat has one job that replies. Validate lists
// the reply_room_taken issue for the second route. The first job validates clean.
func TestJobReplyRoomTaken(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "reply-ws")
	ensureAgent(t, base, wsID, "ellis")
	for _, name := range []string{"italy", "spain", "notes"} {
		if code, body, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{"workspace": wsID, "name": name, "agent": "ellis", "harness": "fake", "prompt": "Go."}); code != 201 {
			t.Fatalf("create %s %d %v", name, code, body)
		}
	}
	reply := job.TriggerYAML{Rooms: []string{"r1"}, Chat: "reply"}
	routingtest.Write(t, wsDir, "chat", routing.File{Routes: []routing.Route{
		{Job: "ellis/italy", Gets: routing.GetsEveryMessage, Filter: reply},
		{Job: "ellis/spain", Gets: routing.GetsEveryMessage, Filter: reply},
		{Job: "ellis/notes", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Rooms: []string{"r1"}}},
	}})
	validate := func(name string) map[string]any {
		t.Helper()
		code, body, _ := doJSON(t, "POST", base+"/v1/jobs/agents/ellis/"+name+"/validate?workspace="+wsID, nil)
		if code != 200 {
			t.Fatalf("validate %s %d %v", name, code, body)
		}
		return body
	}
	if body := validate("italy"); body["ok"] != true {
		t.Fatalf("validate italy %v", body)
	}
	if body := validate("notes"); body["ok"] != true {
		t.Fatalf("validate notes %v", body)
	}
	if body := validate("spain"); body["ok"] != false || !strings.Contains(fmt.Sprint(body["issues"]), "reply_room_taken") {
		t.Fatalf("validate spain %v", body)
	}
}

func TestJobGetArchivedSkills(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	jobFile := writeJobFile(t, wsDir, "agents/ops/j")
	writeTestFile(t, jobFile, "---\nharness: fake\nskills: [tone, live]\n---\n\nDo work.\n")
	wsID := addWorkspace(t, base, wsDir, "job-archived-skills")
	md := "---\nname: x\ndescription: Demo.\n---\n\nBody.\n"
	for _, n := range []string{"tone", "live"} {
		if code, body, _ := doJSON(t, "POST", base+"/v1/skills", map[string]any{"name": n, "skill_md": md}); code != http.StatusCreated {
			t.Fatalf("create %s: %d %v", n, code, body)
		}
	}
	_, got, _ := doJSON(t, "GET", jobURL(base, wsID, "agents/ops/j", ""), nil)
	if rows, _ := got["archived_skills"].([]any); got["archived_skills"] == nil || len(rows) != 0 {
		t.Fatalf("before archive: %v", got["archived_skills"])
	}
	if code, body, _ := doJSON(t, "POST", base+"/v1/skills/tone/archive", nil); code != http.StatusOK {
		t.Fatalf("archive: %d %v", code, body)
	}
	_, got, _ = doJSON(t, "GET", jobURL(base, wsID, "agents/ops/j", ""), nil)
	rows, _ := got["archived_skills"].([]any)
	if len(rows) != 1 {
		t.Fatalf("archived_skills=%v", got["archived_skills"])
	}
	row, _ := rows[0].(map[string]any)
	if row["name"] != "tone" || row["scope"] != "global" || row["ref"] != "global/tone" {
		t.Fatalf("row=%v", row)
	}
}
