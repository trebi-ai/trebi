package workspaces_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

type recordedBudgetAlert struct {
	ws, agent string
	spent     float64
	limit     float64
}

func (r *recordedBudgetAlert) BudgetExceeded(_ context.Context, ws, agent string, spent, monthlyUSD float64) {
	r.ws, r.agent, r.spent, r.limit = ws, agent, spent, monthlyUSD
}

func TestBudgetGateWithoutBudgetDoesNotQueryOrAlert(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.OpenPath(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := workspaces.New(st).WithRoot(dir)
	alerter := &recordedBudgetAlert{}
	svc.SetBudgetAlerter(alerter)
	ok, reason, err := svc.Allow(ctx, "missing", "agents/research", "agent")
	if err != nil || !ok || reason != "" || alerter.agent != "" {
		t.Fatalf("ok=%t reason=%q err=%v alert=%+v", ok, reason, err, alerter)
	}
}

func TestBudgetGateExceedsAndAlerts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.OpenPath(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	agentDir := filepath.Join(root, "agents", "research")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("---\nbudget:\n  monthly_usd: 100\n  on_exceed: pause\n---\n\nRun.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id,name,path,git_enabled,created_at,updated_at) VALUES ('ws','ws',?,0,?,?)`, root, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO job_index (workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES ('ws','agents/research',?,'agent',1,'fake','hash',?)`, filepath.Join(agentDir, "AGENT.md"), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO runs (id,workspace_id,job,kind,status,trace_id,tokens_input,tokens_model,started_at,updated_at)
		VALUES ('run','ws','agents/research/build','job','complete','trace',40000000,'default',?,?)`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	svc := workspaces.New(st).WithRoot(dir)
	alerter := &recordedBudgetAlert{}
	svc.SetBudgetAlerter(alerter)
	ok, reason, err := svc.Allow(ctx, "ws", "agents/research", "agent")
	if err != nil || ok || reason == "" {
		t.Fatalf("ok=%t reason=%q err=%v", ok, reason, err)
	}
	if alerter.agent != "agents/research" || alerter.limit != 100 || alerter.spent != 120 {
		t.Fatalf("alert = %+v", alerter)
	}
}
