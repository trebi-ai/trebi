package daemon

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
)

// wakeRecapN is the message count folded into a composed wake recap.
const wakeRecapN = 10

// lane is one FIFO stream of pending queue rows.
type lane struct{ ws, job, kind, conv string }

func (c *ControlPlane) queueWorker(ctx context.Context) {
	iv := c.Opts.QueueInterval
	if iv <= 0 {
		iv = 2 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.draining(ctx) {
				continue
			}
			c.drainQueue(ctx)
		}
	}
}

func (c *ControlPlane) drainQueue(ctx context.Context) {
	c.tickCfg, _ = config.Load()
	defer func() { c.tickCfg = nil }()
	if _, err := c.queue.ReclaimStale(ctx, 2*time.Minute); err != nil {
		c.log.Warn("queue reclaim", "error", err)
	}
	c.releaseAuthBlocks(ctx)
	for _, l := range c.queueLanes(ctx) {
		c.dispatchLane(ctx, l)
	}
}

// releaseAuthBlocks requeues items blocked on auth_missing once the harness
// stamp is ok again — recovery must drain without a manual requeue.
func (c *ControlPlane) releaseAuthBlocks(ctx context.Context) {
	rows, err := c.Store.DB().QueryContext(ctx, `
		SELECT id, workspace_id, job, kind FROM event_queue
		WHERE status='blocked' AND block_reason=?`, string(dispatch.BlockAuthMissing))
	if err != nil {
		c.log.Warn("list auth-blocked queue", "error", err)
		return
	}
	defer rows.Close()
	type blocked struct{ id, ws, job, kind string }
	var list []blocked
	for rows.Next() {
		var r blocked
		if rows.Scan(&r.id, &r.ws, &r.job, &r.kind) == nil {
			list = append(list, r)
		}
	}
	if err := rows.Err(); err != nil {
		c.log.Warn("list auth-blocked queue", "error", err)
		return
	}
	for _, r := range list {
		j, _ := c.findJobKind(ctx, r.ws, r.job, r.kind)
		name := "claude"
		if j != nil && j.Harness != "" {
			name = j.Harness
		}
		kind := r.kind
		if kind == "" {
			kind = "job"
		}
		if c.blockedFor(ctx, kind, name) == "" {
			if err := c.queue.Requeue(ctx, r.id); err != nil {
				c.log.Warn("requeue after auth", "queue_id", r.id, "error", err)
			}
		}
	}
}

// queueLanes returns distinct lanes with pending rows.
func (c *ControlPlane) queueLanes(ctx context.Context) []lane {
	rows, err := c.Store.DB().QueryContext(ctx, `
		SELECT DISTINCT workspace_id, job, kind, conversation_key FROM event_queue
		WHERE status='pending' LIMIT 50`)
	if err != nil {
		c.log.Warn("queue lane query", "error", err)
		return nil
	}
	defer rows.Close()
	var lanes []lane
	for rows.Next() {
		var l lane
		if rows.Scan(&l.ws, &l.job, &l.kind, &l.conv) == nil {
			lanes = append(lanes, l)
		}
	}
	return lanes
}

