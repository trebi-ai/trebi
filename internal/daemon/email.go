// Email channel orchestration (plan 2026-09-19 Part H): the Actions the
// ingress router calls, plus loop startup. The daemon is the one writer;
// the email package only decides.
package daemon

import (
	"context"
	"fmt"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/runs"
)

var _ email.Actions = (*ControlPlane)(nil)

// RouteEnvFor gathers one workspace's routing facts: agent tags and the
// workspace's own allowlist extension (plan F, A.6).
func (c *ControlPlane) RouteEnvFor(ctx context.Context, workspaceID string) (email.RouteEnv, error) {
	ws, err := c.Workspaces.Get(ctx, workspaceID)
	if err != nil {
		return email.RouteEnv{}, fmt.Errorf("workspace %s: %w", workspaceID, err)
	}
	tags, err := c.Workspaces.AgentTags(ctx, workspaceID)
	if err != nil {
		return email.RouteEnv{}, err
	}
	return email.RouteEnv{Agents: tags, Allowlist: c.Workspaces.EmailAllowlist(ws)}, nil
}

// Ask delivers assistant mail into the channel conversation (plan A.13).
// Send injects into a live run or enqueues a wake, so no launch happens
// here. The thread root is the channel ref: one conversation per mail
// thread, bound by the channel pointer.
func (c *ControlPlane) Ask(ctx context.Context, d email.RouteDecision, msg email.IngressMessage, text string) (string, error) {
	res, err := c.Conversations.Ask(ctx, conversations.AskOpts{
		Workspace:      d.WorkspaceID,
		Text:           text,
		ChannelType:    conversations.ChannelEmail,
		ChannelRef:     email.ThreadRoot(msg),
		IdempotencyKey: msg.ID,
	})
	if err != nil {
		return "", err
	}
	return res.ConversationID, nil
}

// WakeAgent mirrors the agent wake route: one launch, index-name job,
// agent kind, message-driven reason (plan A.7). The untrusted block rides
// WakeOpts; the wake composer fences it.
func (c *ControlPlane) WakeAgent(ctx context.Context, d email.RouteDecision, msg email.IngressMessage, u *runs.UntrustedMessage) (string, error) {
	index := "agents/" + d.Agent
	j, path := c.findJobKind(ctx, d.WorkspaceID, index, runs.KindAgent)
	if j == nil {
		return "", fmt.Errorf("agent %s not indexed", d.Agent)
	}
	def := j.LaunchDefaults()
	ifRunning := def.IfRunning
	if ifRunning == "" {
		ifRunning = "skip"
	}
	cwd := def.WorkingDir
	if cwd == "" {
		cwd = dirOf(path)
	}
	run, _, err := c.Runs.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID:   d.WorkspaceID,
			Job:           index,
			Kind:          runs.KindAgent,
			Reason:        runs.ReasonTrigger,
			Harness:       j.ResolvedHarness(),
			TraceID:       d.TraceID,
			MaxDurationMS: def.MaxDurationMS,
			IdleTimeoutMS: def.IdleTimeoutMS,
			KeepAliveMS:   def.KeepAliveMS,
			NudgeMS:       def.NudgeMS,
			WorkingDir:    cwd,
			Runtime:       j.Runtime,
		},
		Wake: &runs.WakeOpts{
			Message:   "Email from " + msg.From + ": " + msg.Subject,
			Untrusted: u,
		},
		Memory:    c.memoryLaunch(ctx, d.WorkspaceID, j),
		IfRunning: ifRunning,
		Env:       def.Env,
		Model:     def.Model,
		Effort:    def.Effort,
	})
	if err != nil {
		return "", err
	}
	if run == nil {
		return "", fmt.Errorf("agent %s launch returned no run", d.Agent)
	}
	c.log.Info("agent.woken", "reason", string(runs.ReasonTrigger), "agent", index, "channel", "email")
	return run.ID, nil
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

// RunMessage injects one operator reply into a live run (plan I.1).
func (c *ControlPlane) RunMessage(ctx context.Context, runID, text string) error {
	return c.Runs.InjectMessage(ctx, runID, text)
}

// ResolveInbox marks the item one reply decided as resolved (plan I.1).
func (c *ControlPlane) ResolveInbox(ctx context.Context, inboxID string) error {
	return c.Alerts.SetStatus(ctx, inboxID, alerts.InboxResolved, "email")
}

// AmbiguousReplyItem records the item for one approval reply that named
// both decisions (Part P).
func (c *ControlPlane) AmbiguousReplyItem(ctx context.Context, runID string, d email.RouteDecision, msg email.IngressMessage) (string, error) {
	return c.Alerts.AmbiguousReplyItem(ctx, runID, d, msg)
}

// startEmail attaches the poll loop to the service and runs it. The loop
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
