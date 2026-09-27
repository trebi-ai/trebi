package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/conversations"
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
			if c.Policy != nil {
				c.Policy.PruneIdle(time.Now(), 24*time.Hour)
			}
			c.drainQueue(ctx)
		}
	}
}

// channelWorker sends pending channel replies and pulses typing. It skips a
// tick under shadow or drain (plan G.3).
func (c *ControlPlane) channelWorker(ctx context.Context) {
	iv := c.Opts.ChannelInterval
	if iv <= 0 {
		iv = time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.Opts.Shadow || c.draining(ctx) {
				continue
			}
			if err := c.Channels.SendDue(ctx, time.Now()); err != nil {
				c.log.Warn("channel send due", "error", err)
			}
			if err := c.Channels.PulseTyping(ctx, time.Now()); err != nil {
				c.log.Warn("channel typing", "error", err)
			}
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
	due := time.Now().UTC().Format(time.RFC3339)
	rows, err := c.Store.DB().QueryContext(ctx, `
		SELECT DISTINCT workspace_id, job, kind, conversation_key FROM event_queue
		WHERE status='pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?) LIMIT 50`, due)
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
	// Policy is checked at the claim (plan E.2): a blocked row stays pending
	// with the first allowed attempt time.
	if c.policyDefer(ctx, item) {
		return
	}
	if kind == dispatch.KindAssistant {
		// The assistant takes one turn at a time per conversation (plan E.4):
		// the next item waits until the live turn ends.
		if c.assistantTurnActive(ctx, item) {
			_ = c.queue.Defer(ctx, item.ID, time.Now().Add(2*time.Second), "turn_active")
			return
		}
		// The assistant is a session; the lane prompts the manager and
		// marks the item done on the result (plan E).
		c.dispatchAssistant(ctx, item)
		return
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
	convKey := item.ConversationKey
	if id, key := c.channelConversation(ctx, item); id != "" {
		// An agent item from a connector channel binds its conversation
		// through the channel pointer, not the lane key (plan G.2). The
		// inbound text becomes the channel-typed user turn, so the reply
		// has a way back (plan G.3).
		convID, convKey = id, key
		c.noteChannelUser(ctx, item, id, prompt)
	}
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
	kit := c.Workspaces.OwnerKit(ctx, item.WorkspaceID, item.Job)
	d := j.LaunchDefaultsWithKit(kit)
	ifRunning := d.IfRunning
	if ifRunning == "" {
		// A conversation-keyed item coalesces by default so one busy chat
		// does not skip the next message (plan E.3).
		if item.ConversationKey != "" {
			ifRunning = "coalesce"
		} else {
			ifRunning = "skip"
		}
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
			Reason:          runsReasonFor(item),
			Harness:         harness,
			PromptPreview:   trunc(prompt, 200),
			QueueID:         item.ID,
			ConversationKey: convKey,
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
		Memory:    c.memoryLaunch(ctx, item.WorkspaceID, j),
		Kit:       kit,
		JobSkills: j.Skills, JobConnectors: j.Connectors,
		IfRunning: ifRunning,
		Env:       d.Env,
		Model:     d.Model,
		Effort:    d.Effort,
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
	if outcome != "skip" && outcome != "coalesce" {
		c.recordPolicy(ctx, item)
	}
	result := queueResult(outcome, run)
	if merr := c.queue.MarkDone(ctx, item.ID, result, run.ID); merr != nil {
		c.log.Warn("queue mark done", "queue_id", item.ID, "error", merr)
	}
	c.ackChannel(ctx, item, result)
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

func runsReasonFor(item *dispatch.QueueItem) runs.RunReason {
	if item.Connector == "escalation" {
		return runs.ReasonEscalation
	}
	return runs.ReasonTrigger
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
		if errors.Is(err, runs.ErrBudgetExceeded) {
			if merr := c.queue.MarkDone(ctx, item.ID, dispatch.ResultSkipped, ""); merr != nil {
				c.log.Warn("queue mark skipped", "queue_id", item.ID, "error", merr)
			}
			return
		}
		// Back off instead of requeueing at once: 15s << (attempts-1), cap 5m.
		backoff := 15 * time.Second << min(item.Attempts-1, 5)
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
		if rerr := c.queue.Defer(ctx, item.ID, time.Now().Add(backoff), "retry"); rerr != nil {
			c.log.Warn("queue defer", "queue_id", item.ID, "error", rerr)
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

// channelConversation resolves the conversation of an agent item that came
// from a connector channel (plan G.2). It returns the conversation id and
// key; an empty id means the item is not a channel item.
func (c *ControlPlane) channelConversation(ctx context.Context, item *dispatch.QueueItem) (string, string) {
	if item.Kind != runs.KindAgent || item.Connector == "" || item.ConversationKey == "" {
		return "", ""
	}
	if c.Connectors == nil || c.Conversations == nil {
		return "", ""
	}
	if _, _, ok := c.Connectors.Channel(item.Connector); !ok {
		return "", ""
	}
	convID, err := c.Conversations.EnsureForChannel(ctx,
		item.WorkspaceID, item.Job, item.Connector, item.ConversationKey)
	if err != nil || convID == "" {
		return "", ""
	}
	conv, err := c.Conversations.Get(ctx, convID)
	if err != nil {
		return convID, ""
	}
	return conv.ID, conv.Key
}

// noteChannelUser stores the inbound event text as the channel-typed user
// turn of an agent item. The row carries the channel, so the reply path can
// send the answer back (plan G.3). The event id is the idempotency key, so a
// retry writes nothing.
func (c *ControlPlane) noteChannelUser(ctx context.Context, item *dispatch.QueueItem, convID, text string) {
	if c.Conversations == nil || convID == "" {
		return
	}
	if strings.TrimSpace(text) == "" {
		text = "Event trigger " + item.Connector + " fired."
	}
	if _, _, err := c.Conversations.AppendUser(ctx, convID, conversations.UserTurn{
		Text:           text,
		ChannelType:    item.Connector,
		ChannelRef:     item.ConversationKey,
		IdempotencyKey: item.EventID,
	}); err != nil {
		c.log.Warn("channel user turn", "queue_id", item.ID, "error", err)
	}
}

// ackChannel runs the connector's ack command once for a finished queue item
// (plan G.3). A failure is logged and changes nothing else.
func (c *ControlPlane) ackChannel(ctx context.Context, item *dispatch.QueueItem, result dispatch.QueueResult) {
	if c.Channels == nil {
		return
	}
	switch result {
	case dispatch.ResultLaunched, dispatch.ResultCoalesced, dispatch.ResultSkipped:
	default:
		return
	}
	if err := c.Channels.Ack(ctx, *item); err != nil {
		c.log.Warn("channel ack", "queue_id", item.ID, "error", err)
	}
}

// blockedFor is the queue auth gate.
func (c *ControlPlane) blockedFor(ctx context.Context, _, harness string) dispatch.BlockReason {
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
	case "replace", "allow", "":
		// Fail-fast launch (nil error, failed run) is a dead event, not launched.
		if run.Status == runs.StatusFailed {
			return dispatch.ResultDead
		}
		return dispatch.ResultLaunched
	default:
		if run.Status == runs.StatusFailed {
			return dispatch.ResultDead
		}
		return dispatch.ResultLaunched
	}
}

// policyDefer applies the trigger policy window (plan E.2): a blocked row
// stays pending with the first allowed attempt time.
func (c *ControlPlane) policyDefer(ctx context.Context, item *dispatch.QueueItem) bool {
	if c.Policy == nil {
		return false
	}
	debounce, rate, key := c.itemPolicy(ctx, item)
	if key == "" {
		return false
	}
	until := c.Policy.NextAllowed(key, debounce, rate, time.Now())
	if until.IsZero() {
		return false
	}
	if err := c.queue.Defer(ctx, item.ID, until, "policy"); err != nil {
		c.log.Warn("queue policy defer", "queue_id", item.ID, "error", err)
	}
	return true
}

// assistantTurnActive reports whether the conversation of one assistant item
// is between message and turn completion. The next turn waits (plan E.4).
func (c *ControlPlane) assistantTurnActive(ctx context.Context, item *dispatch.QueueItem) bool {
	if c.asst == nil || c.Conversations == nil {
		return false
	}
	channelType := item.Connector
	if channelType == "" {
		channelType = conversations.ChannelAssistant
	}
	ref := item.ConversationKey
	if ref == "" {
		ref = "trigger:" + channelType
	}
	convID, err := c.Conversations.EnsureForChannel(ctx, item.WorkspaceID, "", channelType, ref)
	if err != nil || convID == "" {
		return false
	}
	return c.asst.TurnActive(ctx, convID)
}

// recordPolicy notes one fire for the trigger's rate limit.
func (c *ControlPlane) recordPolicy(ctx context.Context, item *dispatch.QueueItem) {
	if c.Policy == nil {
		return
	}
	_, rate, key := c.itemPolicy(ctx, item)
	if key != "" {
		c.Policy.Record(key, rate, time.Now())
	}
}

// itemPolicy returns the debounce, rate limit, and claim key of the trigger
// behind one queue item. The key matches TriggerRows: workspace, job, trigger
// index, conversation key.
func (c *ControlPlane) itemPolicy(ctx context.Context, item *dispatch.QueueItem) (time.Duration, *dispatch.RateLimit, string) {
	key := item.WorkspaceID + "/" + item.Job + "/" + strconv.Itoa(item.OnIndex) + "/" + item.ConversationKey
	if item.Kind == dispatch.KindAssistant {
		rows, err := c.Workspaces.AssistantTriggerRows(ctx)
		if err != nil {
			return 0, nil, ""
		}
		for _, at := range rows {
			if at.WorkspaceID != item.WorkspaceID {
				continue
			}
			for _, tr := range at.Triggers {
				if tr.Index == item.OnIndex && tr.Family == "event" && tr.Event != "" {
					return tr.Debounce, dispatch.ParseRateLimit(tr.RateLimit), key
				}
			}
		}
		return 0, nil, ""
	}
	j, _ := c.findJobKind(ctx, item.WorkspaceID, item.Job, item.Kind)
	if j == nil {
		return 0, nil, ""
	}
	for _, tr := range j.Triggers {
		if tr.Index == item.OnIndex && tr.Family == "event" && tr.Event != "" {
			return tr.Debounce, dispatch.ParseRateLimit(tr.RateLimit), key
		}
	}
	return 0, nil, ""
}
