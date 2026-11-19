package api_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestDestructiveRoutesNeedConfirm(t *testing.T) {
	_, base, _ := startAPI(t)
	wsID := addWorkspace(t, base, confirmWorkspace(t), "confirm-ws")
	addConfirmFolder(t, base, wsID)
	for _, c := range confirmCases(wsID) {
		t.Run(c.name, func(t *testing.T) {
			code, body, _ := doJSON(t, c.method, base+c.path, nil)
			if code != http.StatusBadRequest || errCode(body) != "validation" {
				t.Fatalf("unconfirmed %s: got %d %v", c.path, code, body)
			}
			details, _ := errDetails(body)["confirm_required"].(bool)
			if !details {
				t.Fatalf("unconfirmed %s: no confirm_required in %v", c.path, body)
			}
			// The gate must pass a confirmed call to the handler, whose own
			// answer (200, 404, …) no longer carries confirm_required.
			_, body, _ = doJSON(t, c.method, base+withConfirm(c.path), nil)
			if _, gated := errDetails(body)["confirm_required"]; gated {
				t.Fatalf("confirmed %s still refused: %v", c.path, body)
			}
		})
	}
}

func TestDestructiveMCPActionsPassConfirm(t *testing.T) {
	_, base, _ := startAPI(t)
	wsID := addWorkspace(t, base, confirmWorkspace(t), "confirm-mcp")
	addConfirmFolder(t, base, wsID)
	cs := mcpSession(t, base)
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

// confirmWorkspace makes a workspace folder with the jobs of the job delete
// cases. The job delete handler asks for confirm only after the job resolves.
func confirmWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"report", "report2"} {
		writeTestFile(t, filepath.Join(dir, name, "JOB.md"), "---\nharness: fake\n---\n\nRun.\n")
	}
	return dir
}

// addConfirmFolder makes the folder of the folder delete case. The folder
// delete handler asks for confirm only after the folder resolves.
func addConfirmFolder(t *testing.T, base, wsID string) {
	t.Helper()
	if code, body, _ := doJSON(t, http.MethodPost, base+"/v1/workspaces/"+wsID+"/folders/old", nil); code != http.StatusCreated {
		t.Fatalf("mkdir %d %v", code, body)
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
		{"job", http.MethodDelete, "/v1/jobs/report?workspace=" + wsID, "job", "delete", map[string]any{"name": "report", "workspace": wsID}},
		{"job purge", http.MethodDelete, "/v1/jobs/report2?workspace=" + wsID + "&purge=1", "job", "delete", map[string]any{"name": "report2", "workspace": wsID, "purge": true}},
		// Before the workspace case, which deletes the workspace. The job cases
		// come first for the same reason.
		{"workspace folder", http.MethodDelete, "/v1/workspaces/" + wsID + "/folders/old", "file", "rmdir", map[string]any{"workspace": wsID, "path": "old"}},
		{"workspace", http.MethodDelete, "/v1/workspaces/" + wsID, "workspace", "delete", map[string]any{"id": wsID}},
		{"workspace move", http.MethodPost, "/v1/workspaces/" + wsID + "/move", "workspace", "move", map[string]any{"id": wsID, "path": "/trebi-move-confirm-no-such/dest"}},
		{"job memory", http.MethodDelete, "/v1/jobs/report/memory?workspace=" + wsID, "job", "memory_clear", map[string]any{"name": "report", "workspace": wsID}},
		{"assistant memory", http.MethodDelete, "/v1/workspaces/" + wsID + "/assistant/memory", "assistant", "memory_clear", map[string]any{"workspace": wsID}},
		{"secret", http.MethodDelete, "/v1/secrets/eval_key", "secret", "delete", map[string]any{"name": "eval_key"}},
		{"connector", http.MethodDelete, "/v1/connectors/tick", "connector", "delete", map[string]any{"connector": "tick"}},
		{"skill", http.MethodDelete, "/v1/skills/tiny", "skill", "delete", map[string]any{"name": "tiny"}},
		{"workspace file", http.MethodDelete, "/v1/workspaces/" + wsID + "/files/notes.txt", "file", "delete", map[string]any{"workspace": wsID, "path": "notes.txt"}},
		{"cloud unlink", http.MethodDelete, "/v1/cloud/link", "system", "cloud_unlink", map[string]any{}},
		{"hook delete", http.MethodDelete, "/v1/hooks/01HOOKDEL", "system", "hook_delete", map[string]any{"id": "01HOOKDEL"}},
	}
}
