package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssistantGetDisabled(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, "TREBI.md"),
		[]byte("---\nharness: fake\ndisabled: true\n---\n\nHi.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "asst-ws")
	code, body, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/assistant", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if body["disabled"] != true {
		t.Fatalf("disabled=%v want true in %v", body["disabled"], body)
	}

	if err := os.WriteFile(filepath.Join(wsDir, "TREBI.md"),
		[]byte("---\nharness: fake\n---\n\nHi.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/assistant", nil)
	if code != 200 {
		t.Fatalf("enabled %d %v", code, body)
	}
	if body["disabled"] != false {
		t.Fatalf("disabled=%v want false in %v", body["disabled"], body)
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
