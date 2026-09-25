package api_test

import (
	"fmt"
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
	if code != 201 {
		t.Fatalf("bare create: %d %v", code, body)
	}
	if body["name"] != "cmo" || body["index_name"] != "agents/cmo" {
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
		"content": "---\nharness: fake\nmemory: true\n---\n\nRun growth.\n",
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
	if row["name"] != "cmo" || row["index_name"] != "agents/cmo" || row["memory"] != true {
		t.Fatalf("list row: %v", row)
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

// TestAgentDeleteCoversPurgeAndChildren: plain delete removes AGENT.md only,
// so the owned jobs stay on disk and keep the owner stamp; purge removes the
// whole tree. Both need confirm.
func TestAgentDeleteCoversPurgeAndChildren(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "agent-del")

	code, body, _ := doJSON(t, "DELETE", base+"/v1/workspaces/"+wsID+"/agents/cmo", nil)
	if code != 400 || errCode(body) != "validation" || !strings.Contains(fmt.Sprint(body), "confirm_required") {
		t.Fatalf("delete without confirm: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "DELETE", base+"/v1/workspaces/"+wsID+"/agents/cmo?confirm=1", nil)
	if code != 200 {
		t.Fatalf("plain delete: %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cmo", "AGENT.md")); !os.IsNotExist(err) {
		t.Fatalf("AGENT.md survived plain delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cmo", "duty", "JOB.md")); err != nil {
		t.Fatalf("owned job died with plain delete: %v", err)
	}
	// The duty job stays a duty: still listed, still owned, agent gone.
	code, body, _ = doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID+"&owner=cmo", nil)
	if code != 200 {
		t.Fatalf("owner list after delete: %d %v", code, body)
	}
	jobs, _ := body["jobs"].([]any)
	if len(jobs) != 1 || jobs[0].(map[string]any)["name"] != "agents/cmo/duty" {
		t.Fatalf("orphaned owned job not listed: %v", body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents", nil)
	if code != 200 {
		t.Fatalf("agent list after delete: %d %v", code, body)
	}
	if items, _ := body["agents"].([]any); len(items) != 0 {
		t.Fatalf("agent row survived AGENT.md delete: %v", body)
	}

	// Purge: recreate, then remove the whole tree.
	code, _, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/cmo", map[string]any{
		"content": "---\nharness: fake\n---\n\nBack again.\n",
	})
	if code != 201 {
		t.Fatalf("recreate: %d", code)
	}
	code, body, _ = doJSON(t, "DELETE", base+"/v1/workspaces/"+wsID+"/agents/cmo?confirm=1&purge=1", nil)
	if code != 200 || body["purged"] != true {
		t.Fatalf("purge delete: %d %v", code, body)
	}
	if n, _ := body["files_removed"].(float64); n < 2 {
		t.Fatalf("purge files_removed = %v want >= 2: %v", body["files_removed"], body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cmo")); !os.IsNotExist(err) {
		t.Fatalf("agent folder survived purge: %v", err)
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

	code, body, _ := doJSON(t, "PUT", base+"/v1/jobs/alpha/files/AGENT.md?workspace="+wsID, map[string]any{
		"content": "---\n---\n\nSneak.\n",
	})
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("job-scoped AGENT.md put: %d %v want 409 conflict", code, body)
	}
	if !strings.Contains(fmt.Sprint(body), "use the agent routes") {
		t.Fatalf("typed message missing: %v", body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "alpha", "AGENT.md")); !os.IsNotExist(err) {
		t.Fatal("job-scoped AGENT.md put leaked to disk")
	}

	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/agents/cmo/notes.md", map[string]any{
		"content": "plain file\n",
	})
	if code != 201 {
		t.Fatalf("workspace-scoped plain file: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/agents/cmo/AGENT.md", map[string]any{
		"content": "---\nharness: fake\n---\n\nVia the file plane.\n",
	})
	if code != 200 {
		t.Fatalf("workspace file plane must accept AGENT.md: %d %v", code, body)
	}
}

// TestJobsOwnerFilter: owner=cmo and owner=agents/cmo both keep only the
// children of that agent.
func TestJobsOwnerFilter(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "agent-owner")

	for _, owner := range []string{"cmo", "agents/cmo"} {
		code, body, _ := doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID+"&owner="+owner, nil)
		if code != 200 {
			t.Fatalf("owner=%s: %d %v", owner, code, body)
		}
		jobs, _ := body["jobs"].([]any)
		if len(jobs) != 1 {
			t.Fatalf("owner=%s items %d want 1: %v", owner, len(jobs), body)
		}
		if got := jobs[0].(map[string]any)["name"]; got != "agents/cmo/duty" {
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
		if name := j.(map[string]any)["name"]; name == "agents/cmo" {
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
	if body["name"] != "agents/cmo/weekly-digest" {
		t.Fatalf("wire name = %v want agents/cmo/weekly-digest", body["name"])
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
	items, _ := body["agents"].([]any)
	if len(items) != 1 {
		t.Fatalf("list items %d want 1: %v", len(items), body)
	}
	if pe, _ := items[0].(map[string]any)["parse_error"].(string); !strings.Contains(pe, "same directory") {
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

	code, body, _ = doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/assistant/wake", map[string]any{
		"dry_run": true,
	})
	if code != 200 || body["dry_run"] != true {
		t.Fatalf("assistant dry run: %d %v", code, body)
	}
	preview, _ = body["prompt_preview"].(string)
	if !strings.Contains(preview, "# Agents") || !strings.Contains(preview, "cmo") {
		t.Fatalf("assistant dry run preview not composed: %q", preview)
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
	if run["kind"] != "agent" || run["job"] != "agents/cmo" {
		t.Fatalf("wake run identity: kind=%v job=%v", run["kind"], run["job"])
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
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	for path, md := range map[string]string{
		filepath.Join(wsDir, "agents", "cmo", "AGENT.md"):  "---\nharness: fake\n---\n\nRun growth.\n",
		filepath.Join(wsDir, "agents", "beta", "AGENT.md"): "---\nharness: fake\n---\n\nBeta.\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wsID := addWorkspace(t, base, wsDir, "agent-runs")

	// Spaced ids keep the newest-first list order deterministic: ULIDs
	// minted in the same millisecond order by entropy, not by time.
	mk := func(agent, status string, end runs.EndReason) string {
		t.Helper()
		run, err := s.Runs().Create(t.Context(), runs.CreateOpts{
			WorkspaceID: wsID, Job: "agents/" + agent, Kind: runs.KindAgent, Harness: "fake",
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

// TestAgentPayloadEnvSets: the agent payloads carry the frontmatter envset
// list as env_sets and keep env_set as the first value for one release
// (plan 2026-09-24 H.3).
func TestAgentPayloadEnvSets(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "agent-envsets")

	put := func(content string) {
		t.Helper()
		code, body, _ := doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/cmo", map[string]any{
			"content": content,
		})
		if code != 201 && code != 200 {
			t.Fatalf("put: %d %v", code, body)
		}
	}
	check := func(stage string, body map[string]any, wantSets []string, wantSet string) {
		t.Helper()
		sets, _ := body["env_sets"].([]any)
		if len(sets) != len(wantSets) {
			t.Fatalf("%s env_sets: %v", stage, body["env_sets"])
		}
		for i, w := range wantSets {
			if sets[i] != w {
				t.Fatalf("%s env_sets: %v", stage, sets)
			}
		}
		if body["env_set"] != wantSet {
			t.Fatalf("%s env_set: %v want %q", stage, body["env_set"], wantSet)
		}
	}

	put("---\nharness: fake\nenvset:\n  - ship\n  - watch\n---\n\nRun.\n")
	code, body, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/cmo", nil)
	if code != 200 {
		t.Fatalf("show: %d %v", code, body)
	}
	check("show", body, []string{"ship", "watch"}, "ship")

	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents", nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	items := body["agents"].([]any)
	if len(items) != 1 {
		t.Fatalf("list items %d want 1: %v", len(items), body)
	}
	check("list", items[0].(map[string]any), []string{"ship", "watch"}, "ship")

	put("---\nharness: fake\nenvset: only\n---\n\nRun.\n")
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/cmo", nil)
	check("scalar", body, []string{"only"}, "only")

	put("---\nharness: fake\n---\n\nRun.\n")
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/cmo", nil)
	check("absent", body, nil, "")
}
