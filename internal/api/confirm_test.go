package api_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestDestructiveRoutesNeedConfirm(t *testing.T) {
	s, base, _ := startAPI(t)
	wsID := addWorkspace(t, base, s.Token(), t.TempDir(), "confirm-ws")
	for _, c := range confirmCases(wsID) {
		t.Run(c.name, func(t *testing.T) {
			code, body, _ := doJSON(t, c.method, base+c.path, s.Token(), nil)
			if code != http.StatusBadRequest || errCode(body) != "validation" {
				t.Fatalf("unconfirmed %s: got %d %v", c.path, code, body)
			}
			details, _ := errDetails(body)["confirm_required"].(bool)
			if !details {
				t.Fatalf("unconfirmed %s: no confirm_required in %v", c.path, body)
			}
			// The gate must pass a confirmed call to the handler, whose own
			// answer (200, 404, …) no longer carries confirm_required.
			_, body, _ = doJSON(t, c.method, base+withConfirm(c.path), s.Token(), nil)
			if _, gated := errDetails(body)["confirm_required"]; gated {
				t.Fatalf("confirmed %s still refused: %v", c.path, body)
			}
		})
	}
}

func TestDestructiveMCPActionsPassConfirm(t *testing.T) {
	s, base, _ := startAPI(t)
	wsID := addWorkspace(t, base, s.Token(), t.TempDir(), "confirm-mcp")
	cs := mcpSession(t, base, s.Token())
	for _, c := range confirmCases(wsID) {
		t.Run(c.name, func(t *testing.T) {
			_, text := mcpCall(t, cs, c.tool, c.action, c.input)
			if !strings.Contains(text, "confirm_required") {
				t.Fatalf("%s.%s without confirm: %s", c.tool, c.action, text)
			}
			input := map[string]any{"confirm": true}
			for k, v := range c.input {
				input[k] = v
			}
			_, text = mcpCall(t, cs, c.tool, c.action, input)
			if strings.Contains(text, "confirm_required") {
				t.Fatalf("%s.%s with confirm still refused: %s", c.tool, c.action, text)
			}
		})
	}
}

func withConfirm(path string) string {
	if strings.Contains(path, "?") {
		return path + "&confirm=1"
	}
	return path + "?confirm=1"
}

func errDetails(m map[string]any) map[string]any {
	e, _ := m["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	return d
}

// confirmCase names one destructive route plus the MCP action that reaches it.
type confirmCase struct {
	name   string
	method string
	path   string // without the confirm parameter
	tool   string
	action string
	input  map[string]any // without confirm
}

// confirmCases covers every route in the A5.3 gate list except shutdown,
// which would stop the test daemon.
func confirmCases(wsID string) []confirmCase {
	return []confirmCase{
		{"workspace", http.MethodDelete, "/v1/workspaces/" + wsID, "workspace", "delete", map[string]any{"id": wsID}},
		{"job", http.MethodDelete, "/v1/jobs/report?workspace=" + wsID, "job", "delete", map[string]any{"name": "report", "workspace": wsID}},
		{"job purge", http.MethodDelete, "/v1/jobs/report?workspace=" + wsID + "&purge=1", "job", "delete", map[string]any{"name": "report", "workspace": wsID, "purge": true}},
		{"job memory", http.MethodDelete, "/v1/jobs/report/memory?workspace=" + wsID, "job", "memory_clear", map[string]any{"name": "report", "workspace": wsID}},
		{"assistant memory", http.MethodDelete, "/v1/workspaces/" + wsID + "/assistant/memory", "assistant", "memory_clear", map[string]any{"workspace": wsID}},
		{"secret", http.MethodDelete, "/v1/secrets/eval_key", "secret", "delete", map[string]any{"name": "eval_key"}},
		{"connector", http.MethodDelete, "/v1/connectors/tick", "connector", "delete", map[string]any{"name": "tick"}},
		{"skill", http.MethodDelete, "/v1/skills/tiny", "skill", "delete", map[string]any{"name": "tiny"}},
		{"workspace file", http.MethodDelete, "/v1/workspaces/" + wsID + "/files/notes.txt", "file", "delete", map[string]any{"workspace": wsID, "path": "notes.txt"}},
		{"cloud unlink", http.MethodDelete, "/v1/cloud/link", "system", "cloud_unlink", map[string]any{}},
	}
}
