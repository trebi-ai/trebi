package api_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/agents"
)

// PUT with empty content writes the default template (2026-09-24 plan K.1).
func TestAssistantPutEmptyUsesTemplate(t *testing.T) {
	_, base := startTestServer(t)
	dir := t.TempDir()
	wsID := addWorkspace(t, base, dir, "asst-tpl")

	code, body, _ := doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/assistant",
		map[string]any{"content": ""})
	if code != 200 {
		t.Fatalf("put %d %v", code, body)
	}
	p, _ := body["path"].(string)
	if p == "" {
		t.Fatalf("no path in response: %v", body)
	}
	got, err := os.ReadFile(filepath.Clean(p))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != agents.AssistantTemplate {
		t.Fatalf("assistant file differs from template:\n%q", string(got))
	}
}
