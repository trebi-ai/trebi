package api_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobArchiveRoute(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	jobFile := writeJobFile(t, wsDir, "ops/old")
	writeJobFile(t, wsDir, "ops/live")
	wsID := addWorkspace(t, base, wsDir, "archive-ws")

	code, body := doReq(t, "POST", jobURL(base, wsID, "ops/old", "archive"), nil, nil)
	if code != http.StatusOK || body["archived_at"] == "" || body["archived_at"] == nil || body["queue_dropped"] != float64(0) {
		t.Fatalf("archive: %d %v", code, body)
	}
	if !strings.Contains(readFile(t, jobFile), "archived_at:") {
		t.Fatalf("JOB.md has no archived_at:\n%s", readFile(t, jobFile))
	}

	code, got, _ := doJSON(t, "GET", jobURL(base, wsID, "ops/old", ""), nil)
	if code != http.StatusOK || got["archived_at"] != body["archived_at"] {
		t.Fatalf("get: %d %v", code, got)
	}

	jobsURL := base + "/v1/jobs?workspace=" + wsID
	names, counts := listNames(t, jobsURL, "jobs")
	if strings.Join(names, ",") != "ops/live" || counts["current"] != float64(1) || counts["archived"] != float64(1) {
		t.Fatalf("default list: %v %v", names, counts)
	}
	names, _ = listNames(t, jobsURL+"&archived=true", "jobs")
	if strings.Join(names, ",") != "ops/old" {
		t.Fatalf("archived=true: %v", names)
	}
	names, _ = listNames(t, jobsURL+"&archived=all", "jobs")
	if len(names) != 2 {
		t.Fatalf("archived=all: %v", names)
	}
	if code, body, _ := doJSON(t, "GET", jobsURL+"&archived=maybe", nil); code != http.StatusBadRequest {
		t.Fatalf("archived=maybe: %d %v", code, body)
	}

	code, body = doReq(t, "POST", jobURL(base, wsID, "ops/old", "unarchive"), nil, nil)
	if code != http.StatusOK {
		t.Fatalf("unarchive: %d %v", code, body)
	}
	if strings.Contains(readFile(t, jobFile), "archived_at") {
		t.Fatalf("JOB.md keeps archived_at:\n%s", readFile(t, jobFile))
	}
	names, counts = listNames(t, jobsURL, "jobs")
	if len(names) != 2 || counts["archived"] != float64(0) {
		t.Fatalf("after unarchive: %v %v", names, counts)
	}
}

func TestJobArchiveIfMatch(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "ops/old")
	wsID := addWorkspace(t, base, wsDir, "archive-ifm")

	code, body := doReq(t, "POST", jobURL(base, wsID, "ops/old", "archive"), map[string]string{"If-Match": "deadbeef"}, nil)
	if code != http.StatusPreconditionFailed || errCode(body) != "precondition_failed" {
		t.Fatalf("stale: %d %v", code, body)
	}
	_, got, _ := doJSON(t, "GET", jobURL(base, wsID, "ops/old", ""), nil)
	hash, _ := got["hash"].(string)
	code, body = doReq(t, "POST", jobURL(base, wsID, "ops/old", "archive"), map[string]string{"If-Match": hash}, nil)
	if code != http.StatusOK {
		t.Fatalf("fresh: %d %v", code, body)
	}
}

func TestJobArchiveAgentScope(t *testing.T) {
	_, base, dir := startAPI(t)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "archive-scope")
	live := "archive-token-0000000000000000000000"
	insertAgentCaller(t, st, wsID, "cmo", live, "running")

	code, body := doScoped(t, "POST", jobURL(base, wsID, "cmo/duty", "archive"), live, nil)
	if code != http.StatusOK {
		t.Fatalf("own job: %d %v", code, body)
	}
	code, body = doScoped(t, "POST", jobURL(base, wsID, "cmo/duty", "unarchive"), live, nil)
	if code != http.StatusOK {
		t.Fatalf("own unarchive: %d %v", code, body)
	}
	code, body = doScoped(t, "POST", jobURL(base, wsID, "ops/alpha", "archive"), live, nil)
	if code != http.StatusForbidden || errCode(body) != "forbidden" {
		t.Fatalf("other job: %d %v", code, body)
	}
	code, body = doScoped(t, "POST", agentURL(base, wsID, "cmo", "archive"), live, nil)
	if code != http.StatusForbidden || errCode(body) != "forbidden" {
		t.Fatalf("agent archive by token: %d %v", code, body)
	}
}

