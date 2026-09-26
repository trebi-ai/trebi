package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAssistantGetDisabled: the disabled flag is a settings field; the
// effective view reports it (plan F.1).
func TestAssistantGetDisabled(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "asst-ws")

	code, body, _ := doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/assistant",
		map[string]any{"override": map[string]any{"disabled": true}})
	if code != 200 {
		t.Fatalf("put %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/assistant", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	eff := body["effective"].(map[string]any)
	if eff["disabled"] != true {
		t.Fatalf("disabled=%v want true in %v", eff["disabled"], eff)
	}

	// Resetting the override inherits the default: enabled.
	code, body, _ = doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/assistant",
		map[string]any{"override": map[string]any{}})
	if code != 200 {
		t.Fatalf("reset %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/assistant", nil)
	eff = body["effective"].(map[string]any)
	if eff["disabled"] != false {
		t.Fatalf("disabled=%v want false in %v", eff["disabled"], eff)
	}
}

func TestWorkspaceMove(t *testing.T) {
	_, base := startTestServer(t)
	parent := t.TempDir()
	src := filepath.Join(parent, "src")
	if err := os.MkdirAll(filepath.Join(src, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	job := "---\nharness: fake\nworking_dir: " + src + "\n---\n\nDo " + src + "/alpha/run.sh\n"
	if err := os.WriteFile(filepath.Join(src, "alpha", "JOB.md"), []byte(job), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "alpha", "run.sh"), []byte("echo "+src+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, src, "move-ws")
	dest := filepath.Join(parent, "dest")

	code, body, _ := doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/move", map[string]any{
		"path": dest, "dry_run": true,
	})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("unconfirmed dry-run %d %v", code, body)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/move?confirm=1", map[string]any{
		"path": dest, "dry_run": true,
	})
	if code != 200 {
		t.Fatalf("dry-run %d %v", code, body)
	}
	if body["dry_run"] != true {
		t.Fatalf("dry_run flag %v", body)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("dry-run moved src")
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/move?confirm=1", map[string]any{
		"path": dest,
	})
	if code != 200 {
		t.Fatalf("move %d %v", code, body)
	}
	if body["path"] != dest {
		t.Fatalf("path %v", body["path"])
	}
	rewritten, _ := body["rewritten"].([]any)
	if len(rewritten) == 0 {
		t.Fatalf("expected rewritten files: %v", body)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("src still there: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "alpha", "JOB.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), src) {
		t.Fatalf("JOB.md still has old path: %s", got)
	}

	code, show, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID, nil)
	if code != 200 || show["path"] != dest {
		t.Fatalf("show %d %v", code, show)
	}
}
