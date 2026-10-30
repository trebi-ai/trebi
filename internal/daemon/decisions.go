package daemon

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/artifacts"
	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/decisions"
	"github.com/trebi-ai/trebi/internal/decisions/policies"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/job"
)

// buildDecisions wires the decisions service with its policies and
// deciders, and gives the reply check to email.
func (c *ControlPlane) buildDecisions(blobs *artifacts.Blobs) {
	liveCfg := func() *config.Config {
		cfg, err := config.Load()
		if err != nil || cfg == nil {
			return &config.Config{}
		}
		return cfg
	}
	c.Decisions = decisions.NewService(c.Store, blobs, func() config.DecisionsConfig { return liveCfg().Decisions })
	c.Decisions.Register(policies.EmailReply())
	c.Decisions.AddDecider("harness", harnessDecider{cfg: liveCfg})
	c.Decisions.AddDecider("native", decisions.NewNative(decisions.NativeDeps{
		Config: assistant.LiveNativeConfig,
		Model: func() string {
			return cmp.Or(liveCfg().Decisions.Model, assistant.LiveNativeConfig().Model)
		},
		NewModel: assistant.NewNativeModel,
		Guard: func(ctx context.Context) error {
			return assistant.NativeDailyCapGuard(ctx, c.asstStore, assistant.LiveNativeConfig().Limits.DailyTokens)
		},
	}))
	c.Email.WithReplyChecker(c)
}

// harnessDecider runs the default harness of the live config, so a change
// of the default needs no restart.
type harnessDecider struct{ cfg func() *config.Config }

func (h harnessDecider) Name() string { return "harness:" + h.name() }

func (h harnessDecider) Decide(ctx context.Context, r decisions.Request) (decisions.Result, error) {
	name := h.name()
	bin, env := agentcli.Process(name)
	if bin == "" {
		return decisions.Result{}, fmt.Errorf("harness %s is not installed", name)
	}
	return decisions.NewHarness(name, bin, "", env).Decide(ctx, r)
}

// CheckReply runs email.reply_risk on a participant reply.
func (c *ControlPlane) CheckReply(ctx context.Context, d email.RouteDecision, msg email.IngressMessage) (email.ReplyCheck, error) {
	text := strings.TrimSpace(msg.Text)
	for _, a := range msg.Attachments {
		text += fmt.Sprintf("\n[attachment: %s, %s]", a.Name, a.Type)
	}
	dec, err := c.Decisions.Decide(ctx, d.WorkspaceID, policies.EmailReplyKind, decisions.Subject{Kind: "email", ID: msg.ID},
		decisions.State{Text: "Subject: " + msg.Subject + "\n\n" + text, Untrusted: true, Facts: c.replyFacts(ctx, d, msg)})
	if err != nil {
		return email.ReplyCheck{Failed: true}, err
	}
	return email.ReplyCheck{DecisionID: dec.ID, Hold: dec.Outcome == policies.EmailHold,
		Failed: dec.Status != decisions.StatusOK, Reasons: dec.Reasons}, nil
}

// replyFacts are the trusted facts of the reply check: who the agent is and
// what it can do.
func (c *ControlPlane) replyFacts(ctx context.Context, d email.RouteDecision, msg email.IngressMessage) map[string]string {
	facts := map[string]string{policies.FactAgent: d.Agent, policies.FactCanSend: "false", policies.FactCanOperate: "false"}
	if _, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(msg.From)), "@"); ok {
		facts[policies.FactSenderDomain] = domain
	}
	row, err := c.Workspaces.JobByName(ctx, d.WorkspaceID, "agents/"+d.Agent)
	if err != nil {
		return facts
	}
	j, err := job.ParseFile(row.Path)
	if err != nil || j == nil {
		return facts
	}
	facts[policies.FactDescription] = j.Description
	if j.Kit != nil && len(j.Kit.Connectors) > 0 {
		facts[policies.FactConnectors] = strings.Join(j.Kit.Connectors, ", ")
		facts[policies.FactCanSend] = "true"
		for _, n := range j.Kit.Connectors {
			if n != job.EmailConnector {
				facts[policies.FactCanOperate] = "true"
			}
		}
	}
	return facts
}

func (h harnessDecider) name() string {
	fields := strings.Fields(h.cfg().Defaults.Command)
	if len(fields) == 0 {
		return "claude"
	}
	return fields[0]
}