// dispatchLane claims the next pending item on a lane and launches it.
func (c *ControlPlane) dispatchLane(ctx context.Context, l lane) {
	item, err := c.queue.ClaimNext(ctx, l.ws, l.job, l.kind, l.conv)
	if err != nil || item == nil {
		return
	}
	kind := item.Kind
	if kind == "" {
		kind = "job"
	}
	j, path := c.findJobKind(ctx, l.ws, l.job, kind)
	if j == nil {
		_ = c.queue.MarkBlocked(ctx, item.ID, dispatch.BlockJobMissing)
		return
	}
	if item.Attempts > dispatch.MaxAttempts {
		_ = c.queue.MarkDone(ctx, item.ID, dispatch.ResultDead, "")
		return
	}
	if item.ResumeOf != "" {
		c.dispatchResume(ctx, item)
		return
	}
	prompt := queuePrompt(item, j)
	convID := c.conversationID(ctx, item.WorkspaceID, item.ConversationKey)
	var wake *runs.WakeOpts
	if kind != runs.KindJob {
		// Operator lane (Part F.3): the item prompt is the wake message; the
		// var-expanded operator body feeds the composer at dispatch time.
		wake = &runs.WakeOpts{
			Body:    expandVars(j.Prompt, item.Vars),
			Message: strings.TrimSpace(item.Prompt),
		}
		if convID != "" && c.Conversations != nil {
			wake.Recap = c.Conversations.Recap(ctx, convID, wakeRecapN)
		}
		prompt = ""
	}
	harness := j.ResolvedHarness()
	if reason := c.blockedFor(ctx, kind, harness); reason != "" {
		_ = c.queue.MarkBlocked(ctx, item.ID, reason)
		return
	}
	d := j.LaunchDefaults()
	ifRunning := d.IfRunning
	if ifRunning == "" {
		ifRunning = "skip"
	}
	cwd := d.WorkingDir
	if cwd == "" {
		cwd = dirOf(path)
	}
	var trigger *runs.TriggerCapture
	if item.Vars != nil || item.EventID != "" {
		trigger = &runs.TriggerCapture{
			EventID: item.EventID, Connector: item.Connector, Vars: item.Vars,
		}
	}
	run, outcome, err := c.Runs.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID:     item.WorkspaceID,
			Job:             item.Job,
			Kind:            item.Kind,
			Reason:          runs.ReasonTrigger,
			Harness:         harness,
			PromptPreview:   trunc(prompt, 200),
			QueueID:         item.ID,
			ConversationKey: item.ConversationKey,
			ConversationID:  convID,
			TraceID:         item.TraceID,
			Trigger:         trigger,
			MaxDurationMS:   d.MaxDurationMS,
			IdleTimeoutMS:   d.IdleTimeoutMS,
			KeepAliveMS:     d.KeepAliveMS,
			NudgeMS:         d.NudgeMS,
			WorkingDir:      cwd,
			Runtime:         j.Runtime,
		},
		Prompt:    prompt,
		Wake:      wake,
		IfRunning: ifRunning,
		Env:       d.Env,
	})
	if err != nil || run == nil {
		c.log.Warn("queue launch", "job", item.Job, "queue_id", item.ID, "error", err)
		c.failOrRetry(ctx, item, err, run)
		return
	}
	if run.Status == runs.StatusFailed && (run.EndReason == runs.EndAuth || run.EndReason == runs.EndLimit) {
		reason := dispatch.BlockAuthMissing
		if run.EndReason == runs.EndLimit {
			reason = dispatch.BlockLimitExceeded
		}
		_ = c.queue.MarkBlocked(ctx, item.ID, reason)
		return
	}
	if merr := c.queue.MarkDone(ctx, item.ID, queueResult(outcome, run), run.ID); merr != nil {
		c.log.Warn("queue mark done", "queue_id", item.ID, "error", merr)
	}
	if item.Kind == runs.KindAssistant {
		c.log.Info("assistant.woken", "reason", string(runs.ReasonTrigger), "channel", "")
	}
	if item.Kind == runs.KindAgent {
		c.log.Info("agent.woken", "reason", string(runs.ReasonTrigger), "agent", item.Job)
	}
}

func (c *ControlPlane) dispatchResume(ctx context.Context, item *dispatch.QueueItem) {
	run, err := c.Runs.Resume(ctx, item.ResumeOf)
	if err != nil || run == nil {
		c.log.Warn("queue resume", "job", item.Job, "queue_id", item.ID, "error", err)
		c.failOrRetry(ctx, item, err, run)
		return
	}
	if merr := c.queue.MarkDone(ctx, item.ID, dispatch.ResultLaunched, run.ID); merr != nil {
		c.log.Warn("queue mark done", "queue_id", item.ID, "error", merr)
	}
}

