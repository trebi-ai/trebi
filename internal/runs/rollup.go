package runs

import (
	"context"
	"time"

	"github.com/trebi-ai/trebi/internal/pricing"
)

// OwnerCosts returns priced month-to-date cost per exact owner lane, including
// the lanes beneath each owner. It reads and prices all runs for the workspace
// in one query, so agent list pages do not issue one query per agent.
func (s *Service) OwnerCosts(ctx context.Context, workspaceID string, owners []string) (map[string]float64, error) {
	out := make(map[string]float64, len(owners))
	want := map[string]bool{}
	for _, owner := range owners {
		want[owner] = true
	}
	monthStart := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := s.st.DB().QueryContext(ctx, `
SELECT job, COALESCE(tokens_input,0), COALESCE(tokens_output,0), COALESCE(tokens_cache_read,0),
       COALESCE(tokens_cache_creation,0), COALESCE(tokens_model,''), started_at
FROM runs WHERE workspace_id=? AND started_at>=?`, workspaceID, monthStart.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var job, model, started string
		var in, output, cacheRead, cacheCreate int64
		if err := rows.Scan(&job, &in, &output, &cacheRead, &cacheCreate, &model, &started); err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339, started)
		if err != nil || at.Before(monthStart) {
			continue
		}
		owner := ownerOf(job, want)
		if owner == "" {
			continue
		}
		u := pricing.TokenUsage{Input: in, Output: output, CacheRead: cacheRead, CacheCreation: cacheCreate, Model: model}
		u.ApplyRates()
		out[owner] += u.CostUSD
	}
	return out, rows.Err()
}

func ownerOf(job string, owners map[string]bool) string {
	for owner := range owners {
		if job == owner || len(job) > len(owner) && job[:len(owner)] == owner && job[len(owner)] == '/' {
			return owner
		}
	}
	return ""
}
