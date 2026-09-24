package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnedJobRoutesUseFullPath(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	jobPath := filepath.Join(wsDir, "agents", "keeper", "build", "JOB.md")
	if err := os.MkdirAll(filepath.Dir(jobPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobPath, []byte("---\nharness: fake\n---\n\nBuild.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "owned-routes")

	code, body, _ := doJSON(t, "GET", base+"/v1/jobs/agents/keeper/build?workspace="+wsID, nil)
	if code != http.StatusOK || body["name"] != "agents/keeper/build" {
		t.Fatalf("nested show: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/jobs/agents/keeper/build/run?workspace="+wsID, map[string]any{"harness": "fake"})
	if code != http.StatusCreated {
		t.Fatalf("nested run: %d %v", code, body)
	}
}
