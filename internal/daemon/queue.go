package daemon

import (
	"cmp"
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
				c.Policy.PruneIdle(c.now())
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

// now is the queue and policy clock.
func (c *ControlPlane) now() time.Time {
	if c.Opts.Now != nil {
		return c.Opts.Now()
	}
	return time.Now()
}

// queueLanes returns distinct lanes with due pending rows. A conversation
// lane counts only when its head row is due (FIFO inside a conversation).
func (c *ControlPlane) queueLanes(ctx context.Context) []lane {
	due := c.now().UTC().Format(time.RFC3339)
	rows, err := c.Store.DB().QueryContext(ctx, `
		SELECT DISTINCT workspace_id, job, kind, conversation_key FROM event_queue q
		WHERE status='pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		AND (conversation_key = '' OR id = (SELECT MIN(h.id) FROM event_queue h
			WHERE h.status='pending' AND h.workspace_id=q.workspace_id AND h.job=q.job
			AND h.kind=q.kind AND h.conversation_key=q.conversation_key))
		LIMIT 50`, due)
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
			_ = c.queue.Defer(ctx, item.ID, c.now().Add(2*time.Second), dispatch.DeferTurn)
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
	if item.ResumeOf != "" {
		c.dispatchResume(ctx, item)
		return
	}
	prompt := queuePrompt(item, j)
	coalesceText := prompt
	convID := c.conversationID(ctx, item.WorkspaceID, item.ConversationKey)
	convKey := item.ConversationKey
	if id, key, ref := c.channelConversation(ctx, item); id != "" {
		// An agent item from a connector channel binds its conversation
		// through the channel pointer, not the lane key (plan G.2). The
		// inbound text becomes the channel-typed user turn, so the reply
		// has a way back (plan G.3).
		convID, convKey = id, key
		c.noteChannelUser(ctx, item, id, ref, prompt)
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
		coalesceText = wake.Message
	}
	harness := j.ResolvedHarness()
	if reason := c.blockedFor(ctx, kind, harness); reason != "" {
		_ = c.queue.MarkBlocked(ctx, item.ID, reason)
		return
	}
	if !c.markAttempt(ctx, item) {
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
		Prompt:       prompt,
		CoalesceText: coalesceText,
		Wake:         wake,
		Memory:       c.memoryLaunch(ctx, item.WorkspaceID, j),
		Kit:          kit,
		JobSkills:    j.Skills, JobConnectors: j.Connectors,
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
	result := queueResult(outcome, run)
	if result == dispatch.ResultLaunched || result == dispatch.ResultCoalesced {
		c.recordPolicy(ctx, item)
	}
	if merr := c.queue.MarkDone(ctx, item.ID, result, run.ID); merr != nil {
		c.log.Warn("queue mark done", "queue_id", item.ID, "error", merr)
	} else {
		c.ackChannel(ctx, item, result)
	}
	if item.Kind == runs.KindAgent {
		c.log.Info("agent.woken", "reason", string(runs.ReasonTrigger), "agent", item.Job)
	}
}

// markAttempt counts one launch attempt just before the launch. It marks
// the row dead and returns false when the row has no attempts left.
func (c *ControlPlane) markAttempt(ctx context.Context, item *dispatch.QueueItem) bool {
	n, err := c.queue.MarkAttempt(ctx, item.ID)
	if err != nil {
		c.log.Warn("queue mark attempt", "queue_id", item.ID, "error", err)
		return false
	}
	item.Attempts = n
	if n > dispatch.MaxAttempts {
		if merr := c.queue.MarkDone(ctx, item.ID, dispatch.ResultDead, ""); merr != nil {
			c.log.Warn("queue mark dead", "queue_id", item.ID, "error", merr)
		}
		return false
	}
	return true
}

func (c *ControlPlane) dispatchResume(ctx context.Context, item *dispatch.QueueItem) {
	if !c.markAttempt(ctx, item) {
		return
	}
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
	// An action error is a config fault: a retry sends the same launch.
	if errors.Is(err, runs.ErrActionsInvalid) || errors.Is(err, runs.ErrActionsUnsupported) {
		if merr := c.queue.MarkDead(ctx, item.ID, err.Error()); merr != nil {
			c.log.Warn("queue mark dead", "queue_id", item.ID, "error", merr)
		}
		return
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
		if rerr := c.queue.Defer(ctx, item.ID, c.now().Add(backoff), dispatch.DeferRetry); rerr != nil {
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
// key, and the channel ref of the event for the reply; an empty id means the
// item is not a channel item.
func (c *ControlPlane) channelConversation(ctx context.Context, item *dispatch.QueueItem) (convID, key, ref string) {
	if item.Kind != runs.KindAgent || item.Connector == "" || item.ConversationKey == "" {
		return "", "", ""
	}
	if c.Connectors == nil || c.Conversations == nil {
		return "", "", ""
	}
	if _, _, ok := c.Connectors.Channel(item.Connector); !ok {
		return "", "", ""
	}
	ref = c.eventChannelRef(ctx, item.EventID)
	pointer := cmp.Or(ref, item.ConversationKey)
	convID, err := c.Conversations.EnsureForChannel(ctx, item.WorkspaceID, item.Job, item.Connector, pointer)
	if err != nil || convID == "" {
		c.noteOperatorConflict(ctx, err, item, item.Connector, pointer, item.Job)
		return "", "", ""
	}
	conv, err := c.Conversations.Get(ctx, convID)
	if err != nil {
		return convID, "", ref
	}
	return conv.ID, conv.Key, ref
}

// eventChannelRef reads the channel ref of the event behind a queue item.
// The conversation key is only the lane key; the reply goes to this ref.
func (c *ControlPlane) eventChannelRef(ctx context.Context, eventID string) string {
	if eventID == "" {
		return ""
	}
	var ref string
	// A missing event row means no channel address.
	_ = c.Store.DB().QueryRowContext(ctx,
		`SELECT COALESCE(channel_ref,'') FROM events WHERE id=?`, eventID).Scan(&ref)
	return ref
}

// noteOperatorConflict logs a channel that already belongs to another
// operator. The doctor reports the trigger setup that causes it.
func (c *ControlPlane) noteOperatorConflict(ctx context.Context, err error, item *dispatch.QueueItem, channelType, ref, operator string) {
	if !errors.Is(err, conversations.ErrOperatorConflict) {
		return
	}
	current := ""
	if p, perr := c.Conversations.GetPointer(ctx, item.WorkspaceID, channelType, ref); perr == nil {
		if conv, gerr := c.Conversations.Get(ctx, p.ConversationID); gerr == nil {
			current = conv.Operator
		}
	}
	c.log.WarnContext(ctx, "channel operator conflict", "connector", channelType, "ref", ref,
		"operator", operator, "current_operator", current, "queue_id", item.ID)
}

// noteChannelUser stores the inbound event text as the channel-typed user
// turn of an agent item. The row carries the channel and the event's channel
// ref, so the reply path can send the answer back (plan G.3). An empty ref
// gets no reply. The event id is the idempotency key, so a retry writes
// nothing.
func (c *ControlPlane) noteChannelUser(ctx context.Context, item *dispatch.QueueItem, convID, ref, text string) {
	if c.Conversations == nil || convID == "" {
		return
	}
	if strings.TrimSpace(text) == "" {
		text = "Event trigger " + item.Connector + " fired."
	}
	if _, _, err := c.Conversations.AppendUser(ctx, convID, conversations.UserTurn{
		Text:           text,
		ChannelType:    item.Connector,
		ChannelRef:     ref,
		IdempotencyKey: item.EventID,
	}); err != nil {
		c.log.Warn("channel user turn", "queue_id", item.ID, "error", err)
	}
}

// ackChannel runs the connector's ack command once for a finished queue item
// (plan G.3) in the background, so a slow ack does not hold the queue. A
// failure is logged and changes nothing else. Stop waits for it.
func (c *ControlPlane) ackChannel(ctx context.Context, item *dispatch.QueueItem, result dispatch.QueueResult) {
	if c.Channels == nil {
		return
	}
	switch result {
	case dispatch.ResultLaunched, dispatch.ResultCoalesced, dispatch.ResultSkipped:
	default:
		return
	}
	it := *item
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		if err := c.Channels.Ack(ctx, it); err != nil {
			c.log.Warn("channel ack", "queue_id", it.ID, "error", err)
		}
	}()
}

// Busy implements channels.LiveTurns: an assistant turn is in flight, or a
// running run of the conversation is not waiting and not idle.
func (c *ControlPlane) Busy(ctx context.Context, convID string) bool {
	if c.asst != nil && c.asst.TurnActive(ctx, convID) {
		return true
	}
	rows, err := c.Store.DB().QueryContext(ctx,
		`SELECT id FROM runs WHERE conversation_id=? AND status='running'`, convID)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			continue
		}
		if state := c.Runs.SessionState(id); state != runs.SessionWaiting && state != runs.SessionIdle {
			return true
		}
	}
	return false
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
	until := c.Policy.NextAllowed(key, debounce, rate, c.now())
	if until.IsZero() {
		return false
	}
	if err := c.queue.Defer(ctx, item.ID, until, dispatch.DeferPolicy); err != nil {
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
	convID, _, _, err := c.assistantConversation(ctx, item)
	if err != nil || convID == "" {
		return false
	}
	return c.asst.TurnActive(ctx, convID)
}

// assistantConversation resolves the conversation of one assistant item:
// the channel pointer of the item connector and the event's channel ref.
// An event with no channel ref points by the conversation key and gets no
// reply ref. An item with neither shares one trigger conversation per
// channel, and its trigger ref gets no reply either.
func (c *ControlPlane) assistantConversation(ctx context.Context, item *dispatch.QueueItem) (convID, channelType, ref string, err error) {
	channelType = cmp.Or(item.Connector, conversations.ChannelAssistant)
	ref = c.eventChannelRef(ctx, item.EventID)
	pointer := ref
	switch {
	case ref != "":
	case item.ConversationKey != "":
		pointer = item.ConversationKey
	default:
		pointer = "trigger:" + channelType
		ref = pointer
	}
	convID, err = c.Conversations.EnsureForChannel(ctx, item.WorkspaceID, "", channelType, pointer)
	if err != nil {
		c.noteOperatorConflict(ctx, err, item, channelType, pointer, "")
	}
	return convID, channelType, ref, err
}

// recordPolicy notes one fire for the trigger's rate limit.
func (c *ControlPlane) recordPolicy(ctx context.Context, item *dispatch.QueueItem) {
	if c.Policy == nil {
		return
	}
	debounce, rate, key := c.itemPolicy(ctx, item)
	if key != "" {
		c.Policy.Record(key, debounce, rate, c.now())
	}
}

// itemPolicy returns the debounce, rate limit, and claim key of the trigger
// behind one queue item. The key matches TriggerRows: workspace, job, trigger
// index, conversation key. A resume item, or an item whose connector is not
// the event of the trigger at OnIndex (escalation, conversation wake), has
// no policy.
func (c *ControlPlane) itemPolicy(ctx context.Context, item *dispatch.QueueItem) (time.Duration, *dispatch.RateLimit, string) {
	if item.ResumeOf != "" {
		return 0, nil, ""
	}
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
				// The pipeline takes an assistant trigger with no family.
				if tr.Index == item.OnIndex && (tr.Family == "" || tr.Family == "event") && tr.Event == item.Connector {
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
		if tr.Index == item.OnIndex && tr.Family == "event" && tr.Event == item.Connector {
			return tr.Debounce, dispatch.ParseRateLimit(tr.RateLimit), key
		}
	}
	return 0, nil, ""
}
