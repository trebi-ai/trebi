package api_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentPutLifecycle covers the H collection: PUT is the upsert, a bare
// create seeds AGENT.md plus the memory ledger, and list/show/patch carry the
// Part H payload fields.
func TestAgentPutLifecycle(t *testing.T) {
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, s.Token(), wsDir, "agent-crud")

	code, body, _ := doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/cmo", s.Token(), nil)
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
	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/cmo", s.Token(), map[string]any{
		"content": "---\nharness: fake\nmemory: true\n---\n\nRun growth.\n",
	})
	if code != 200 {
		t.Fatalf("full replace: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/cmo", s.Token(), nil)
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("empty replace: %d %v want 400 validation", code, body)
	}
	for _, name := range []string{"Cmo", "bad_name", "-cmo"} {
		code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/"+name, s.Token(), nil)
		if code != 400 || errCode(body) != "validation" {
			t.Errorf("name %q: %d %v want 400 validation", name, code, body)
		}
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents", s.Token(), nil)
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

	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/cmo", s.Token(), nil)
	if code != 200 {
		t.Fatalf("show: %d %v", code, body)
	}
	if body["markdown"] == "" || body["disabled"] != false || body["job_count"] != float64(0) {
		t.Fatalf("show payload: %v", body)
	}

	code, body, _ = doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID+"/agents/cmo", s.Token(), map[string]any{
		"disabled": true,
	})
	if code != 200 || body["patched"] != true {
		t.Fatalf("patch: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/cmo", s.Token(), nil)
	if code != 200 || body["disabled"] != true {
		t.Fatalf("show after patch: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/nope", s.Token(), nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("unknown agent: %d %v", code, body)
	}
}

// TestAgentDeleteCoversPurgeAndChildren: plain delete removes AGENT.md only,
// so the owned jobs stay on disk and keep the owner stamp; purge removes the
// whole tree. Both need confirm.
func TestAgentDeleteCoversPurgeAndChildren(t *testing.T) {
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, s.Token(), wsDir, "agent-del")
	token := s.Token()

	code, body, _ := doJSON(t, "DELETE", base+"/v1/workspaces/"+wsID+"/agents/cmo", token, nil)
	if code != 400 || errCode(body) != "validation" || !strings.Contains(fmt.Sprint(body), "confirm_required") {
		t.Fatalf("delete without confirm: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "DELETE", base+"/v1/workspaces/"+wsID+"/agents/cmo?confirm=1", token, nil)
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
	code, body, _ = doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID+"&owner=cmo", token, nil)
	if code != 200 {
		t.Fatalf("owner list after delete: %d %v", code, body)
	}
	jobs, _ := body["jobs"].([]any)
	if len(jobs) != 1 || jobs[0].(map[string]any)["name"] != "agents/cmo/duty" {
		t.Fatalf("orphaned owned job not listed: %v", body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents", token, nil)
	if code != 200 {
		t.Fatalf("agent list after delete: %d %v", code, body)
	}
	if items, _ := body["agents"].([]any); len(items) != 0 {
		t.Fatalf("agent row survived AGENT.md delete: %v", body)
	}

	// Purge: recreate, then remove the whole tree.
	code, _, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/cmo", token, map[string]any{
		"content": "---\nharness: fake\n---\n\nBack again.\n",
	})
	if code != 201 {
		t.Fatalf("recreate: %d", code)
	}
	code, body, _ = doJSON(t, "DELETE", base+"/v1/workspaces/"+wsID+"/agents/cmo?confirm=1&purge=1", token, nil)
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

// TestAgentMemoryRoutes: the seeded ledger reads back, entries append and
// delete, and memory_clear sits behind the confirm gate.
func TestAgentMemoryRoutes(t *testing.T) {
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	// The fixture AGENT.md has no memory flag; write the ledger explicitly.
	if err := os.WriteFile(filepath.Join(wsDir, "agents", "cmo", "MEMORY.md"), []byte("# Memory\n\nDated notes follow below.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, s.Token(), wsDir, "agent-mem")
	token := s.Token()
	root := base + "/v1/workspaces/" + wsID + "/agents/cmo/memory"

	code, body, _ := doJSON(t, "GET", root, token, nil)
	if code != 200 || !strings.Contains(fmt.Sprint(body["content"]), "# Memory") {
		t.Fatalf("memory show: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "POST", root+"/entries", token, map[string]any{
		"date": "2026-09-12", "text": "learned the API",
	})
	if code != 201 {
		t.Fatalf("memory add: %d %v", code, body)
	}
	entryID := fmt.Sprint(body["id"])
	code, body, _ = doJSON(t, "GET", root, token, nil)
	if code != 200 {
		t.Fatalf("memory show after add: %d %v", code, body)
	}
	if entries, _ := body["entries"].([]any); len(entries) != 1 {
		t.Fatalf("entry not appended: %v", body)
	}
	code, body, _ = doJSON(t, "DELETE", root+"/entries/"+entryID, token, nil)
	if code != 200 {
		t.Fatalf("memory entry delete: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", root, token, nil)
	if code != 200 {
		t.Fatalf("memory show after delete: %d %v", code, body)
	}
	if entries, _ := body["entries"].([]any); len(entries) != 0 {
		t.Fatalf("entry survived delete: %v", body)
	}

	code, body, _ = doJSON(t, "DELETE", root, token, nil)
	if code != 400 || !strings.Contains(fmt.Sprint(body), "confirm_required") {
		t.Fatalf("memory clear without confirm: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "DELETE", root+"?confirm=1", token, nil)
	if code != 200 {
		t.Fatalf("memory clear: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", root, token, nil)
	if code != 200 || fmt.Sprint(body["content"]) != "" {
		t.Fatalf("memory not cleared: %d %v", code, body)
	}
}

// TestAgentMDTypingCoversFileJail: the job-scoped file jail refuses AGENT.md
// with the typed message; the workspace file plane may still write it.
func TestAgentMDTypingCoversFileJail(t *testing.T) {
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, s.Token(), wsDir, "agent-jail")
	token := s.Token()

	code, body, _ := doJSON(t, "PUT", base+"/v1/jobs/alpha/files/AGENT.md?workspace="+wsID, token, map[string]any{
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

	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/agents/cmo/notes.md", token, map[string]any{
		"content": "plain file\n",
	})
	if code != 201 {
		t.Fatalf("workspace-scoped plain file: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/agents/cmo/AGENT.md", token, map[string]any{
		"content": "---\nharness: fake\n---\n\nVia the file plane.\n",
	})
	if code != 200 {
		t.Fatalf("workspace file plane must accept AGENT.md: %d %v", code, body)
	}
}

// TestJobsOwnerFilter: owner=cmo and owner=agents/cmo both keep only the
// children of that agent.
func TestJobsOwnerFilter(t *testing.T) {
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, s.Token(), wsDir, "agent-owner")
	token := s.Token()

	for _, owner := range []string{"cmo", "agents/cmo"} {
		code, body, _ := doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID+"&owner="+owner, token, nil)
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
	code, body, _ := doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID, token, nil)
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
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, s.Token(), wsDir, "agent-child")
	token := s.Token()

	code, body, _ := doJSON(t, "POST", base+"/v1/jobs", token, map[string]any{
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
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/cmo", token, nil)
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

	code, body, _ = doJSON(t, "POST", base+"/v1/jobs", token, map[string]any{
		"workspace": wsID, "agent": "ghost", "name": "nope", "prompt": "x",
	})
	if code != 404 || errCode(body) != "not_found" || !strings.Contains(fmt.Sprint(body), "agent not found") {
		t.Fatalf("missing agent: %d %v want 404 agent not found", code, body)
	}
}

// TestAgentWakeSugarRoute: POST /agents/{name}/wake pins the identity fields
// and lands in the agent lane with a scoped prompt.
func TestAgentWakeSugarRoute(t *testing.T) {
	s, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, s.Token(), wsDir, "agent-wake")
	token := s.Token()

	code, run, _ := doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/agents/cmo/wake", token, map[string]any{})
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

	code, body, _ := doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/agents/ghost/wake", token, map[string]any{})
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("wake unknown agent: %d %v", code, body)
	}
}
