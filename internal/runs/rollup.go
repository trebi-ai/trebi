package runs

import (
	"context"
	"time"

	"github.com/trebi-ai/trebi/internal/pricing"
)

// Spend is the turn count, the priced cost, and the token totals of a set of turns.
type Spend struct {
	Turns   int
	CostUSD float64
	Tokens  pricing.TokenTotals
}

// add prices one turn and folds it in.
func (s *Spend) add(u pricing.TokenUsage) {
	u.ApplyRates()
	s.Turns++
	s.CostUSD += u.CostUSD
	s.Tokens.Add(u)
}

// OwnerSpend returns the month-to-date spend per agent ID, including the
// jobs that the agent owns. It reads and prices all runs for the workspace
// in one query, so agent list pages do not issue one query per agent. A turn
// counts in the month it started.
func (s *Service) OwnerSpend(ctx context.Context, workspaceID string, agentIDs []string) (map[string]Spend, error) {
	out := make(map[string]Spend, len(agentIDs))
	want := map[string]bool{}
	for _, id := range agentIDs {
		want[id] = true
	}
	monthStart := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := s.st.DB().QueryContext(ctx, `
SELECT COALESCE(NULLIF(ji.owner_id,''), r.job_id), COALESCE(t.tokens_input,0), COALESCE(t.tokens_output,0), COALESCE(t.tokens_cache_read,0),
       COALESCE(t.tokens_cache_creation,0), COALESCE(NULLIF(t.tokens_model,''), r.tokens_model, ''), t.started_at
FROM run_turns t JOIN runs r ON r.id = t.run_id LEFT JOIN job_index ji ON ji.id = r.job_id WHERE r.workspace_id=? AND t.started_at>=?`, workspaceID, monthStart.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var owner, model, started string
		var in, output, cacheRead, cacheCreate int64
		if err := rows.Scan(&owner, &in, &output, &cacheRead, &cacheCreate, &model, &started); err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339Nano, started)
		if err != nil || at.Before(monthStart) {
			continue
		}
		if !want[owner] {
			continue
		}
		sp := out[owner]
		sp.add(pricing.TokenUsage{Input: in, Output: output, CacheRead: cacheRead, CacheCreation: cacheCreate, Model: model})
		out[owner] = sp
	}
	return out, rows.Err()
}