func TestJobUnarchiveKeepsPaused(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "ops/old")
	wsID := addWorkspace(t, base, wsDir, "archive-paused")

	if code, body := doReq(t, "POST", jobURL(base, wsID, "ops/old", "pause"), nil, nil); code != http.StatusOK {
		t.Fatalf("pause: %d %v", code, body)
	}
	for _, op := range []string{"archive", "unarchive"} {
		if code, body := doReq(t, "POST", jobURL(base, wsID, "ops/old", op), nil, nil); code != http.StatusOK {
			t.Fatalf("%s: %d %v", op, code, body)
		}
	}
	_, got, _ := doJSON(t, "GET", jobURL(base, wsID, "ops/old", ""), nil)
	if got["active"] != false || got["archived_at"] != nil && got["archived_at"] != "" {
		t.Fatalf("after cycle: active=%v archived_at=%v", got["active"], got["archived_at"])
	}
}

func TestJobUnarchiveRefusedUnderArchivedAgent(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "ops/old")
	wsID := addWorkspace(t, base, wsDir, "archive-owner")

	code, body := doReq(t, "POST", agentURL(base, wsID, "ops", "archive"), nil, nil)
	if code != http.StatusOK || body["name"] != "ops" {
		t.Fatalf("agent archive: %d %v", code, body)
	}
	jobs, _ := body["jobs"].([]any)
	if len(jobs) != 1 || jobs[0] != "ops/old" {
		t.Fatalf("jobs=%v", body["jobs"])
	}
	// The agent flag shows on the job; the JOB.md file stays unchanged.
	_, got, _ := doJSON(t, "GET", jobURL(base, wsID, "ops/old", ""), nil)
	if got["archived_at"] == nil || got["archived_at"] == "" || got["archived_with_agent"] != true {
		t.Fatalf("job under archived agent: %v", got)
	}
	if strings.Contains(readFile(t, filepath.Join(wsDir, "agents/ops/old/JOB.md")), "archived_at") {
		t.Fatal("JOB.md changed by the agent archive")
	}
	code, body = doReq(t, "POST", jobURL(base, wsID, "ops/old", "unarchive"), nil, nil)
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if code != http.StatusConflict || errCode(body) != "archived" || !strings.Contains(msg, "Unarchive the agent") {
		t.Fatalf("unarchive under archived agent: %d %v", code, body)
	}
	if code, body = doReq(t, "POST", agentURL(base, wsID, "ops", "unarchive"), nil, nil); code != http.StatusOK {
		t.Fatalf("agent unarchive: %d %v", code, body)
	}
	_, got, _ = doJSON(t, "GET", jobURL(base, wsID, "ops/old", ""), nil)
	if got["archived_at"] != nil && got["archived_at"] != "" {
		t.Fatalf("job still archived: %v", got)
	}
}

func TestJobArchiveDropsQueue(t *testing.T) {
	_, base, dir := startAPI(t)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	wsID := fireWorkspace(t, base)
	code, res, _ := doJSON(t, "POST", base+"/v1/rules/fire", map[string]any{
		"connector": "qtest", "workspace": wsID, "job": "ops/j", "match": "true", "event": map[string]any{"x": 1},
	})
	qid, _ := res["queue_id"].(string)
	if code != http.StatusOK || qid == "" {
		t.Fatalf("fire: %d %v", code, res)
	}
	code, body := doReq(t, "POST", jobURL(base, wsID, "ops/j", "archive"), nil, nil)
	if code != http.StatusOK || body["queue_dropped"] != float64(1) {
		t.Fatalf("archive: %d %v", code, body)
	}
	var status, result string
	if err := st.DB().QueryRow(`SELECT status, COALESCE(result,'') FROM event_queue WHERE id=?`, qid).Scan(&status, &result); err != nil {
		t.Fatal(err)
	}
	if status != "done" || result != "skipped" {
		t.Fatalf("queue row %s/%s, want done/skipped", status, result)
	}
}

func TestJobPutKeepsArchived(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	jobFile := writeJobFile(t, wsDir, "ops/old")
	wsID := addWorkspace(t, base, wsDir, "archive-put")
	if code, body := doReq(t, "POST", jobURL(base, wsID, "ops/old", "archive"), nil, nil); code != http.StatusOK {
		t.Fatalf("archive: %d %v", code, body)
	}
	code, body := doReq(t, "PUT", jobURL(base, wsID, "ops/old", ""), nil,
		map[string]any{"markdown": "---\nharness: fake\n---\n\nChanged.\n"})
	if code != http.StatusOK {
		t.Fatalf("put: %d %v", code, body)
	}
	txt := readFile(t, jobFile)
	if !strings.Contains(txt, "archived_at:") || !strings.Contains(txt, "Changed.") {
		t.Fatalf("put dropped the flag:\n%s", txt)
	}
	_, got, _ := doJSON(t, "GET", jobURL(base, wsID, "ops/old", ""), nil)
	if got["archived_at"] == nil || got["archived_at"] == "" {
		t.Fatalf("index lost the flag: %v", got)
	}
}

