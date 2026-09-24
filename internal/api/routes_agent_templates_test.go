package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentTemplateCreate(t *testing.T) {
	_, base, _ := startAPI(t)
	wsPath := t.TempDir()
	wsID := addWorkspace(t, base, wsPath, "template-ws")

	code, body, _ := doJSON(t, "GET", base+"/v1/agent-templates", nil)
	if code != http.StatusOK {
		t.Fatalf("templates: %d %v", code, body)
	}
	list, _ := body["templates"].([]any)
	if len(list) != 5 {
		t.Fatalf("template count = %d", len(list))
	}

	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/research", map[string]any{
		"title": "Research", "template": "research", "email_accept": []string{"operators"},
		"inputs": map[string]string{"competitors": "acme"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	data, err := os.ReadFile(filepath.Join(wsPath, "agents", "research", "weekly-brief", "JOB.md"))
	if err != nil || !strings.Contains(string(data), "disabled: true") {
		t.Fatalf("starter job %q %v", data, err)
	}
	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/agents/research", map[string]any{"template": "blank"})
	if code != http.StatusConflict || errCode(body) != "conflict" {
		t.Fatalf("duplicate: %d %v", code, body)
	}
}
