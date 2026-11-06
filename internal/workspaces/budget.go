package workspaces

import (
	"context"
	"strconv"
	"time"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/store"
)

// Allow implements runs.BudgetGate. It resolves the effective owner from the
// index and reads the owner's AGENT.md; absent budget allows without querying.
func (s *Service) Allow(ctx context.Context, workspaceID, name, kind string) (bool, string, error) {
	rows, err := s.ListJobs(ctx, workspaceID)
	if err != nil {
		return true, "", err
	}
	owner := name
	for _, row := range rows {
		if row.Name == name || (kind == "agent" && row.Kind == "agent" && row.Name == name) {
			if row.OwnerIndex() != "" {
				owner = row.OwnerIndex()
			} else if row.Kind == "agent" {
				owner = row.Name
			}
			break
		}
		if row.Name == name && row.OwnerIndex() != "" {
			owner = row.OwnerIndex()
		}
	}
	if owner == "" {
		return true, "", nil
	}
	var budget *job.BudgetYAML
	for _, row := range rows {
		if row.Kind == "agent" && row.Name == owner {
			parsed, err := job.ParseFile(row.Path)
			if err == nil && parsed != nil {
				budget = parsed.Budget
			}
			break
		}
	}
	if budget == nil || budget.MonthlyUSD <= 0 {
		return true, "", nil
	}
	spent, err := s.monthToDateCost(ctx, workspaceID, owner)
	if err != nil {
		return true, "", err
	}
	if spent < budget.MonthlyUSD {
		return true, "", nil
	}
	if s.budgetAlerter != nil {
		s.budgetAlerter.BudgetExceeded(ctx, workspaceID, owner, spent, budget.MonthlyUSD)
	}
	if budget.OnExceed == "alert" {
		return true, "monthly budget exceeded; continuing because on_exceed is alert", nil
	}
	return false, "monthly budget of " + formatUSD(budget.MonthlyUSD) + " exceeded", nil
}

// BudgetAlerter receives an exceeded event once per cooldown at the alert
// boundary. workspaces cannot import alerts, so the daemon injects this.
type BudgetAlerter interface {
	BudgetExceeded(ctx context.Context, workspaceID, agent string, spent, monthlyUSD float64)
}

func (s *Service) SetBudgetAlerter(a BudgetAlerter) { s.budgetAlerter = a }

func (s *Service) monthToDateCost(ctx context.Context, workspaceID, owner string) (float64, error) {
	monthStart := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := s.st.DB().QueryContext(ctx, `
SELECT r.job_id, COALESCE(t.tokens_input,0), COALESCE(t.tokens_output,0), COALESCE(t.tokens_cache_read,0),
       COALESCE(t.tokens_cache_creation,0), COALESCE(NULLIF(t.tokens_model,''), r.tokens_model, ''), COALESCE(t.tokens_cost_usd,0), t.started_at
FROM run_turns t JOIN runs r ON r.id = t.run_id
WHERE r.workspace_id=? AND t.started_at>=? AND `+store.OwnerRefSQL("r.job_id", "r.workspace_id"),
		workspaceID, monthStart.Format(time.RFC3339), owner, owner, owner, owner)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var spent float64
	for rows.Next() {
		var lane, model, started string
		var in, output, cacheRead, cacheCreate int64
		var harnessUSD float64
		if err := rows.Scan(&lane, &in, &output, &cacheRead, &cacheCreate, &model, &harnessUSD, &started); err != nil {
			return 0, err
		}
		at, err := time.Parse(time.RFC3339, started)
		if err != nil || at.Before(monthStart) {
			continue
		}
		u := pricing.TokenUsage{Input: in, Output: output, CacheRead: cacheRead, CacheCreation: cacheCreate, Model: model, HarnessUSD: harnessUSD}
		u.ApplyRates()
		spent += u.CostUSD
	}
	return spent, rows.Err()
}

func formatUSD(v float64) string {
	return "$" + strconv.FormatFloat(v, 'f', 2, 64)
}

// ReportBlock composes the portfolio context for an agent's recurring report.
func (s *Service) ReportBlock(ctx context.Context, workspaceID, owner string, since time.Time) (string, error) {
	cost, err := s.monthToDateCost(ctx, workspaceID, owner)
	if err != nil {
		return "", err
	}
	var runsTotal, failures, openItems int
	err = s.st.DB().QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(CASE WHEN COALESCE(outcome,status) IN ('failed','timed_out','exited') THEN 1 ELSE 0 END),0)
FROM runs WHERE workspace_id=? AND started_at>=? AND `+store.OwnerRefSQL("runs.job_id", "runs.workspace_id"),
		workspaceID, since.Format(time.RFC3339), owner, owner, owner, owner).Scan(&runsTotal, &failures)
	if err != nil {
		return "", err
	}
	if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_items WHERE workspace_id=? AND `+
		store.JobRefSQL("inbox_items.job_id", "inbox_items.workspace_id")+` AND done_at IS NULL`,
		workspaceID, owner, owner).Scan(&openItems); err != nil {
		return "", err
	}
	return "Report period starts " + since.UTC().Format(time.RFC3339) +
		"\nRuns: " + strconv.Itoa(runsTotal) +
		"\nFailures: " + strconv.Itoa(failures) +
		"\nCost USD: " + strconv.FormatFloat(cost, 'f', 4, 64) +
		"\nOpen inbox items: " + strconv.Itoa(openItems) +
		"\n\nAnswer with a concise brief for report.to. Write it in Markdown; Trebi mails it as styled HTML. " +
		"Use kpi, status, and button blocks and GitHub alerts where they help (trebi guide email-format).", nil
}