func TestJobPatchArchivedAtRefused(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "ops/old")
	wsID := addWorkspace(t, base, wsDir, "archive-patch")
	code, body := doReq(t, "PATCH", jobURL(base, wsID, "ops/old", ""), nil,
		map[string]any{"archived_at": "2026-11-01T10:00:00Z"})
	if code != http.StatusBadRequest {
		t.Fatalf("patch archived_at: %d %v", code, body)
	}
	code, body = doReq(t, "PATCH", agentURL(base, wsID, "ops", ""), nil,
		map[string]any{"archived_at": "2026-11-01T10:00:00Z"})
	if code != http.StatusBadRequest {
		t.Fatalf("agent patch archived_at: %d %v", code, body)
	}
}

func TestRunArchivedJob409(t *testing.T) {
	_, base, dir := startAPI(t)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	wsID := fireWorkspace(t, base)
	code, first, _ := doJSON(t, "POST", base+"/v1/runs", map[string]any{"workspace": wsID, "job": "ops/j"})
	if code != http.StatusCreated {
		t.Fatalf("first run: %d %v", code, first)
	}
	runID, _ := first["id"].(string)
	if _, err := st.DB().Exec(`UPDATE runs SET status='failed', end_reason='error' WHERE id=?`, runID); err != nil {
		t.Fatal(err)
	}
	if code, body := doReq(t, "POST", jobURL(base, wsID, "ops/j", "archive"), nil, nil); code != http.StatusOK {
		t.Fatalf("archive: %d %v", code, body)
	}
	for name, call := range map[string]func() (int, map[string]any){
		"create": func() (int, map[string]any) {
			return doReq(t, "POST", base+"/v1/runs", nil, map[string]any{"workspace": wsID, "job": "ops/j"})
		},
		"job run": func() (int, map[string]any) {
			return doReq(t, "POST", jobURL(base, wsID, "ops/j", "run"), nil, map[string]any{})
		},
		"replay": func() (int, map[string]any) { return doReq(t, "POST", base+"/v1/runs/"+runID+"/replay", nil, nil) },
		"retry":  func() (int, map[string]any) { return doReq(t, "POST", base+"/v1/runs/"+runID+"/retry", nil, nil) },
	} {
		code, body := call()
		if code != http.StatusConflict || errCode(body) != "archived" {
			t.Fatalf("%s: %d %v, want 409 archived", name, code, body)
		}
	}
	code, body := doReq(t, "POST", jobURL(base, wsID, "ops/j", "run"), nil, map[string]any{"dry_run": true})
	if code != http.StatusOK || body["warning"] != "archived" {
		t.Fatalf("dry run: %d %v", code, body)
	}
}

func TestRulesFireArchived409(t *testing.T) {
	_, base, _ := startAPI(t)
	wsID := fireWorkspace(t, base)
	if code, body := doReq(t, "POST", jobURL(base, wsID, "ops/j", "archive"), nil, nil); code != http.StatusOK {
		t.Fatalf("archive: %d %v", code, body)
	}
	code, body := doReq(t, "POST", base+"/v1/rules/fire", nil, map[string]any{
		"connector": "qtest", "workspace": wsID, "job": "ops/j", "match": "true", "event": map[string]any{"x": 1},
	})
	if code != http.StatusConflict || errCode(body) != "archived" {
		t.Fatalf("fire: %d %v", code, body)
	}
}

func TestAgentArchiveRoute(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "agent-archive")

	code, body := doReq(t, "POST", agentURL(base, wsID, "cmo", "archive"), nil, nil)
	if code != http.StatusOK || body["archived_at"] == nil || body["archived_at"] == "" || body["queue_dropped"] != float64(0) {
		t.Fatalf("archive: %d %v", code, body)
	}
	if !strings.Contains(readFile(t, filepath.Join(wsDir, "agents/cmo/AGENT.md")), "archived_at:") {
		t.Fatal("AGENT.md has no archived_at")
	}
	_, got, _ := doJSON(t, "GET", agentURL(base, wsID, "cmo", ""), nil)
	if got["archived_at"] != body["archived_at"] {
		t.Fatalf("get: %v", got)
	}
	code, body = doReq(t, "POST", agentURL(base, wsID, "ghost", "archive"), nil, nil)
	if code != http.StatusNotFound {
		t.Fatalf("missing agent: %d %v", code, body)
	}
	if code, body = doReq(t, "POST", agentURL(base, wsID, "cmo", "unarchive"), nil, nil); code != http.StatusOK {
		t.Fatalf("unarchive: %d %v", code, body)
	}
	if strings.Contains(readFile(t, filepath.Join(wsDir, "agents/cmo/AGENT.md")), "archived_at") {
		t.Fatal("AGENT.md keeps archived_at")
	}
}

