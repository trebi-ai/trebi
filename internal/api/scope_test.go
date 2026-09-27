package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

func insertAgentCaller(t *testing.T, st *store.Store, wsID, agent, token, status string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := st.DB().Exec(`INSERT INTO runs
		(id,workspace_id,job,kind,status,trace_id,started_at,updated_at,actor_token_hash)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		"run-"+agent+"-"+status, wsID, "agents/"+agent, "agent", status, "trace", now, now,
		runs.HashActorToken(token))
	if err != nil {
		t.Fatal(err)
	}
}

func doScoped(t *testing.T, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if token == "" {
		code, out, _ := doJSON(t, method, path, body)
		return code, out
	}
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Trebi-Run-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestAgentRunTokenScopesWrites(t *testing.T) {
	_, base, dir := startAPI(t)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	if err := os.WriteFile(filepath.Join(wsDir, "TREBI.md"), []byte("---\nharness: fake\n---\n\nAssist.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A second agent proves ownership, not agent-kind membership, is checked.
	if err := os.MkdirAll(filepath.Join(wsDir, "agents", "beta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "agents", "beta", "AGENT.md"), []byte("---\n---\n\nBeta.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "scoped")

	live := "live-token-000000000000000000000000"
	dead := "dead-token-000000000000000000000000"
	insertAgentCaller(t, st, wsID, "cmo", live, "running")
	insertAgentCaller(t, st, wsID, "cmo", dead, "failed")

	// Valid token resolves a live actor and allows an owned create.
	code, body := doScoped(t, "POST", base+"/v1/jobs", live, map[string]any{
		"workspace": wsID, "name": "owned", "prompt": "Do owned work.", "agent": "cmo",
	})
	if code != http.StatusCreated {
		t.Fatalf("owned create: %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "agents", "cmo", "owned", "JOB.md")); err != nil {
		t.Fatalf("owned job not created: %v", err)
	}

	// A different owner is forbidden.
	code, body = doScoped(t, "POST", base+"/v1/jobs", live, map[string]any{
		"workspace": wsID, "name": "other", "prompt": "No.", "agent": "beta",
	})
	if code != http.StatusForbidden || errCode(body) != "forbidden" {
		t.Fatalf("other owner: %d %v", code, body)
	}

	// The nested owned-job shape routes through the same scope guard. Beta's
	// job belongs to another actor and must be read-only for this run.
	if err := os.MkdirAll(filepath.Join(wsDir, "agents", "beta", "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "agents", "beta", "other", "JOB.md"), []byte("---\nharness: fake\n---\n\nNo.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/agents/beta/other/JOB.md", map[string]any{
		"content": "---\nharness: fake\n---\n\nNo.\n",
	})
	if code != http.StatusOK {
		t.Fatalf("seed nested beta job: %d %v", code, body)
	}
	code, body = doScoped(t, "PUT", base+"/v1/jobs/agents/beta/other/files/NOTE.md?workspace="+wsID, live, map[string]any{"content": "no"})
	if code != http.StatusForbidden || errCode(body) != "forbidden" {
		t.Fatalf("unowned nested file: %d %v", code, body)
	}
	if code != http.StatusForbidden || errCode(body) != "forbidden" {
		t.Fatalf("unowned nested job: %d %v", code, body)
	}

	// Workspace file writes outside the agent folder are forbidden.
	code, body = doScoped(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/outside.txt", live, map[string]any{"content": "no"})
	if code != http.StatusForbidden || errCode(body) != "forbidden" {
		t.Fatalf("outside file: %d %v", code, body)
	}

	// Protected identity/trust/spend keys are human-only.
	code, body = doScoped(t, "PATCH", base+"/v1/workspaces/"+wsID+"/agents/cmo", live,
		map[string]any{"email": map[string]any{"accept": []string{"ops@example.com"}}})
	if code != http.StatusAccepted || body["status"] != "pending" {
		t.Fatalf("protected patch: %d %v", code, body)
	}

	// A terminal token is dead and must be rejected before routing.
	code, body = doScoped(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/outside.txt", dead,
		map[string]any{"content": "no"})
	if code != http.StatusUnauthorized || errCode(body) != "unauthorized" {
		t.Fatalf("dead token: %d %v", code, body)
	}

	// No header is a human call.
	code, _ = doScoped(t, "PUT", base+"/v1/workspaces/"+wsID+"/files/human.txt", "", map[string]any{"content": "ok"})
	if code != http.StatusCreated {
		t.Fatalf("human write: %d", code)
	}
}

func TestAgentEscalationRoutes(t *testing.T) {
	_, base, dir := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	if err := os.WriteFile(filepath.Join(wsDir, "TREBI.md"), []byte("---\nharness: fake\n---\n\nAssist.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "escalation-ws")
	writeFakeHarness(t, wsDir)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	live := "escalate-token-0000000000000000000"
	insertAgentCaller(t, st, wsID, "cmo", live, "running")

	code, body := doScoped(t, "POST", base+"/v1/workspaces/"+wsID+"/agents/cmo/escalate", live, map[string]any{
		"to": "operator", "reason": "credentials expired",
	})
	if code != http.StatusAccepted || body["escalated"] != true {
		t.Fatalf("operator escalate: %d %v", code, body)
	}
	var inbox int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM inbox_items WHERE alert_kind='agent_escalation'`).Scan(&inbox); err != nil {
		t.Fatal(err)
	}
	if inbox != 1 {
		t.Fatalf("agent_escalation items = %d", inbox)
	}

	code, body = doScoped(t, "POST", base+"/v1/workspaces/"+wsID+"/agents/cmo/escalate", live, map[string]any{
		"to": "assistant", "reason": "need a human decision",
	})
	if code != http.StatusAccepted || body["to"] != "assistant" {
		t.Fatalf("assistant escalate: %d %v", code, body)
	}
	var operator string
	var channel string
	if err := st.DB().QueryRow(`SELECT operator, origin_channel FROM conversations ORDER BY id DESC LIMIT 1`).Scan(&operator, &channel); err != nil {
		t.Fatal(err)
	}
	if operator != "" || channel != "agent" {
		t.Fatalf("assistant escalation conversation operator=%q channel=%q", operator, channel)
	}
}

func TestScopeGateFailsClosed(t *testing.T) {
	_, base, dir := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "scope-gate")
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	live := "gate-token-000000000000000000000"
	insertAgentCaller(t, st, wsID, "cmo", live, "running")

	deny := map[string]any{
		"/v1/inbox/01ABC/resolve":                    map[string]any{},
		"/v1/harnesses/claude":                       map[string]any{"model": "opus"},
		"/v1/config":                                 map[string]any{},
		"/v1/connectors":                             map[string]any{"name": "mail", "type": "imap"},
		"/v1/runs/01ABC/approve":                     map[string]any{"decision": "approve"},
		"/v1/queue/cancel":                           map[string]any{"ids": []string{"01ABC"}},
		"/v1/workspaces/" + wsID + "/assistant/wake": map[string]any{},
		"/v1/workspaces/" + wsID:                     map[string]any{"name": "renamed"},
	}
	for path := range deny {
		code, body := doScoped(t, "POST", base+path, live, deny[path])
		if path == "/v1/config" {
			// PATCH is the mutating method for config.
			code, body = doScoped(t, "PATCH", base+path, live, deny[path])
		}
		if path == "/v1/harnesses/claude" {
			code, body = doScoped(t, "PATCH", base+path, live, deny[path])
		}
		if code != http.StatusForbidden || errCode(body) != "forbidden" {
			t.Fatalf("%s: %d %v", path, code, body)
		}
	}
}
