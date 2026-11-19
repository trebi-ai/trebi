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

// seedSpentAgent adds an agent with a $100 budget and one turn of 40M
// default-priced input tokens and 5M cache reads ($121.50) this month.
func seedSpentAgent(t *testing.T) (base, wsID string) {
	t.Helper()
	_, base, dir := startAPI(t)
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "agents", "keeper")
	jobDir := filepath.Join(agentDir, "build")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(agentDir, "AGENT.md"): "---\ndescription: Test agent.\nharness: fake\nbudget:\n  monthly_usd: 100\n  on_exceed: pause\n---\n\nKeep the repository healthy.\n",
		filepath.Join(jobDir, "JOB.md"):     "---\nharness: fake\n---\n\nBuild.\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wsID = addWorkspace(t, base, wsDir, "budget-ws")
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`INSERT INTO runs
		(id,workspace_id,job_id,kind,status,trace_id,tokens_input,tokens_cache_read,tokens_model,started_at,updated_at)
		VALUES ('spent',?,?,'job','complete','trace',40000000,5000000,'default',?,?)`,
		wsID, indexedJobID(t, st, wsID, "keeper/build"), now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO run_turns (run_id,n,reason,status,started_at,tokens_input,tokens_cache_read,tokens_model)
		VALUES ('spent',1,'manual','complete',?,40000000,5000000,'default')`, now); err != nil {
		t.Fatal(err)
	}
	return base, wsID
}

func TestHumanManualRunAllowedOverBudgetWithWarning(t *testing.T) {
	base, wsID := seedSpentAgent(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/runs", map[string]any{
		"workspace": wsID, "job": "keeper/build", "kind": "job",
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

// TestSpendShowsTokens checks that the agent, job, and stats reads give
// month-to-date tokens, and that the budget stays in USD.
func TestSpendShowsTokens(t *testing.T) {
	base, wsID := seedSpentAgent(t)
	wantTokens := map[string]float64{"input": 40e6, "output": 0, "cache_read": 5e6, "cache_creation": 0, "billed": 40e6, "cached": 5e6}
	checkTokens := func(what string, v any) {
		t.Helper()
		m, _ := v.(map[string]any)
		for k, want := range wantTokens {
			if m[k] != want {
				t.Fatalf("%s.%s = %v, want %v (%v)", what, k, m[k], want, m)
			}
		}
	}

	code, body, _ := doJSON(t, "GET", base+"/v1/agents?workspace="+wsID, nil)
	agents, _ := body["agents"].([]any)
	if code != http.StatusOK || len(agents) != 1 {
		t.Fatalf("agents: %d %v", code, body)
	}
	agent, _ := agents[0].(map[string]any)
	if _, ok := agent["cost_mtd_usd"]; ok {
		t.Fatalf("agent still carries cost_mtd_usd: %v", agent)
	}
	checkTokens("tokens_30d", agent["tokens_30d"])
	budget, _ := agent["budget"].(map[string]any)
	if spent, _ := budget["spent_usd"].(float64); spent < 121.49 || spent > 121.51 || budget["state"] != "exceeded" {
		t.Fatalf("budget must stay in USD: %v", budget)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/jobs?workspace="+wsID+"&q=build", nil)
	jobs, _ := body["jobs"].([]any)
	if code != http.StatusOK || len(jobs) != 1 {
		t.Fatalf("jobs: %d %v", code, body)
	}
	row, _ := jobs[0].(map[string]any)
	if _, ok := row["mtd_usd"]; ok {
		t.Fatalf("job row still carries mtd_usd: %v", row)
	}
	checkTokens("tokens_30d", row["tokens_30d"])

	code, body, _ = doJSON(t, "GET", base+"/v1/stats/runs?workspace="+wsID+"&range=mtd", nil)
	mtd, _ := body["mtd"].(map[string]any)
	if code != http.StatusOK || mtd["tokens_input"] != 40e6 || mtd["tokens_cache_read"] != 5e6 ||
		mtd["tokens_billed"] != 40e6 || mtd["tokens_cached"] != 5e6 || mtd["cost_usd"] == 0.0 {
		t.Fatalf("stats mtd: %d %v", code, body)
	}
}

// TestRunActionErrorIsValidation proves an action error on launch maps to
// 400 validation, not 500 (P1-15).
func TestRunActionErrorIsValidation(t *testing.T) {
	_, base, _ := startAPI(t)
	wsDir := t.TempDir()
	writeAgentFile(t, wsDir, "ops")
	jobDir := filepath.Join(wsDir, "agents", "ops", "acts")
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
		"workspace": wsID, "job": "ops/acts", "kind": "job", "reason": "manual",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("launch: %d %v, want 400", code, body)
	}
	e, _ := body["error"].(map[string]any)
	if e == nil || e["code"] != "validation" || !strings.Contains(fmt.Sprint(e["message"]), "connector actions invalid") {
		t.Fatalf("error=%v, want the action error as validation", body["error"])
	}
}
