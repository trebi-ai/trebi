package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/store"
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

// TestWorkspaceEmailAllowlist: the patch writes, reads, and clears the
// workspace trebi.yaml email.allowlist, folds case, and keeps the other keys.
func TestWorkspaceEmailAllowlist(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "allow-ws")
	if err := os.WriteFile(filepath.Join(wsDir, "trebi.yaml"),
		[]byte("assistant:\n  name: Ellis\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body, _ := doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID,
		map[string]any{"email_allowlist": []string{" Emma@Example.com ", "ops@example.com"}})
	if code != 200 {
		t.Fatalf("patch %d %v", code, body)
	}
	got := body["email_allowlist"].([]any)
	if len(got) != 2 || got[0] != "emma@example.com" || got[1] != "ops@example.com" {
		t.Fatalf("email_allowlist = %v", got)
	}
	raw, err := os.ReadFile(filepath.Join(wsDir, "trebi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "emma@example.com") || !strings.Contains(string(raw), "name: Ellis") {
		t.Fatalf("trebi.yaml = %s", raw)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID, nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, body)
	}
	if got := body["email_allowlist"].([]any); len(got) != 2 || got[0] != "emma@example.com" {
		t.Fatalf("get email_allowlist = %v", got)
	}

	// A domain entry cannot match an exact-address allowlist.
	code, body, _ = doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID,
		map[string]any{"email_allowlist": []string{"@example.com"}})
	if code != 400 {
		t.Fatalf("domain entry %d %v", code, body)
	}
	if details := body["error"].(map[string]any)["details"].(map[string]any); details["field"] != "email_allowlist" {
		t.Fatalf("details = %v", details)
	}

	// null clears the list and keeps the rest of the file.
	code, body, _ = doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID,
		map[string]any{"email_allowlist": nil})
	if code != 200 {
		t.Fatalf("clear %d %v", code, body)
	}
	if got, ok := body["email_allowlist"]; ok && got != nil && len(got.([]any)) != 0 {
		t.Fatalf("cleared email_allowlist = %v", got)
	}
	raw, err = os.ReadFile(filepath.Join(wsDir, "trebi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "allowlist") {
		t.Fatalf("trebi.yaml keeps the allowlist: %s", raw)
	}
	if !strings.Contains(string(raw), "name: Ellis") {
		t.Fatalf("trebi.yaml lost the assistant block: %s", raw)
	}
}

func TestWorkspaceHandlePatchAndCheck(t *testing.T) {
	var db *store.Store
	_, base := startTestServerSeeded(t, api.Options{}, func(st *store.Store) { db = st })
	wsID := addWorkspace(t, base, t.TempDir(), "Reports")
	otherID := addWorkspace(t, base, t.TempDir(), "Other")
	if code, _, _ := doJSON(t, "PATCH", base+"/v1/workspaces/"+otherID, map[string]any{"handle": "taken"}); code != 200 {
		t.Fatalf("set other handle: %d", code)
	}

	check := func(query string) (int, map[string]any) {
		code, body, _ := doJSON(t, "GET", base+"/v1/workspaces/handle-check?"+query, nil)
		return code, body
	}
	if code, b := check("handle=fresh&workspace=" + wsID); code != 200 || b["ok"] != true {
		t.Fatalf("ok: %d %v", code, b)
	}
	if code, b := check("handle=Bad!"); code != 200 || b["ok"] != false || b["reason"] != "format" {
		t.Fatalf("format: %d %v", code, b)
	}
	if code, b := check("handle=taken&workspace=" + wsID); code != 200 || b["ok"] != false || b["reason"] != "taken" {
		t.Fatalf("taken: %d %v", code, b)
	}
	if code, b := check(""); code != 400 || errCode(b) != "validation" {
		t.Fatalf("missing: %d %v", code, b)
	}

	code, body, _ := doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID, map[string]any{"handle": "mine"})
	if code != 200 || body["handle"] != "mine" {
		t.Fatalf("patch: %d %v", code, body)
	}
	if code, body, _ = doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID, map[string]any{"handle": "Bad!"}); code != 400 || errCode(body) != "validation" {
		t.Fatalf("bad handle: %d %v", code, body)
	}

	if _, err := db.DB().Exec(`INSERT INTO email_addresses (workspace_id, local_part, domain, created_at)
		VALUES (?, 'mine-01', 'in.example.com', 't')`, wsID); err != nil {
		t.Fatal(err)
	}
	if code, body, _ = doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID, map[string]any{"handle": "later"}); code != 409 || errCode(body) != "conflict" {
		t.Fatalf("locked: %d %v", code, body)
	}
	if code, b := check("handle=later&workspace=" + wsID); code != 200 || b["ok"] != false || b["reason"] != "locked" {
		t.Fatalf("locked check: %d %v", code, b)
	}
}
