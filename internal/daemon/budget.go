package daemon

import (
	"context"
	"fmt"

	"github.com/trebi-ai/trebi/internal/alerts"
)

// BudgetExceeded forwards the workspaces budget gate to the inbox. The alert
// kind's 720h cooldown gives one item per agent per month.
func (c *ControlPlane) BudgetExceeded(ctx context.Context, workspaceID, agent string, spent, monthlyUSD float64) {
	if c.Alerts == nil {
		return
	}
	_, err := c.Alerts.Raise(ctx, alerts.RaiseOpts{
		Kind: alerts.KindBudgetExceeded, WorkspaceID: workspaceID, JobName: agent,
		Body: fmt.Sprintf("It spent $%.2f this month. Its limit is $%.2f.", spent, monthlyUSD),
		Hint: "Raise the limit, or pause its jobs.",
	})
	if err != nil {
		c.log.Warn("budget alert", "agent", agent, "error", err)
	}
}
