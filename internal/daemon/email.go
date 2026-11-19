// Email channel orchestration (plan 2026-09-19 Part H): the Actions the
// ingress router calls, plus loop startup. The daemon is the one writer;
// the email package only decides.
package daemon

import (
	"context"
	"fmt"

	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/proposals"
	"github.com/trebi-ai/trebi/internal/requests"
)

var _ email.Actions = (*ControlPlane)(nil)

// RouteEnvFor gathers one workspace's routing facts: agent tags and the
// workspace's own allowlist extension (plan F, A.6).
func (c *ControlPlane) RouteEnvFor(ctx context.Context, workspaceID string) (email.RouteEnv, error) {
	ws, err := c.Workspaces.Get(ctx, workspaceID)
	if err != nil {
		return email.RouteEnv{}, fmt.Errorf("workspace %s: %w", workspaceID, err)
	}
	agentEntries, assistantEntries, err := c.Workspaces.AcceptRules(ctx, workspaceID)
	if err != nil {
		return email.RouteEnv{}, err
	}
	agents := make(map[string]email.AcceptRule, len(agentEntries))
	for tag, entries := range agentEntries {
		agents[tag] = email.NewAgentAcceptRule(entries)
	}
	return email.RouteEnv{
		Agents:    agents,
		Assistant: email.NewAcceptRule(assistantEntries),
		Allowlist: c.Workspaces.EmailAllowlist(ws),
	}, nil
}

// Ask delivers assistant mail into the channel conversation (plan A.13).
// An assistant ask prompts the session manager through Conversations.Ask;
// an agent ask still injects or enqueues. The thread root is the channel
// ref: one conversation per mail thread, bound by the channel pointer.
func (c *ControlPlane) Ask(ctx context.Context, d email.RouteDecision, msg email.IngressMessage, agent, text string) (string, error) {
	operatorID := ""
	if agent != "" {
		row, err := c.Workspaces.JobByName(ctx, d.WorkspaceID, agent)
		if err != nil {
			return "", fmt.Errorf("agent %s: %w", agent, err)
		}
		operatorID = row.ID
	}
	res, err := c.Conversations.Ask(ctx, conversations.AskOpts{
		Workspace:  d.WorkspaceID,
		OperatorID: operatorID,
		Text:       text,
		Address: conversations.Address{
			Connector: conversations.ChannelEmail,
			Room:      email.MailboxRoom(d.WorkspaceID, d.Agent),
			Thread:    email.ThreadRoot(msg),
		},
		IdempotencyKey: msg.ID,
		Headless:       operatorID == "",
	})
	if err != nil {
		return "", err
	}
	return res.ConversationID, nil
}

// InboxItem records the misrouted or spoof item through alerts (A.8, A.10).
func (c *ControlPlane) InboxItem(ctx context.Context, d email.RouteDecision, msg email.IngressMessage) (string, error) {
	return c.Alerts.EmailInboxItem(ctx, d, msg)
}

// DecideApproval applies one reply decision when an approval waits (plan
// I.1). applied is false when none waits; the reply then falls back to a
// run message.
func (c *ControlPlane) DecideApproval(ctx context.Context, runID, decision string) (bool, error) {
	a, err := c.Runs.PendingApproval(ctx, runID)
	if err != nil {
		return false, err
	}
	if a == nil {
		return false, nil
	}
	if _, err := c.Runs.DecideApproval(ctx, runID, "", decision); err != nil {
		return false, err
	}
	return true, nil
}

// RunMessage sends one operator reply to the session of a run (plan I.1).
func (c *ControlPlane) RunMessage(ctx context.Context, runID, text string) error {
	_, err := c.Runs.SendMessage(ctx, runID, text)
	return err
}

// DecideProposal applies one emailed AGENT.md proposal decision (Part A.6).
func (c *ControlPlane) DecideProposal(ctx context.Context, id string, allow bool, by string) error {
	_, err := c.Proposals.Decide(ctx, id, proposals.Decision{Allow: allow, By: by})
	return err
}

// Request returns one request for an emailed answer.
func (c *ControlPlane) Request(ctx context.Context, id string) (requests.Request, error) {
	return c.Requests.Get(ctx, id)
}

// AnswerRequest applies one emailed request answer.
func (c *ControlPlane) AnswerRequest(ctx context.Context, id string, in requests.AnswerIn) error {
	in.Via = "email"
	_, err := c.Requests.Answer(ctx, id, in, 0)
	return err
}

// ResolveInbox closes the item one reply decided (plan I.1).
func (c *ControlPlane) ResolveInbox(ctx context.Context, inboxID string) error {
	return c.Alerts.Close(ctx, inboxID, "email")
}

// AmbiguousReplyItem records the item for one approval reply that named
// both decisions (Part P).
func (c *ControlPlane) AmbiguousReplyItem(ctx context.Context, runID string, d email.RouteDecision, msg email.IngressMessage) (string, error) {
	return c.Alerts.AmbiguousReplyItem(ctx, runID, d, msg)
}

// AssistantName returns the From name of assistant mail.
func (c *ControlPlane) AssistantName(ctx context.Context, workspaceID string) string {
	ws, err := c.Workspaces.Get(ctx, workspaceID)
	if err != nil || ws == nil {
		return ""
	}
	return ws.SenderName()
}

// startEmail attaches the inbound pipeline to the service and runs it.
// The poll loop pulls only on the bearer transport; the hosted transport
// pushes into Service.Deliver through the cloudlink socket. The loop
// itself skips automatic polls under shadow and drain; the manual poll
// route shares this instance through the service.
func (c *ControlPlane) startEmail(ctx context.Context) {
	iv := c.Opts.EmailInterval
	if iv <= 0 {
		if st, err := c.Email.Settings(); err == nil && st.PollInterval > 0 {
			iv = st.PollInterval
		}
	}
	g := email.NewIngress(c.Email, c.Connectors, c, iv, c.Opts.Shadow)
	c.Email.AttachIngress(g)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		g.Run(ctx)
	}()
}
