package daemon

import (
	"context"
	"strings"

	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/runs"
)

// sendReportIfDue mails the final assistant turn of a portfolio report run.
func (c *ControlPlane) sendReportIfDue(ctx context.Context, run *runs.Run) {
	if run.Reason != runs.ReasonReport || run.ConversationID == "" ||
		c.Email == nil || !c.Email.Configured() {
		return
	}
	agent, _ := c.findJobKind(ctx, run.Workspace, run.Job, runs.KindAgent)
	if agent == nil || agent.Report == nil {
		return
	}
	recipients, err := c.Email.ResolveRecipients(ctx, run.Workspace, agent.Report.To)
	if err != nil || len(recipients) == 0 {
		c.log.Warn("report recipients", "job", run.Job, "error", err)
		return
	}
	turn, err := c.Conversations.LastAssistantTurn(ctx, run.ConversationID, run.ID)
	if err != nil || turn == nil || strings.TrimSpace(turn.Text) == "" {
		return
	}
	subject := "Portfolio report"
	if agent.Title != "" {
		subject = agent.Title + " portfolio report"
	}
	var inReplyTo string
	_ = c.Store.DB().QueryRowContext(ctx, `
SELECT message_id FROM email_messages
WHERE direction='out' AND disposition='sent' AND workspace_id=? AND agent=? AND purpose='report'
ORDER BY created_at DESC, id DESC LIMIT 1`, run.Workspace, shortAgent(run.Job)).Scan(&inReplyTo)
	var existing int
	_ = c.Store.DB().QueryRowContext(ctx, `SELECT 1 FROM email_messages WHERE run_id=? AND purpose='report' LIMIT 1`, run.ID).Scan(&existing)
	if existing == 1 {
		return
	}
	if _, err := c.Email.Send(ctx, email.SendOpts{
		WorkspaceID: run.Workspace, Agent: shortAgent(run.Job),
		To: recipients, Subject: subject, Text: turn.Text, Markdown: true,
		InReplyTo: inReplyTo, RunID: run.ID, ConversationID: run.ConversationID,
		FromName: agent.Title, Purpose: "report",
	}); err != nil {
		c.log.Warn("report send", "job", run.Job, "error", err)
	}
}

func shortAgent(job string) string {
	return strings.TrimPrefix(job, "agents/")
}
