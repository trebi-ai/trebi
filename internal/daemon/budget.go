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
		Title: "Agent budget exceeded",
		Body: fmt.Sprintf("Month-to-date cost %.2f USD exceeded the %.2f USD limit.",
			spent, monthlyUSD),
		Hint: "Raise the budget, switch to alert mode, or pause the agent's jobs.",
	})
	if err != nil {
		c.log.Warn("budget alert", "agent", agent, "error", err)
	}
}
