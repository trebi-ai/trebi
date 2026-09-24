package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentMarkdownProposals(t *testing.T) {
	_, base, dir := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "proposals")
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	live := "proposal-token-000000000000000000"
	insertAgentCaller(t, st, wsID, "cmo", live, "running")

	agentPath := filepath.Join(wsDir, "agents", "cmo", "AGENT.md")
	next := "---\ntitle: CMO\nemail:\n  accept:\n    - \"ops@example.com\"\n---\n\nRun growth safely.\n"

	// Three write paths create proposals. Files and agent PUT/PATCH never write.
	code, body := doScoped(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/agents/cmo/AGENT.md", live, map[string]any{"content": next})
	if code != http.StatusAccepted || body["proposal_id"] == "" {
		t.Fatalf("files proposal: %d %v", code, body)
	}
	filesID := body["proposal_id"].(string)
	if got, _ := os.ReadFile(agentPath); strings.Contains(string(got), "ops@example.com") {
		t.Fatal("files proposal wrote directly")
	}

	code, body = doScoped(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/cmo", live, map[string]any{"content": next})
	if code != http.StatusAccepted || body["proposal_id"] == "" {
		t.Fatalf("agent put proposal: %d %v", code, body)
	}

	code, body = doScoped(t, "PATCH", base+"/v1/workspaces/"+wsID+"/agents/cmo", live, map[string]any{"title": "Growth CMO", "email": map[string]any{"accept": []string{"ops@example.com"}}})
	if code != http.StatusAccepted || body["proposal_id"] == "" || body["protected_keys"] == nil {
		t.Fatalf("agent patch proposal: %d %v", code, body)
	}

	// Supersede and decide through the human-only API.
	code, body, _ = doJSON(t, "GET", base+"/v1/proposals?state=pending", nil)
	if code != http.StatusOK || len(body["proposals"].([]any)) != 1 {
		t.Fatalf("pending list: %d %v", code, body)
	}
	pendingID := body["proposals"].([]any)[0].(map[string]any)["id"].(string)
	code, body = doScoped(t, "POST", base+"/v1/proposals/"+filesID+"/decide", live, map[string]any{"decision": "allow"})
	if code != http.StatusForbidden || errCode(body) != "forbidden" {
		t.Fatalf("agent decide: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/proposals/"+pendingID+"/decide", map[string]any{"decision": "allow"})
	if code != http.StatusOK || body["status"] != "applied" {
		t.Fatalf("human decide: %d %v", code, body)
	}
	got, err := os.ReadFile(agentPath)
	if err != nil || !strings.Contains(string(got), "ops@example.com") {
		t.Fatalf("approved file=%q err=%v", got, err)
	}
	var protected string
	if err := st.DB().QueryRow(`SELECT protected_keys FROM agent_proposals WHERE id=?`, pendingID).Scan(&protected); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(protected, "email.accept") {
		t.Fatalf("protected keys=%q", protected)
	}

	// Delete remains human-only.
	code, body = doScoped(t, "DELETE", base+"/v1/workspaces/"+wsID+"/files/agents/cmo/AGENT.md", live, nil)
	if code != http.StatusForbidden || errCode(body) != "forbidden" {
		t.Fatalf("agent delete: %d %v", code, body)
	}
}
