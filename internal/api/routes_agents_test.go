package api_test

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/runs"
)

// TestAgentPutLifecycle covers the H collection: PUT is the upsert, a bare
// create seeds AGENT.md plus the memory ledger, and list/show/patch carry the
// Part H payload fields.
func TestAgentPutLifecycle(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "agent-crud")

	code, body, _ := doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/cmo", nil)
	if code != 400 || errCode(body) != "validation" || errDetails(body)["field"] != "description" {
		t.Fatalf("create with no description: %d %v want 400 details.field description", code, body)
	}
	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/cmo", map[string]any{"description": "a\nb"})
	if code != 400 || errDetails(body)["field"] != "description" {
		t.Fatalf("two-line description: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/cmo", map[string]any{"description": "Growth plans and campaigns."})
	if code != 201 {
		t.Fatalf("bare create: %d %v", code, body)
	}
	if body["name"] != "cmo" {
		t.Fatalf("bare create names: %v", body)
	}
	seed, err := os.ReadFile(filepath.Join(wsDir, "agents", "cmo", "AGENT.md"))
	if err != nil || !strings.Contains(string(seed), "memory: true") {
		t.Fatalf("seeded AGENT.md wrong: %q %v", seed, err)
	}
	ledger, err := os.ReadFile(filepath.Join(wsDir, "agents", "cmo", "MEMORY.md"))
	if err != nil || !strings.Contains(string(ledger), "# Memory") {
		t.Fatalf("memory ledger not seeded: %q %v", ledger, err)
	}

	// Full replace on the existing agent; empty body must not wipe it.
	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/cmo", map[string]any{
		"content": "---\ndescription: Test agent.\nharness: fake\nmemory: true\n---\n\nRun growth.\n",
	})
	if code != 200 {
		t.Fatalf("full replace: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/cmo", nil)
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("empty replace: %d %v want 400 validation", code, body)
	}
	for _, name := range []string{"Cmo", "bad_name", "-cmo"} {
		code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/"+name, nil)
		if code != 400 || errCode(body) != "validation" {
			t.Errorf("name %q: %d %v want 400 validation", name, code, body)
		}
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents", nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	items, _ := body["agents"].([]any)
	if len(items) != 1 {
		t.Fatalf("list items %d want 1: %v", len(items), body)
	}
	row, _ := items[0].(map[string]any)
	if row["name"] != "cmo" || row["memory"] != true {
		t.Fatalf("list row: %v", row)
	}
	if row["description"] != "Test agent." {
		t.Fatalf("list row description: %v", row["description"])
	}
	if team, _ := row["team"].(map[string]any); team["chains"] != true || team["can_ask"] != nil {
		t.Fatalf("list row team: %v", row["team"])
	}
	for query, want := range map[string]int{"test": 1, "TEST ledger": 1, "invoices": 0, "": 1} {
		code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents?for="+url.QueryEscape(query), nil)
		if got, _ := body["agents"].([]any); code != 200 || len(got) != want {
			t.Fatalf("for=%q: %d %d agents want %d", query, code, len(got), want)
		}
		code, body, _ = doJSON(t, "GET", base+"/v1/agents?for="+url.QueryEscape(query), nil)
		if got, _ := body["agents"].([]any); code != 200 || len(got) != want {
			t.Fatalf("all for=%q: %d %d agents want %d", query, code, len(got), want)
		}
	}
	if _, ok := row["jobs"].([]any); ok {
		t.Fatalf("list row carries jobs before any owned job exists: %v", row)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/cmo", nil)
	if code != 200 {
		t.Fatalf("show: %d %v", code, body)
	}
	if body["markdown"] == "" || body["disabled"] != false || body["job_count"] != float64(0) {
		t.Fatalf("show payload: %v", body)
	}

	code, body, _ = doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID+"/agents/cmo", map[string]any{
		"disabled": true,
	})
	if code != 200 || body["patched"] != true {
		t.Fatalf("patch: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/cmo", nil)
	if code != 200 || body["disabled"] != true {
		t.Fatalf("show after patch: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/nope", nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("unknown agent: %d %v", code, body)
	}
}

// TestAgentDeleteCoversPurgeAndChildren: an agent with jobs needs
// move_jobs_to or purge. A move keeps the job IDs and folders; purge removes
// the whole tree. An agent with no jobs deletes behind the confirm gate.
func TestAgentDeleteCoversPurgeAndChildren(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	duty := filepath.Join(wsDir, "agents", "cmo", "duty", "JOB.md")
	if err := os.WriteFile(duty, []byte("---\nharness: fake\nfolder: SEO\n---\n\nDo the duty.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "agent-del")
	del := base + "/v1/workspaces/" + wsID + "/agents/cmo"

	code, body, _ := doJSON(t, "DELETE", del, nil)
	if code != 409 || errCode(body) != "conflict" || errDetails(body)["jobs"] != float64(1) {
		t.Fatalf("delete with jobs and no choice: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "DELETE", del+"?confirm=1&purge=1&move_jobs_to=ops", nil)
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("both choices: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "DELETE", del+"?confirm=1&move_jobs_to=cmo", nil)
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("move to self: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "DELETE", del+"?confirm=1&move_jobs_to=nope", nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("move to unknown agent: %d %v", code, body)
	}
	for _, tc := range []struct{ query, want string }{
		{"?move_jobs_to=ops", "moves the 1 jobs of agent cmo to ops, then deletes agent cmo and its folder"},
		{"?purge=1", "deletes agent cmo and its 1 jobs; run rows stay"},
	} {
		code, body, _ = doJSON(t, "DELETE", del+tc.query, nil)
		if code != 400 || errDetails(body)["confirm_required"] != true || !strings.Contains(fmt.Sprint(body), tc.want) {
			t.Fatalf("confirm %s: %d %v want %q", tc.query, code, body, tc.want)
		}
	}

	before := jobByName(t, base, wsID, "cmo/duty")
	code, body, _ = doJSON(t, "DELETE", del+"?confirm=1&move_jobs_to=ops", nil)
	if code != 200 || body["purged"] != false {
		t.Fatalf("move delete: %d %v", code, body)
	}
	moved, _ := body["moved"].([]any)
	if len(moved) != 1 || fmt.Sprint(moved[0]) != "map[from:cmo/duty to:ops/duty]" {
		t.Fatalf("moved list: %v", body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cmo")); !os.IsNotExist(err) {
		t.Fatalf("agent folder survived the move delete: %v", err)
	}
	after := jobByName(t, base, wsID, "ops/duty")
	if after["id"] != before["id"] || after["folder"] != "SEO" {
		t.Fatalf("moved job lost its ID or folder: before %v after %v", before, after)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents", nil)
	if items, _ := body["agents"].([]any); code != 200 || len(items) != 1 {
		t.Fatalf("agent list after delete: %d %v", code, body)
	}

	// Purge: recreate with one job, then remove the whole tree.
	code, _, _ = doJSON(t, "PUT", del, map[string]any{
		"content": "---\ndescription: Test agent.\nharness: fake\n---\n\nBack again.\n",
	})
	if code != 201 {
		t.Fatalf("recreate: %d", code)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "name": "again", "agent": "cmo", "harness": "fake", "prompt": "Go.",
	})
	if code != 201 {
		t.Fatalf("job create under cmo: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "DELETE", del+"?confirm=1&purge=1", nil)
	if code != 200 || body["purged"] != true {
		t.Fatalf("purge delete: %d %v", code, body)
	}
	if n, _ := body["files_removed"].(float64); n < 2 {
		t.Fatalf("purge files_removed = %v want >= 2: %v", body["files_removed"], body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cmo")); !os.IsNotExist(err) {
		t.Fatalf("agent folder survived purge: %v", err)
	}

	// An agent with no jobs needs only the confirm.
	code, _, _ = doJSON(t, "PUT", del, map[string]any{"description": "Empty."})
	if code != 201 {
		t.Fatalf("recreate empty: %d", code)
	}
	code, body, _ = doJSON(t, "DELETE", del, nil)
	if code != 400 || !strings.Contains(fmt.Sprint(body), "deletes agent cmo and its folder") {
		t.Fatalf("empty delete without confirm: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "DELETE", del+"?confirm=1", nil)
	if code != 200 || body["purged"] != false {
		t.Fatalf("empty delete: %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cmo")); !os.IsNotExist(err) {
		t.Fatalf("empty agent folder survived: %v", err)
	}
}

// TestAgentMemoryRoutes: the seeded file reads back, log entries append and
// delete, and clear sits behind the confirm gate.
func TestAgentMemoryRoutes(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "agent-mem")
	root := base + "/v1/workspaces/" + wsID + "/agents/cmo/memory"

	code, body, _ := doJSON(t, "GET", root, nil)
	if code != 200 || !strings.Contains(fmt.Sprint(body["content"]), "## Context") {
		t.Fatalf("memory show: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "POST", root+"/log", map[string]any{
		"at": "2026-09-12 09:00", "text": "learned the API",
	})
	if code != 201 {
		t.Fatalf("memory log add: %d %v", code, body)
	}
	entryID := fmt.Sprint(body["id"])
	code, body, _ = doJSON(t, "GET", root, nil)
	if code != 200 {
		t.Fatalf("memory show after add: %d %v", code, body)
	}
	if n := docEntries(t, body, "log"); n != 1 {
		t.Fatalf("entry not appended: %d", n)
	}
	code, body, _ = doJSON(t, "DELETE", root+"/log/"+entryID, nil)
	if code != 200 {
		t.Fatalf("memory log delete: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", root, nil)
	if code != 200 {
		t.Fatalf("memory show after delete: %d %v", code, body)
	}
	if n := docEntries(t, body, "log"); n != 0 {
		t.Fatalf("entry survived delete: %d", n)
	}

	code, body, _ = doJSON(t, "DELETE", root, nil)
	if code != 400 || !strings.Contains(fmt.Sprint(body), "confirm") {
		t.Fatalf("memory clear without confirm: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "DELETE", root+"?confirm=1", nil)
	if code != 200 {
		t.Fatalf("memory clear: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", root, nil)
	if code != 200 || !strings.Contains(fmt.Sprint(body["content"]), "## Context") {
		t.Fatalf("memory not reset to skeleton: %d %v", code, body)
	}
}

// TestAgentMDTypingCoversFileJail: the job-scoped file jail refuses AGENT.md
// with the typed message; the workspace file plane may still write it.
func TestAgentMDTypingCoversFileJail(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "agent-jail")

	code, body, _ := doJSON(t, "PUT", base+"/v1/jobs/ops%2Falpha/files/AGENT.md?workspace="+wsID, map[string]any{
		"content": "---\ndescription: Test agent.\n---\n\nSneak.\n",
	})
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("job-scoped AGENT.md put: %d %v want 409 conflict", code, body)
	}
	if !strings.Contains(fmt.Sprint(body), "use the agent routes") {
		t.Fatalf("typed message missing: %v", body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "ops", "alpha", "AGENT.md")); !os.IsNotExist(err) {
		t.Fatal("job-scoped AGENT.md put leaked to disk")
	}

	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/agents/cmo/notes.md", map[string]any{
		"content": "plain file\n",
	})
	if code != 201 {
		t.Fatalf("workspace-scoped plain file: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/agents/cmo/AGENT.md", map[string]any{
		"content": "---\ndescription: Test agent.\nharness: fake\n---\n\nVia the file plane.\n",
	})
	if code != 200 {
		t.Fatalf("workspace file plane must accept AGENT.md: %d %v", code, body)
	}
}

// TestJobsOwnerFilter: owner=cmo and owner=cmo both keep only the
// children of that agent.
func TestJobsOwnerFilter(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "agent-owner")

	for _, owner := range []string{"cmo", "cmo"} {
		code, body, _ := doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID+"&owner="+owner, nil)
		if code != 200 {
			t.Fatalf("owner=%s: %d %v", owner, code, body)
		}
		jobs, _ := body["jobs"].([]any)
		if len(jobs) != 1 {
			t.Fatalf("owner=%s items %d want 1: %v", owner, len(jobs), body)
		}
		if got := jobs[0].(map[string]any)["name"]; got != "cmo/duty" {
			t.Fatalf("owner=%s name=%v", owner, got)
		}
	}

	// No filter: duties only, agent row never leaks into the jobs list.
	code, body, _ := doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("plain list: %d %v", code, body)
	}
	jobs, _ := body["jobs"].([]any)
	if len(jobs) != 2 {
		t.Fatalf("plain list items %d want 2 (duty + alpha): %v", len(jobs), body)
	}
	for _, j := range jobs {
		if name := j.(map[string]any)["name"]; name == "cmo" {
			t.Fatalf("agent row leaked into /v1/jobs: %v", body)
		}
	}
}

// TestJobCreateUnderAgent: the agent field lands the file under the agent
// folder, the wire name is the full relative path, and a missing agent 404s.
func TestJobCreateUnderAgent(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "agent-child")

	code, body, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "agent": "cmo", "name": "weekly-digest",
		"prompt": "Write the digest.", "harness": "fake",
	})
	if code != 201 {
		t.Fatalf("create under agent: %d %v", code, body)
	}
	if body["name"] != "cmo/weekly-digest" {
		t.Fatalf("wire name = %v want cmo/weekly-digest", body["name"])
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cmo", "weekly-digest", "JOB.md")); err != nil {
		t.Fatalf("JOB.md not under agent folder: %v", err)
	}

	// The agent show groups it: job_count and jobs carry the child.
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/cmo", nil)
	if code != 200 {
		t.Fatalf("agent show: %d %v", code, body)
	}
	if body["job_count"] != float64(2) {
		t.Fatalf("job_count = %v want 2: %v", body["job_count"], body)
	}
	jobs, _ := body["jobs"].([]any)
	if len(jobs) != 2 {
		t.Fatalf("jobs = %v", body["jobs"])
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "agent": "ghost", "name": "nope", "prompt": "x",
	})
	if code != 404 || errCode(body) != "not_found" || !strings.Contains(fmt.Sprint(body), "agent not found") {
		t.Fatalf("missing agent: %d %v want 404 agent not found", code, body)
	}
}

// TestAgentShowKeepsLayoutParseError: a JOB.md beside AGENT.md stamps a layout
// error on the index row; show and list must keep it beside the live parse.
func TestAgentShowKeepsLayoutParseError(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	if err := os.WriteFile(filepath.Join(wsDir, "agents", "cmo", "JOB.md"), []byte("---\ntitle: Stray\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "agent-layout")

	code, body, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/cmo", nil)
	if code != 200 {
		t.Fatalf("show: %d %v", code, body)
	}
	if pe, _ := body["parse_error"].(string); !strings.Contains(pe, "same directory") {
		t.Fatalf("show parse_error = %q want layout error", pe)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents", nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	var cmo map[string]any
	items, _ := body["agents"].([]any)
	for _, it := range items {
		if m, _ := it.(map[string]any); m["name"] == "cmo" {
			cmo = m
		}
	}
	if cmo == nil {
		t.Fatalf("list has no cmo: %v", body)
	}
	if pe, _ := cmo["parse_error"].(string); !strings.Contains(pe, "same directory") {
		t.Fatalf("list parse_error = %q want layout error", pe)
	}
}

// TestWakeDryRunComposesPreview: an operator dry run previews the composed
// wake prompt, not the raw operator body.
func TestWakeDryRunComposesPreview(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	if err := os.WriteFile(filepath.Join(wsDir, "TREBI.md"), []byte("---\nharness: fake\n---\n\nLook after the workspace.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "wake-dry")

	code, body, _ := doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/agents/cmo/wake", map[string]any{
		"dry_run": true,
	})
	if code != 200 || body["dry_run"] != true {
		t.Fatalf("agent dry run: %d %v", code, body)
	}
	preview, _ := body["prompt_preview"].(string)
	if !strings.Contains(preview, "Run growth.") || !strings.Contains(preview, "# Your jobs") {
		t.Fatalf("agent dry run preview not composed: %q", preview)
	}

}

// TestAgentWakeSugarRoute: POST /agents/{name}/wake pins the identity fields
// and lands in the agent lane with a scoped prompt.
func TestAgentWakeSugarRoute(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "agent-wake")

	code, run, _ := doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/agents/cmo/wake", map[string]any{})
	if code != 201 {
		t.Fatalf("wake: %d %v", code, run)
	}
	if run["kind"] != "agent" || run["job_name"] != "cmo" {
		t.Fatalf("wake run identity: kind=%v job_name=%v", run["kind"], run["job_name"])
	}
	preview, _ := run["prompt_preview"].(string)
	if !strings.Contains(preview, "Run growth.") || !strings.Contains(preview, "# Your jobs") {
		t.Fatalf("wake preview not agent-scoped: %q", preview)
	}
	if strings.Contains(preview, "alpha") {
		t.Fatalf("wake preview leaks workspace jobs: %q", preview)
	}

	code, body, _ := doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/agents/ghost/wake", map[string]any{})
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("wake unknown agent: %d %v", code, body)
	}
}

// docEntries counts doc.<section> entries in a GET memory payload.
func docEntries(t *testing.T, body map[string]any, section string) int {
	t.Helper()
	doc, ok := body["doc"].(map[string]any)
	if !ok {
		t.Fatalf("no doc in payload: %v", body)
	}
	list, _ := doc[section].([]any)
	return len(list)
}

// TestAgentListRecentRuns: the list path fills recent_runs (last 20,
// oldest first) and the 7-day counts from the same run walk, and the
// single GET leaves the list-only fields empty (plan 2026-09-24 G.1).
func TestAgentListRecentRuns(t *testing.T) {
	s, base, dir := startAPI(t)
	wsDir := t.TempDir()
	for path, md := range map[string]string{
		filepath.Join(wsDir, "agents", "cmo", "AGENT.md"):  "---\ndescription: Test agent.\nharness: fake\n---\n\nRun growth.\n",
		filepath.Join(wsDir, "agents", "beta", "AGENT.md"): "---\ndescription: Test agent.\nharness: fake\n---\n\nBeta.\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wsID := addWorkspace(t, base, wsDir, "agent-runs")
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	defer st.Close()

	// Spaced ids keep the newest-first list order deterministic: ULIDs
	// minted in the same millisecond order by entropy, not by time.
	mk := func(agent, status string, end runs.EndReason) string {
		t.Helper()
		run, err := s.Runs().Create(t.Context(), runs.CreateOpts{
			WorkspaceID: wsID, JobID: indexedJobID(t, st, wsID, agent), JobName: agent, Kind: runs.KindAgent, Harness: "fake",
		})
		if err != nil {
			t.Fatal(err)
		}
		if status != "" {
			if err := s.Runs().Terminate(t.Context(), run.ID, runs.Status(status), end, nil); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(3 * time.Millisecond)
		return run.ID
	}
	c1 := mk("cmo", "complete", runs.EndResult)
	c2 := mk("cmo", "failed", runs.EndFailed)
	c3 := mk("cmo", "", runs.EndSentinel) // stays running
	b1 := mk("beta", "complete", runs.EndResult)

	code, body, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents", nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	rows := map[string]map[string]any{}
	for _, it := range body["agents"].([]any) {
		row := it.(map[string]any)
		rows[row["name"].(string)] = row
	}
	cmo := rows["cmo"]
	got := cmo["recent_runs"].([]any)
	if len(got) != 3 {
		t.Fatalf("cmo recent_runs: %v", cmo["recent_runs"])
	}
	wantIDs := []string{c1, c2, c3} // oldest first
	for i, bar := range got {
		entry := bar.(map[string]any)
		if entry["id"] != wantIDs[i] {
			t.Fatalf("recent_runs order: %v", got)
		}
		if s, _ := entry["status"].(string); s == "" {
			t.Fatalf("recent_runs status missing: %v", entry)
		}
		if s, _ := entry["started_at"].(string); s == "" {
			t.Fatalf("recent_runs started_at missing: %v", entry)
		}
	}
	if got[0].(map[string]any)["status"] != "complete" || got[2].(map[string]any)["status"] != "running" {
		t.Fatalf("recent_runs statuses: %v", got)
	}
	if cmo["runs_7d"] != float64(3) || cmo["ok_7d"] != float64(1) {
		t.Fatalf("cmo 7d counts: runs_7d=%v ok_7d=%v", cmo["runs_7d"], cmo["ok_7d"])
	}
	beta := rows["beta"]
	if bars := beta["recent_runs"].([]any); len(bars) != 1 || bars[0].(map[string]any)["id"] != b1 {
		t.Fatalf("beta recent_runs: %v", beta["recent_runs"])
	}
	if beta["runs_7d"] != float64(1) || beta["ok_7d"] != float64(1) {
		t.Fatalf("beta 7d counts: runs_7d=%v ok_7d=%v", beta["runs_7d"], beta["ok_7d"])
	}

	// The single-agent GET leaves the list-only fields empty.
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/cmo", nil)
	if code != 200 || body["recent_runs"] != nil || body["runs_7d"] != float64(0) || body["ok_7d"] != float64(0) {
		t.Fatalf("show must leave run fields empty: %d %v %v", code, body["recent_runs"], body["runs_7d"])
	}
}

func TestWorkspaceChainSettings(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "chain-settings")

	code, body, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID, nil)
	chains, _ := body["chains"].(map[string]any)
	if code != 200 || chains["enabled"] != true || chains["max_hops"] != float64(5) || chains["max_turns"] != float64(20) || chains["pair_per_hour"] != float64(10) {
		t.Fatalf("default chains: %d %v", code, body["chains"])
	}
	code, body, _ = doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID, map[string]any{"chains": map[string]any{"max_turns": 40, "enabled": false}})
	chains, _ = body["chains"].(map[string]any)
	if code != 200 || chains["enabled"] != false || chains["max_turns"] != float64(40) || chains["max_hops"] != float64(5) {
		t.Fatalf("patch chains: %d %v", code, body)
	}
	data, _ := os.ReadFile(filepath.Join(wsDir, "trebi.yaml"))
	if !strings.Contains(string(data), "max_turns: 40") || strings.Contains(string(data), "max_hops") {
		t.Fatalf("trebi.yaml = %s", data)
	}
	code, body, _ = doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID, map[string]any{"chains": map[string]any{"max_hops": 21}})
	if code != 400 || errCode(body) != "validation" || errDetails(body)["field"] != "chains" {
		t.Fatalf("out of range: %d %v", code, body)
	}
}

// TestAgentRenameDryRunAndAskers: a dry run moves nothing and names the
// agents whose team.can_ask lists the old name.
func TestAgentRenameDryRunAndAskers(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	for name, md := range map[string]string{
		"cmo": "---\ndescription: CMO.\n---\n\nMarket.\n",
		"cfo": "---\ndescription: CFO.\nteam:\n  can_ask: [cmo]\n---\n\nCount.\n",
	} {
		dir := filepath.Join(wsDir, "agents", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wsID := addWorkspace(t, base, wsDir, "rename-agents")
	path := base + "/v1/workspaces/" + wsID + "/agents/cmo/rename"

	code, body, _ := doJSON(t, "POST", path+"?dry_run=1", map[string]any{"to": "growth"})
	if code != 200 || body["dry_run"] != true || body["renamed"] != false || fmt.Sprint(body["askers"]) != "[cfo]" {
		t.Fatalf("dry run: %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cmo", "AGENT.md")); err != nil {
		t.Fatalf("dry run moved the folder: %v", err)
	}
	if code, body, _ := doJSON(t, "POST", path+"?dry_run=1", map[string]any{"to": "cfo"}); code != 409 {
		t.Fatalf("dry run conflict: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "POST", path, map[string]any{"to": "growth"})
	if code != 200 || body["renamed"] != true || body["to"] != "growth" {
		t.Fatalf("rename: %d %v", code, body)
	}
}

func TestAgentReservedNameNobody(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	dir := filepath.Join(wsDir, "agents", "cmo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("---\ndescription: CMO.\n---\n\nMarket.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "reserved-agents")
	code, body, _ := doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/nobody", map[string]any{"description": "No one."})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("create nobody: %d %v want 400 validation", code, body)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/agents/cmo/rename", map[string]any{"to": "nobody"})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("rename to nobody: %d %v want 400 validation", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "nobody")); err == nil {
		t.Fatal("nobody folder exists")
	}
}

// jobByName returns the list row of one job.
func jobByName(t *testing.T, base, wsID, name string) map[string]any {
	t.Helper()
	code, body, _ := doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("job list: %d %v", code, body)
	}
	jobs, _ := body["jobs"].([]any)
	for _, j := range jobs {
		if row, _ := j.(map[string]any); row["name"] == name {
			return row
		}
	}
	t.Fatalf("job %s not in list: %v", name, body)
	return nil
}

func TestAgentReservedNameAssistant(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "reserved-assistant")
	code, body, _ := doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/assistant", map[string]any{"description": "Not allowed."})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("create assistant: %d %v want 400 validation", code, body)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/agents/cmo/rename", map[string]any{"to": "assistant"})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("rename to assistant: %d %v want 400 validation", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "assistant")); err == nil {
		t.Fatal("assistant folder exists")
	}
}

func TestAgentPayloadHasNoIndexName(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "no-index-name")
	agents := base + "/v1/workspaces/" + wsID + "/agents"
	noKeys := func(what string, body map[string]any, keys ...string) {
		t.Helper()
		for _, k := range keys {
			if _, ok := body[k]; ok {
				t.Errorf("%s reply has %q: %v", what, k, body)
			}
		}
	}

	code, body, _ := doJSON(t, "GET", agents+"/cmo", nil)
	if code != 200 || body["name"] != "cmo" {
		t.Fatalf("get: %d %v", code, body)
	}
	noKeys("get", body, "index_name")
	code, body, _ = doJSON(t, "GET", agents, nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	rows, _ := body["agents"].([]any)
	if len(rows) == 0 {
		t.Fatalf("list has no agents: %v", body)
	}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		noKeys("list row", row, "index_name")
	}
	code, body, _ = doJSON(t, "PUT", agents+"/cmo", map[string]any{"content": "---\ndescription: Test agent.\nharness: fake\n---\n\nRun growth.\n"})
	if code != 200 {
		t.Fatalf("put: %d %v", code, body)
	}
	noKeys("put", body, "index_name")
	code, body, _ = doJSON(t, "POST", agents+"/ops/archive", nil)
	if code != 200 || body["name"] != "ops" {
		t.Fatalf("archive: %d %v", code, body)
	}
	noKeys("archive", body, "index_name")
	code, body, _ = doJSON(t, "POST", agents+"/cmo/rename", map[string]any{"to": "growth"})
	if code != 200 || body["to"] != "growth" {
		t.Fatalf("rename: %d %v", code, body)
	}
	noKeys("rename", body, "from_index", "to_index", "index_name")
	code, body, _ = doJSON(t, "DELETE", agents+"/growth?purge=1&confirm=true", nil)
	if code != 200 {
		t.Fatalf("delete: %d %v", code, body)
	}
	noKeys("delete", body, "index_name")
}

func TestOldPrefixIsNotFound(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "old-prefix")
	for _, path := range []string{"agents%2Fcmo%2Fduty", "agents/cmo/duty", "agents%2Fcmo"} {
		code, body, _ := doJSON(t, "GET", base+"/v1/jobs/"+path+"?workspace="+wsID, nil)
		if code != 404 || errCode(body) != "not_found" {
			t.Errorf("GET /v1/jobs/%s: %d %v want 404 not_found", path, code, body)
		}
	}
	if code, body, _ := doJSON(t, "GET", base+"/v1/jobs/cmo/duty?workspace="+wsID, nil); code != 200 || body["name"] != "cmo/duty" {
		t.Fatalf("new form: %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "GET", base+"/v1/jobs/cmo%2Fduty?workspace="+wsID, nil); code != 200 || body["name"] != "cmo/duty" {
		t.Fatalf("escaped form: %d %v", code, body)
	}
}

func TestJobCreateSplitsOwner(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "split-owner")
	code, body, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{"workspace": wsID, "name": "cmo/weekly", "prompt": "Go.", "harness": "fake"})
	if code != 201 || body["name"] != "cmo/weekly" {
		t.Fatalf("name with owner: %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cmo", "weekly", "JOB.md")); err != nil {
		t.Fatalf("JOB.md not in the agent folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "cmo")); err == nil {
		t.Fatal("job folder written outside agents/")
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/jobs", map[string]any{"workspace": wsID, "name": "cmo/monthly", "agent": "ops", "prompt": "Go.", "harness": "fake"})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("owner mismatch: %d %v want 400 validation", code, body)
	}
}