func (c *ControlPlane) failOrRetry(ctx context.Context, item *dispatch.QueueItem, err error, run *runs.Run) {
	if run != nil && run.Status == runs.StatusFailed {
		switch run.EndReason {
		case runs.EndAuth:
			_ = c.queue.MarkBlocked(ctx, item.ID, dispatch.BlockAuthMissing)
			return
		case runs.EndLimit:
			_ = c.queue.MarkBlocked(ctx, item.ID, dispatch.BlockLimitExceeded)
			return
		}
	}
	if err != nil && item.Attempts < dispatch.MaxAttempts {
		if rerr := c.queue.Requeue(ctx, item.ID); rerr != nil {
			c.log.Warn("queue requeue", "queue_id", item.ID, "error", rerr)
		}
		return
	}
	if merr := c.queue.MarkDone(ctx, item.ID, dispatch.ResultDead, ""); merr != nil {
		c.log.Warn("queue mark dead", "queue_id", item.ID, "error", merr)
	}
}

func (c *ControlPlane) conversationID(ctx context.Context, workspaceID, key string) string {
	if c.Conversations == nil || key == "" {
		return ""
	}
	id, err := c.Conversations.IDByKey(ctx, workspaceID, key)
	if err != nil {
		return ""
	}
	return id
}

// blockedFor is the queue auth gate. Native assistants probe the API key, not the vendor stamp.
func (c *ControlPlane) blockedFor(ctx context.Context, kind, harness string) dispatch.BlockReason {
	if kind == runs.KindAssistant {
		cfg := c.tickCfg
		if cfg == nil {
			cfg, _ = config.Load()
		}
		if cfg != nil && strings.TrimSpace(cfg.Assistant.Model) != "" {
			if cfg.Assistant.Enabled() {
				return ""
			}
			return dispatch.BlockAuthMissing
		}
	}
	return c.harnessBlocked(ctx, harness)
}

// harnessBlocked returns a BlockReason when harness_state says not ready.
func (c *ControlPlane) harnessBlocked(ctx context.Context, harness string) dispatch.BlockReason {
	if harness == "" || harness == "fake" {
		return ""
	}
	// Skip gate when using CI fake harness
	if os.Getenv("TREBI_FAKE_HARNESS") != "" {
		return ""
	}
	var authOK, limitExc sql.NullInt64
	err := c.Store.DB().QueryRowContext(ctx,
		`SELECT auth_ok, limit_exceeded FROM harness_state WHERE name=?`, harness).
		Scan(&authOK, &limitExc)
	if err != nil {
		// no row → unknown, allow launch (probe at readiness)
		return ""
	}
	if limitExc.Valid && limitExc.Int64 == 1 {
		return dispatch.BlockLimitExceeded
	}
	if authOK.Valid && authOK.Int64 == 0 {
		return dispatch.BlockAuthMissing
	}
	return ""
}

// queuePrompt picks the item prompt (job prompt as fallback) and expands vars.
func queuePrompt(item *dispatch.QueueItem, j *job.Job) string {
	prompt := item.Prompt
	if prompt == "" {
		prompt = j.Prompt
	}
	return expandVars(prompt, item.Vars)
}

// expandVars applies {{var}} substitutions from the trigger extract.
func expandVars(prompt string, vars map[string]string) string {
	for k, v := range vars {
		prompt = replaceVar(prompt, k, v)
	}
	return prompt
}

// queueResult maps the if_running outcome to a queue result.
func queueResult(outcome string, run *runs.Run) dispatch.QueueResult {
	switch outcome {
	case "skip":
		return dispatch.ResultSkipped
	case "coalesce":
		return dispatch.ResultCoalesced
	default:
		// Fail-fast launch (nil error, failed run) is a dead event, not launched.
		if run.Status == runs.StatusFailed {
			return dispatch.ResultDead
		}
		return dispatch.ResultLaunched
	}
}
