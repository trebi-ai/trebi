package api_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHumanManualRunAllowedOverBudgetWithWarning(t *testing.T) {
	s, base, dir := startAPI(t)
	_ = s
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "agents", "keeper")
	jobDir := filepath.Join(agentDir, "build")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(agentDir, "AGENT.md"): "---\nharness: fake\nbudget:\n  monthly_usd: 100\n  on_exceed: pause\n---\n\nKeep the repository healthy.\n",
		filepath.Join(jobDir, "JOB.md"):     "---\nharness: fake\n---\n\nBuild.\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// AddWorkspace triggers the same API-side index rebuild as production.
	// Write TREBI first? not needed here.
	wsID := addWorkspace(t, base, wsDir, "budget-ws")
	code, jobsBody, _ := doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID, nil)
	if code != http.StatusOK {
		t.Fatalf("jobs: %d %v", code, jobsBody)
	}
	t.Logf("jobs=%v", jobsBody)

	// 40m default-priced input tokens costs $120 month-to-date.
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	if _, err := st.DB().Exec(`INSERT INTO runs
		(id,workspace_id,job,kind,status,trace_id,tokens_input,tokens_model,started_at,updated_at)
		VALUES ('spent',?,'agents/keeper/build','job','complete','trace',40000000,'default',?,?)`,
		wsID, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	code, body, _ := doJSON(t, "POST", base+"/v1/runs", map[string]any{
		"workspace": wsID, "job": "agents/keeper/build", "kind": "job",
		"reason": "manual", "harness": "fake",
	})
	if code != http.StatusCreated {
		t.Fatalf("human run: %d %v", code, body)
	}
	if body["warning"] == "" {
		t.Fatalf("expected over-budget warning: %v", body)
	}
	if _, ok := body["run"].(map[string]any); !ok {
		t.Fatalf("run missing from envelope: %v", body)
	}
}

// TestRunActionErrorIsValidation proves an action error on launch maps to
// 400 validation, not 500 (P1-15).
func TestRunActionErrorIsValidation(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	jobDir := filepath.Join(wsDir, "acts")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"),
		[]byte("---\nharness: claude\nconnectors: [ghost]\n---\n\nGo.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "acts-ws")
	t.Setenv("TREBI_FAKE_HARNESS", "")
	code, body, _ := doJSON(t, "POST", base+"/v1/runs", map[string]any{
		"workspace": wsID, "job": "acts", "kind": "job", "reason": "manual",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("launch: %d %v, want 400", code, body)
	}
	e, _ := body["error"].(map[string]any)
	if e == nil || e["code"] != "validation" || !strings.Contains(fmt.Sprint(e["message"]), "connector actions invalid") {
		t.Fatalf("error=%v, want the action error as validation", body["error"])
	}
}