func TestAgentArchivedRefusals(t *testing.T) {
	_, base, dir := startAPI(t)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	wsDir := t.TempDir()
	writeTeamFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "agent-refusals")
	ceoTok, cfoTok := "ceo-token-00000000000000000000000", "cfo-token-00000000000000000000000"
	insertAgentCaller(t, st, wsID, "ceo", ceoTok, "running")
	insertAgentCaller(t, st, wsID, "cfo", cfoTok, "running")
	if code, body := doReq(t, "POST", agentURL(base, wsID, "cfo", "archive"), nil, nil); code != http.StatusOK {
		t.Fatalf("archive: %d %v", code, body)
	}
	cases := []struct {
		name, path, token string
		body              map[string]any
	}{
		{"ask", "cfo/ask", ceoTok, map[string]any{"text": "Budget?"}},
		{"person ask", "cfo/ask", "", map[string]any{"text": "Budget?"}},
		{"handoff", "cfo/handoff", ceoTok, map[string]any{"text": "Take this."}},
		{"escalate", "cfo/escalate", cfoTok, map[string]any{"to": "operator", "reason": "stuck"}},
		{"wake", "cfo/wake", "", map[string]any{}},
	}
	for _, c := range cases {
		code, body := doScoped(t, "POST", agentURL(base, wsID, c.path, ""), c.token, c.body)
		if code != http.StatusConflict || errCode(body) != "archived" {
			t.Fatalf("%s: %d %v, want 409 archived", c.name, code, body)
		}
	}
}

func TestAgentListArchivedFilter(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "agent-list")
	if code, body := doReq(t, "POST", agentURL(base, wsID, "ops", "archive"), nil, nil); code != http.StatusOK {
		t.Fatalf("archive: %d %v", code, body)
	}
	listURL := base + "/v1/workspaces/" + wsID + "/agents"
	names, counts := listNames(t, listURL, "agents")
	if strings.Join(names, ",") != "cmo" {
		t.Fatalf("default: %v", names)
	}
	if counts["current"] != float64(1) || counts["archived"] != float64(1) {
		t.Fatalf("counts=%v", counts)
	}
	names, _ = listNames(t, listURL+"?archived=true", "agents")
	if len(names) != 1 || !strings.HasSuffix(names[0], "ops") {
		t.Fatalf("archived=true: %v", names)
	}
	names, _ = listNames(t, listURL+"?archived=all", "agents")
	if len(names) != 2 {
		t.Fatalf("archived=all: %v", names)
	}
	names, _ = listNames(t, base+"/v1/agents?archived=true", "agents")
	if len(names) != 1 {
		t.Fatalf("all workspaces archived=true: %v", names)
	}
	code, body, _ := doJSON(t, "GET", listURL+"?archived=x", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("bad value: %d %v", code, body)
	}
}

func TestDoctorLayoutSkipsArchived(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFile(t, wsDir, "ops")
	writeLooseJobFile(t, wsDir, "jobs/x")
	writeTestFile(t, filepath.Join(wsDir, "jobs", "y", "JOB.md"), "---\nharness: fake\narchived_at: 2026-11-01T10:00:00Z\n---\n\nOld.\n")
	addWorkspace(t, base, wsDir, "doctor-archived")

	code, body, _ := doJSON(t, "GET", base+"/v1/system/doctor", nil)
	if code != http.StatusOK {
		t.Fatalf("doctor: %d %v", code, body)
	}
	var msgs []string
	rows, _ := body["findings"].([]any)
	for _, r := range rows {
		if m, _ := r.(map[string]any); m["code"] == "job_outside_agent" {
			msgs = append(msgs, m["message"].(string))
		}
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "jobs/x") {
		t.Fatalf("findings = %v, want only jobs/x", msgs)
	}
}

func jobURL(base, wsID, name, op string) string {
	u := base + "/v1/jobs/" + url.PathEscape(name)
	if op != "" {
		u += "/" + op
	}
	return u + "?workspace=" + wsID
}

func agentURL(base, wsID, name, op string) string {
	u := base + "/v1/workspaces/" + wsID + "/agents/" + name
	if op != "" {
		u += "/" + op
	}
	return u
}

func listNames(t *testing.T, u, key string) ([]string, map[string]any) {
	t.Helper()
	code, body, _ := doJSON(t, "GET", u, nil)
	if code != http.StatusOK {
		t.Fatalf("list %s: %d %v", u, code, body)
	}
	var names []string
	rows, _ := body[key].([]any)
	for _, r := range rows {
		row, _ := r.(map[string]any)
		names = append(names, row["name"].(string))
	}
	counts, _ := body["counts"].(map[string]any)
	return names, counts
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
