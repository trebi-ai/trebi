package daemon

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trebi-ai/trebi/internal/chains"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// wakeRecapN is the message count folded into a composed wake recap.
const wakeRecapN = 10

// lane is one FIFO stream of pending queue rows.
type lane struct{ ws, jobID, kind, conv string }

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

// Retry policies of the outbox kinds (plan 2026-09-28 A.2).
var (
	channelPolicy = outbox.Policy{Base: 5 * time.Second, Max: 5 * time.Minute, Attempts: 5}   //nolint:gochecknoglobals // static policy
	emailPolicy   = outbox.Policy{Base: 30 * time.Second, Max: 10 * time.Minute, Attempts: 5} //nolint:gochecknoglobals // static policy
)

// outboxPruneEvery is how often the outbox worker prunes old rows.
const outboxPruneEvery = time.Hour

// outboxWorker sends due outbox rows and pulses typing. It skips a tick
// under shadow or drain (plan G.3), and prunes old rows once an hour.
func (c *ControlPlane) outboxWorker(ctx context.Context) {
	iv := c.Opts.OutboxInterval
	if iv <= 0 {
		iv = time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	var lastPrune time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.Opts.Shadow || c.draining(ctx) {
				continue
			}
			now := time.Now()
			if err := c.Outbox.SendDue(ctx, now); err != nil {
				c.log.Warn("outbox send due", "error", err)
			}
			if c.Channels != nil {
				if err := c.Channels.PulseTyping(ctx, now); err != nil {
					c.log.Warn("channel typing", "error", err)
				}
			}
			if now.Sub(lastPrune) >= outboxPruneEvery {
				lastPrune = now
				if err := c.Outbox.Prune(ctx, now); err != nil {
					c.log.Warn("outbox prune", "error", err)
				}
				c.pruneEmailContent(ctx, now)
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

// releaseAuthBlocks requeues items blocked on auth_missing or
// limit_exceeded once the harness stamp is ok again — recovery must drain
// without a manual requeue.
func (c *ControlPlane) releaseAuthBlocks(ctx context.Context) {
	rows, err := c.Store.DB().QueryContext(ctx, `
		SELECT id, workspace_id, job_id, `+store.JobNameSQL("event_queue.job_id")+`, kind FROM event_queue
		WHERE status='blocked' AND block_reason IN (?, ?)`, string(dispatch.BlockAuthMissing), string(dispatch.BlockLimitExceeded))
	if err != nil {
		c.log.Warn("list auth-blocked queue", "error", err)
		return
	}
	defer rows.Close()
	type blocked struct{ id, ws, job, name, kind string }
	var list []blocked
	for rows.Next() {
		var r blocked
		if rows.Scan(&r.id, &r.ws, &r.job, &r.name, &r.kind) == nil {
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
		if j != nil {
			name = j.ResolvedHarness(c.Workspaces.OwnerKit(ctx, r.ws, r.name))
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

// queueLanes returns distinct lanes with due pending rows. A lane counts
// only when its head row is due (FIFO inside each lane).
func (c *ControlPlane) queueLanes(ctx context.Context) []lane {
	due := c.now().UTC().Format(time.RFC3339)
	rows, err := c.Store.DB().QueryContext(ctx, `
		SELECT DISTINCT workspace_id, job_id, kind, conversation_key FROM event_queue q
		WHERE status='pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		AND id = (SELECT MIN(h.id) FROM event_queue h
			WHERE h.status='pending' AND h.job_id=q.job_id
			AND h.kind=q.kind AND h.conversation_key=q.conversation_key)
		LIMIT 50`, due)
	if err != nil {
		c.log.Warn("queue lane query", "error", err)
		return nil
	}
	defer rows.Close()
	var lanes []lane
	for rows.Next() {
		var l lane
		if rows.Scan(&l.ws, &l.jobID, &l.kind, &l.conv) == nil {
			lanes = append(lanes, l)
		}
	}
	return lanes
}

// dispatchLane claims the next pending item on a lane and launches it.
func (c *ControlPlane) dispatchLane(ctx context.Context, l lane) {
	item, err := c.queue.ClaimNext(ctx, l.jobID, l.kind, l.conv)
	if err != nil || item == nil {
		return
	}
	kind := item.Kind
	if kind == "" {
		kind = "job"
	}
	if item.Launch != nil {
		c.launchDeferred(ctx, item)
		return
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
	j, path := c.findJobKind(ctx, l.ws, l.jobID, kind)
	if j == nil || outsideAgent(j) {
		_ = c.queue.MarkBlocked(ctx, item.ID, dispatch.BlockJobMissing)
		return
	}
	if item.ResumeOf != "" {
		c.dispatchResume(ctx, item)
		return
	}
	var batch *batchRun
	if dispatch.IsBatchLane(item.ConversationKey) {
		if batch = c.loadBatch(ctx, item); batch == nil {
			return
		}
	}
	prompt := queuePrompt(item, j)
	if kind != runs.KindJob {
		// The body of an operator is not its message. A channel event
		// carries the message as the text var.
		prompt = cmp.Or(strings.TrimSpace(item.Prompt), strings.TrimSpace(item.Vars["text"]))
	}
	if batch != nil {
		prompt = batch.prompt(j.Prompt, kind)
	}
	var followUp *runs.FollowUp
	if item.FollowUp && kind == runs.KindJob {
		followUp = c.followUp(ctx, item, j)
		prompt = followUp.Text
	}
	coalesceText := prompt
	if kind == runs.KindJob && followUp == nil && batch == nil && item.EventID != "" && item.Vars["text"] != "" {
		// A live or continued session has the job prompt; it gets only the message.
		coalesceText = c.messageText(ctx, item)
	}
	convID := c.conversationID(ctx, item.WorkspaceID, item.ConversationKey)
	convKey := item.ConversationKey
	chat := "" // the chat section of a job that replies in a chat
	switch {
	case batch != nil:
		// A batch run has no conversation; the lane key only orders it.
		convID, convKey = "", ""
	case kind == runs.KindJob && item.Connector == conversations.ConnectorConversation && convID != "":
		// A person writes in the app into a chat that this job answers. Send
		// already stored the user row, so the reply stays in the app.
		text := item.Prompt
		chat = c.chatSection(ctx, item, convID, true)
		prompt = chat + "\n\n" + expandVars(j.Prompt, item.PromptVars()) + "\n\nMessage: " + text
		coalesceText = text
	default:
		id, key, addr := c.channelConversation(ctx, item, j)
		if id == "" {
			break
		}
		// A channel item of an agent, or of a job that replies in the chat,
		// binds its conversation through the channel pointer, not the lane
		// key (plan G.2). The inbound text becomes the channel-typed user
		// turn, so the reply has a way back (plan G.3).
		convID, convKey = id, key
		userText := prompt
		if kind == runs.KindJob {
			userText = item.Vars["text"]
			chat = c.chatSection(ctx, item, id, false)
			prompt = chat + "\n\n" + prompt
		}
		c.noteChannelUser(ctx, item, id, addr, userText)
	}
	frame, ok := c.chainFrame(ctx, item)
	if !ok {
		return
	}
	var wake *runs.WakeOpts
	if kind != runs.KindJob {
		// Operator lane (Part F.3): the item prompt is the wake message; the
		// var-expanded operator body feeds the composer at dispatch time.
		wake = &runs.WakeOpts{
			Body:    expandVars(j.Prompt, item.PromptVars()),
			Message: prompt,
		}
		if batch != nil {
			wake.Body = fillTemplate(j.Prompt, batch.vars)
		}
		if frame != nil {
			wake.Chain = frame.Wake
		}
		if convID != "" && c.Conversations != nil {
			wake.Recap = c.Conversations.Recap(ctx, convID, wakeRecapN)
		}
		prompt = ""
		coalesceText = wake.Message
	}
	kit := c.Workspaces.OwnerKit(ctx, item.WorkspaceID, item.JobName)
	harness := j.ResolvedHarness(kit)
	if c.heldItem(ctx, item, kind, harness, false) {
		return
	}
	if c.waitHarnessUpdate(ctx, item, harness) {
		return
	}
	if reason := c.blockedFor(ctx, kind, harness); reason != "" {
		_ = c.queue.MarkBlocked(ctx, item.ID, reason)
		return
	}
	if !c.markAttempt(ctx, item) {
		return
	}
	d := j.LaunchDefaultsWithKit(kit)
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
	var runID string
	var files map[string][]byte
	if batch != nil {
		trigger, runID, files = batch.trigger(item), batch.runID, batch.files()
	}
	run, outcome, err := c.Runs.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			ID:              runID,
			WorkspaceID:     item.WorkspaceID,
			JobID:           item.JobID,
			JobName:         item.JobName,
			Kind:            item.Kind,
			Reason:          runsReasonFor(item, frame),
			Harness:         harness,
			Model:           d.Model,
			PromptPreview:   trunc(strings.TrimPrefix(prompt, chat+"\n\n"), 200),
			QueueID:         item.ID,
			ConversationKey: convKey,
			ConversationID:  convID,
			TraceID:         item.TraceID,
			HopID:           item.HopID,
			ChainPath:       frame.path(),
			Trigger:         trigger,
			Inputs:          item.Inputs,
			MaxDurationMS:   d.MaxDurationMS,
			IdleTimeoutMS:   d.IdleTimeoutMS,
			KeepAliveMS:     d.KeepAliveMS,
			NudgeMS:         d.NudgeMS,
			WorkingDir:      cwd,
			Runtime:         j.Runtime,
			FolderEnv:       d.FolderEnv,
		},
		Prompt:       prompt,
		CoalesceText: coalesceText,
		Wake:         wake,
		Memory:       c.memoryLaunch(ctx, item.WorkspaceID, j),
		StateDecl:    j.State,
		Kit:          kit,
		JobSkills:    j.Skills, JobConnectors: j.Connectors,
		// A message of a conversation goes into its live run (plan A.6).
		Conversation: convKey != "",
		ContinueMS:   d.ContinueMS,
		FollowUp:     followUp,
		IfRunning:    d.IfRunning,
		Env:          d.Env,
		Effort:       d.Effort,
		Files:        files,
	})
	done := c.finishLaunch(ctx, item, run, outcome, err)
	c.bindHop(ctx, item, run, err, done)
	if !done {
		return
	}
	if item.Kind == runs.KindAgent {
		c.log.Info("agent.woken", "reason", string(runs.ReasonTrigger), "agent", item.JobName)
	}
}

// finishLaunch writes the queue result of one launch. A live run of the
// same job puts the row back to wait; the wait is not an attempt. Only a
// new run counts toward the trigger limit. It reports whether the row is
// done.
func (c *ControlPlane) finishLaunch(ctx context.Context, item *dispatch.QueueItem, run *runs.Run, outcome string, err error) bool {
	if errors.Is(err, runs.ErrRunActive) {
		c.waitRunActive(ctx, item)
		return false
	}
	if errors.Is(err, runs.ErrPaused) {
		if derr := c.queue.Wait(ctx, item.ID, c.now().Add(pauseRecheck), dispatch.DeferPaused); derr != nil {
			c.log.Warn("queue defer for pause", "queue_id", item.ID, "error", derr)
		}
		return false
	}
	if err != nil || run == nil {
		c.log.Warn("queue launch", "job", item.JobName, "queue_id", item.ID, "error", err)
		c.failOrRetry(ctx, item, err, run)
		return false
	}
	if run.Status == runs.StatusFailed && (run.EndReason == runs.EndAuth || run.EndReason == runs.EndLimit) {
		reason := dispatch.BlockAuthMissing
		if run.EndReason == runs.EndLimit {
			reason = dispatch.BlockLimitExceeded
		}
		_ = c.queue.MarkBlocked(ctx, item.ID, reason)
		return false
	}
	result := run.QueueResult(outcome)
	if result == dispatch.ResultLaunched {
		c.recordPolicy(ctx, item)
	}
	if merr := c.queue.MarkDone(ctx, item.ID, result, run.ID); merr != nil {
		c.log.Warn("queue mark done", "queue_id", item.ID, "error", merr)
	} else {
		c.ackChannel(ctx, item, result)
	}
	return true
}

// launchDeferred launches a manual or scheduled request that waited for a
// live run. The launch options come from the current job file; the request
// adds the reason, prompt, wake, inputs, and conversation. The trigger limit
// does not apply.
func (c *ControlPlane) launchDeferred(ctx context.Context, item *dispatch.QueueItem) {
	lr := item.Launch
	j, path := c.findJobKind(ctx, item.WorkspaceID, item.JobID, cmp.Or(item.Kind, dispatch.KindJob))
	if j == nil || outsideAgent(j) {
		_ = c.queue.MarkBlocked(ctx, item.ID, dispatch.BlockJobMissing)
		return
	}
	kit := c.Workspaces.OwnerKit(ctx, item.WorkspaceID, item.JobName)
	harness := cmp.Or(lr.Harness, j.ResolvedHarness(kit))
	manual := lr.Reason == string(runs.ReasonManual) || lr.Reason == string(runs.ReasonReplay)
	if c.heldItem(ctx, item, cmp.Or(item.Kind, dispatch.KindJob), harness, manual) {
		return
	}
	if c.waitHarnessUpdate(ctx, item, harness) {
		return
	}
	if reason := c.blockedFor(ctx, item.Kind, harness); reason != "" {
		_ = c.queue.MarkBlocked(ctx, item.ID, reason)
		return
	}
	if !c.markAttempt(ctx, item) {
		return
	}
	d := j.LaunchDefaultsWithKit(kit)
	cwd := cmp.Or(d.WorkingDir, dirOf(path))
	var wake *runs.WakeOpts
	if lr.Wake != nil {
		wake = &runs.WakeOpts{Body: lr.Wake.Body, ExtraPrompt: lr.Wake.ExtraPrompt, Message: lr.Wake.Message}
		if lr.ConversationID != "" && c.Conversations != nil {
			wake.Recap = c.Conversations.Recap(ctx, lr.ConversationID, wakeRecapN)
		}
	}
	runtime := cmp.Or(lr.Runtime, j.Runtime)
	var trigger *runs.TriggerCapture
	if ev := lr.Event; ev != nil {
		trigger = &runs.TriggerCapture{EventID: ev.ID, Connector: ev.Connector, OnIndex: &ev.OnIndex,
			Vars: ev.Vars, Source: runs.TriggerManual}
	}
	run, outcome, err := c.Runs.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID:     item.WorkspaceID,
			JobID:           item.JobID,
			JobName:         item.JobName,
			Kind:            item.Kind,
			Reason:          runs.RunReason(lr.Reason),
			Harness:         harness,
			Model:           d.Model,
			PromptPreview:   trunc(lr.Prompt, 200),
			Inputs:          lr.Inputs,
			Trigger:         trigger,
			QueueID:         item.ID,
			ConversationKey: item.ConversationKey,
			ConversationID:  lr.ConversationID,
			TraceID:         item.TraceID,
			MaxDurationMS:   d.MaxDurationMS,
			IdleTimeoutMS:   d.IdleTimeoutMS,
			KeepAliveMS:     d.KeepAliveMS,
			NudgeMS:         d.NudgeMS,
			WorkingDir:      cwd,
			Runtime:         runtime,
			FolderEnv:       d.FolderEnv,
		},
		Prompt:    lr.Prompt,
		Wake:      wake,
		Memory:    c.memoryLaunch(ctx, item.WorkspaceID, j),
		StateDecl: j.State,
		Kit:       kit,
		JobSkills: j.Skills, JobConnectors: j.Connectors,
		IfRunning: d.IfRunning,
		Env:       d.Env,
		Effort:    d.Effort,
	})
	c.finishLaunch(ctx, item, run, outcome, err)
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

// waitRunActive puts the row back until the live run of its job ends.
func (c *ControlPlane) waitRunActive(ctx context.Context, item *dispatch.QueueItem) {
	if err := c.queue.Wait(ctx, item.ID, c.now().Add(2*time.Second), dispatch.DeferRunActive); err != nil {
		c.log.Warn("queue wait", "queue_id", item.ID, "error", err)
	}
}

func (c *ControlPlane) dispatchResume(ctx context.Context, item *dispatch.QueueItem) {
	// A live turn of the run makes the resume wait; the wait is not an attempt.
	if prev, err := c.Runs.Get(ctx, item.ResumeOf); err == nil && prev.Status == runs.StatusRunning {
		c.waitRunActive(ctx, item)
		return
	}
	if !c.markAttempt(ctx, item) {
		return
	}
	run, err := c.Launcher.Resume(ctx, item.ResumeOf, runs.ResumeOpts{Reason: runs.ReasonResume, QueueID: item.ID})
	if errors.Is(err, runs.ErrRunActive) {
		c.waitRunActive(ctx, item)
		return
	}
	if err != nil || run == nil {
		c.log.Warn("queue resume", "job", item.JobName, "queue_id", item.ID, "error", err)
		c.failOrRetry(ctx, item, err, run)
		return
	}
	if merr := c.queue.MarkDone(ctx, item.ID, dispatch.ResultLaunched, run.ID); merr != nil {
		c.log.Warn("queue mark done", "queue_id", item.ID, "error", merr)
	}
}

func runsReasonFor(item *dispatch.QueueItem, frame *chainFrame) runs.RunReason {
	if frame != nil {
		return frame.Reason
	}
	if item.Connector == "escalation" {
		return runs.ReasonEscalation
	}
	return runs.ReasonTrigger
}

// chainFrame is the launch frame of a chain hop.
type chainFrame struct{ *chains.LaunchFrame }

func (f *chainFrame) path() []string {
	if f == nil {
		return nil
	}
	return f.Path
}

// chainFrame reads the frame of a chain row. A row whose hop closed (a
// stopped chain) is skipped, and ok is false.
func (c *ControlPlane) chainFrame(ctx context.Context, item *dispatch.QueueItem) (*chainFrame, bool) {
	if item.HopID == "" || c.Chains == nil {
		return nil, true
	}
	lf, err := c.Chains.LaunchFrame(ctx, item.HopID)
	if errors.Is(err, chains.ErrHopClosed) {
		if merr := c.queue.MarkDone(ctx, item.ID, dispatch.ResultSkipped, ""); merr != nil {
			c.log.Warn("queue mark skipped", "queue_id", item.ID, "error", merr)
		}
		return nil, false
	}
	if err != nil {
		c.log.Warn("chain frame", "hop_id", item.HopID, "error", err)
		return nil, true
	}
	return &chainFrame{lf}, true
}

// bindHop links a chain hop to the run that took it, or fails the hop when
// the launch will not happen.
func (c *ControlPlane) bindHop(ctx context.Context, item *dispatch.QueueItem, run *runs.Run, launchErr error, done bool) {
	if item.HopID == "" || c.Chains == nil {
		return
	}
	if done && run != nil && run.Status != runs.StatusSkipped {
		if err := c.Chains.Bind(ctx, item.HopID, run.ID, run.TurnCount); err != nil {
			c.log.Warn("chain bind", "hop_id", item.HopID, "error", err)
		}
		return
	}
	final := done || errors.Is(launchErr, runs.ErrBudgetExceeded) || errors.Is(launchErr, runs.ErrWorkspaceArchived) ||
		errors.Is(launchErr, runs.ErrActionsInvalid) ||
		errors.Is(launchErr, runs.ErrActionsUnsupported) || item.Attempts >= dispatch.MaxAttempts
	if final && !errors.Is(launchErr, runs.ErrRunActive) {
		if launchErr == nil {
			launchErr = errors.New("launch skipped")
		}
		if err := c.Chains.LaunchFailed(ctx, item.HopID, launchErr); err != nil {
			c.log.Warn("chain launch failed", "hop_id", item.HopID, "error", err)
		}
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
	// An action error is a config fault: a retry sends the same launch.
	if errors.Is(err, runs.ErrActionsInvalid) || errors.Is(err, runs.ErrActionsUnsupported) {
		if merr := c.queue.MarkDead(ctx, item.ID, err.Error()); merr != nil {
			c.log.Warn("queue mark dead", "queue_id", item.ID, "error", merr)
		}
		return
	}
	if err != nil && item.Attempts < dispatch.MaxAttempts {
		if errors.Is(err, runs.ErrBudgetExceeded) || errors.Is(err, runs.ErrWorkspaceArchived) {
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

// channelConversation resolves the conversation of an item that came from
// a connector channel (plan G.2): an agent item, or a job item whose trigger
// replies in the chat. The chat of a reply job belongs to its owner agent.
// It returns the conversation id and key, and the channel address of the
// event for the reply; an empty id means the item is not a channel item.
func (c *ControlPlane) channelConversation(ctx context.Context, item *dispatch.QueueItem, j *job.Job) (convID, key string, addr conversations.Address) {
	kind := cmp.Or(item.Kind, runs.KindJob)
	if (kind != runs.KindAgent && kind != runs.KindJob) || item.Connector == "" || item.ConversationKey == "" {
		return "", "", addr
	}
	if c.Connectors == nil || c.Conversations == nil {
		return "", "", addr
	}
	if !c.Connectors.IsChannel(item.Connector) {
		return "", "", addr
	}
	addr = c.eventAddress(ctx, item)
	operator, name := item.JobID, item.JobName
	if kind == runs.KindJob {
		owner, ok := c.replyOwner(ctx, item, j, addr.Room)
		if !ok {
			return "", "", addr
		}
		operator, name = owner.OwnerID, owner.OwnerIndex()
	}
	pointer := addr
	pointer.Room = cmp.Or(addr.Room, item.ConversationKey)
	convID, err := c.Conversations.EnsureForChannel(ctx, item.WorkspaceID, operator, pointer)
	if err != nil || convID == "" {
		c.noteOperatorConflict(ctx, err, item, pointer, name)
		return "", "", addr
	}
	conv, err := c.Conversations.Get(ctx, convID)
	if err != nil {
		return convID, "", addr
	}
	return conv.ID, conv.Key, addr
}

// replyOwner returns the index row of a job item whose trigger replies in
// the chat of room, when the room claim is this job. A losing duplicate
// claim stays listen-only.
func (c *ControlPlane) replyOwner(ctx context.Context, item *dispatch.QueueItem, j *job.Job, room string) (workspaces.JobRow, bool) {
	if j == nil || room == "" {
		return workspaces.JobRow{}, false
	}
	// OnIndex counts each trigger family on its own.
	replies := false
	for _, tr := range j.Triggers {
		if tr.Family == "event" && tr.Index == item.OnIndex {
			replies = tr.RepliesInChat()
			break
		}
	}
	if !replies {
		return workspaces.JobRow{}, false
	}
	row, ok := c.Connectors.ReplyJob(ctx, item.WorkspaceID, c.Connectors.SlugOf(item.Connector), room)
	if !ok || row.ID != item.JobID || row.OwnerID == "" {
		return workspaces.JobRow{}, false
	}
	return row, true
}

// HandlerJob returns the job that replies in the chat of conv. The room
// claim is the source, so a job that stops replying stops getting messages.
func (c *ControlPlane) HandlerJob(ctx context.Context, conv *conversations.Conversation) (jobID, jobName string, ok bool) {
	if c.Connectors == nil || c.Conversations == nil || conv == nil || conv.OperatorID == "" {
		return "", "", false
	}
	p, err := c.Conversations.PointerFor(ctx, conv.Workspace, conv.ID)
	if err != nil {
		return "", "", false
	}
	row, found := c.Connectors.ReplyJob(ctx, conv.Workspace, c.Connectors.SlugOf(p.Instance), p.Room)
	if !found || row.OwnerID != conv.OperatorID {
		return "", "", false
	}
	return row.ID, row.Name, true
}

// chatSection tells the run of a reply job where it talks and as whom. app
// marks a message that a person typed in the app.
func (c *ControlPlane) chatSection(ctx context.Context, item *dispatch.QueueItem, convID string, app bool) string {
	conv, err := c.Conversations.Get(ctx, convID)
	if err != nil {
		return ""
	}
	agent := conv.OperatorName
	desc := ""
	if owner, _ := c.findJobKind(ctx, item.WorkspaceID, conv.OperatorID, runs.KindAgent); owner != nil {
		agent = cmp.Or(owner.Title, agent)
		desc = strings.TrimSpace(owner.Description)
	}
	instance, room := item.Connector, item.Vars["room"]
	if p, err := c.Conversations.PointerFor(ctx, conv.Workspace, conv.ID); err == nil {
		instance, room = p.Instance, cmp.Or(room, p.Room)
	}
	roomName := cmp.Or(item.Vars["room_name"], conv.Title, room)
	connector := c.Connectors.Title(instance)
	lines := []string{"## Chat"}
	if app {
		lines = append(lines, fmt.Sprintf("A person writes to you in the app about the %s chat '%s'. You answer as %s. Your reply shows in the app.", connector, roomName, agent))
	} else {
		lines = append(lines, fmt.Sprintf("You reply in the %s chat '%s' as %s. Your text reply goes to the chat. Do not send it with a tool.", connector, roomName, agent))
	}
	if desc != "" {
		lines = append(lines, agent+": "+desc)
	}
	lines = append(lines, runs.ChatStyleLine)
	if recap := c.Conversations.Recap(ctx, convID, wakeRecapN); recap != "" {
		lines = append(lines, "Recent messages:\n"+recap)
	}
	return strings.Join(lines, "\n")
}

// eventAddress reads the channel address of the event behind a queue item.
// The conversation key is only the lane key; the reply goes to this address.
// An item with no event row has an address with no room.
func (c *ControlPlane) eventAddress(ctx context.Context, item *dispatch.QueueItem) conversations.Address {
	// item.Connector holds the connection id; the event row has the type.
	addr := conversations.Address{Connector: item.Connector, Instance: item.Connector}
	if item.EventID == "" {
		return addr
	}
	// A missing event row means no channel address.
	_ = c.Store.DB().QueryRowContext(ctx,
		`SELECT connector, instance, COALESCE(room,''), COALESCE(thread,'') FROM events WHERE id=?`, item.EventID).
		Scan(&addr.Connector, &addr.Instance, &addr.Room, &addr.Thread)
	return addr
}

// noteOperatorConflict logs a channel that already belongs to another
// operator. The doctor reports the trigger setup that causes it.
func (c *ControlPlane) noteOperatorConflict(ctx context.Context, err error, item *dispatch.QueueItem, addr conversations.Address, operator string) {
	if !errors.Is(err, conversations.ErrOperatorConflict) {
		return
	}
	current := ""
	if p, perr := c.Conversations.GetPointer(ctx, item.WorkspaceID, addr); perr == nil {
		if conv, gerr := c.Conversations.Get(ctx, p.ConversationID); gerr == nil {
			current = conv.OperatorName
		}
	}
	c.log.WarnContext(ctx, "channel operator conflict", "instance", addr.Instance, "room", addr.Room,
		"thread", addr.Thread, "operator", operator, "current_operator", current, "queue_id", item.ID)
}

// noteChannelUser stores the inbound event text as the channel-typed user
// turn of an agent item. The row carries the channel address of the event,
// so the reply path can send the answer back (plan G.3). An address with no
// room gets no reply. The event id is the idempotency key, so a retry writes
// nothing.
func (c *ControlPlane) noteChannelUser(ctx context.Context, item *dispatch.QueueItem, convID string, addr conversations.Address, text string) {
	if c.Conversations == nil || convID == "" {
		return
	}
	if strings.TrimSpace(text) == "" {
		text = "Event trigger " + item.Connector + " fired."
	}
	if _, _, err := c.Conversations.AppendUser(ctx, convID, conversations.UserTurn{
		Text:           text,
		Address:        addr,
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

// waitHarnessUpdate defers item while the visor updates its harness.
func (c *ControlPlane) waitHarnessUpdate(ctx context.Context, item *dispatch.QueueItem, harness string) bool {
	if c.Harness == nil || !c.Harness.Updating(ctx, harness) {
		return false
	}
	if err := c.queue.Defer(ctx, item.ID, c.now().Add(10*time.Second), dispatch.DeferHarnessUpdate); err != nil {
		c.log.Warn("queue defer for harness update", "queue_id", item.ID, "error", err)
	}
	return true
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

// The texts of a follow-up (plan A.11, A.16).
const (
	followUpTemplate = "New message in this conversation from {{sender}}:\n\n{{text}}"
	reactionTemplate = "{{sender}} reacted {{text}} to message {{reply_to}}"
	lookupLine       = "Earlier messages: connection {{connection}}, room {{room}}, thread {{thread}}. Read more with the history command of the connector."
	recapN           = 20
	recapCut         = 500
)

// followUp builds the texts of a follow-up item. A resumed session gets the
// message and the lookup line. A fresh session also gets the job prompt with
// the vars of the first run, and a recap of the stored messages.
func (c *ControlPlane) followUp(ctx context.Context, item *dispatch.QueueItem, j *job.Job) *runs.FollowUp {
	msg := c.messageText(ctx, item)
	tmpl := lookupLine
	if item.Vars["thread"] == "" {
		tmpl = strings.Replace(tmpl, ", thread {{thread}}", "", 1)
	}
	lookup := fillTemplate(tmpl, item.Vars)
	first, err := c.Runs.FirstTriggerVars(ctx, item.WorkspaceID, item.JobID, item.ConversationKey)
	if err != nil {
		c.log.Warn("follow-up first vars", "queue_id", item.ID, "error", err)
	}
	if first == nil {
		first = item.Vars
	}
	parts := []string{expandVars(j.Prompt, first)}
	if recap := c.recap(ctx, item); recap != "" {
		parts = append(parts, recap)
	}
	parts = append(parts, lookup, msg)
	return &runs.FollowUp{Text: msg + "\n\n" + lookup, FreshPrompt: strings.Join(parts, "\n\n")}
}

// messageText is the text of the channel event of item for a session that
// already has the job prompt. A reaction quotes the message it reacts to.
func (c *ControlPlane) messageText(ctx context.Context, item *dispatch.QueueItem) string {
	var typ, instance, raw string
	_ = c.Store.DB().QueryRowContext(ctx, `SELECT COALESCE(event_type,''), instance, COALESCE(raw_json,'') FROM events WHERE id=?`,
		item.EventID).Scan(&typ, &instance, &raw) // a missing row is a plain message
	if typ != "reaction" {
		return fillTemplate(followUpTemplate, item.Vars)
	}
	target := item.Vars["reply_to"]
	if target == "" {
		return fillTemplate("{{sender}} reacted {{text}}", item.Vars)
	}
	msg := fillTemplate(reactionTemplate, item.Vars)
	var ev struct {
		Data struct {
			MessageText string `json:"message_text"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(raw), &ev) // a bad payload has no quote
	quote := strings.TrimSpace(ev.Data.MessageText)
	if quote == "" {
		// The adapter did not send the text; a stored message has it.
		_ = c.Store.DB().QueryRowContext(ctx, `SELECT COALESCE(text,'') FROM events WHERE instance=? AND source_id=?`,
			instance, target).Scan(&quote)
		quote = strings.TrimSpace(quote)
	}
	if quote == "" {
		return msg
	}
	if r := []rune(quote); len(r) > recapCut {
		quote = string(r[:recapCut]) + "…"
	}
	return msg + ":\n\n> " + strings.ReplaceAll(quote, "\n", "\n> ")
}

// recap formats the last stored messages of the conversation of item, not
// the item event itself. Lines from the connected account read "you".
func (c *ControlPlane) recap(ctx context.Context, item *dispatch.QueueItem) string {
	if c.Connectors == nil || item.EventID == "" {
		return ""
	}
	var instance, root string
	if err := c.Store.DB().QueryRowContext(ctx, `SELECT instance, COALESCE(root,'') FROM events WHERE id=?`, item.EventID).
		Scan(&instance, &root); err != nil || root == "" {
		return ""
	}
	lines, err := c.Connectors.Recap(ctx, instance, root, recapN+1)
	if err != nil {
		c.log.Warn("follow-up recap", "queue_id", item.ID, "error", err)
		return ""
	}
	var b strings.Builder
	n := 0
	for _, l := range lines {
		if l.EventID == item.EventID || n == recapN {
			continue
		}
		n++
		who := l.Sender
		if l.Self {
			who = "you"
		}
		text := l.Text
		if r := []rune(text); len(r) > recapCut {
			text = string(r[:recapCut]) + "…"
		}
		fmt.Fprintf(&b, "\n%s: %s", who, text)
	}
	if n == 0 {
		return ""
	}
	return "Messages in this conversation so far:" + b.String()
}

// batchRun is the launch input of a batch row: its events and the run id
// that the path of their events.json names.
type batchRun struct {
	runID string
	batch dispatch.Batch
	vars  map[string]string
	data  []byte
}

// loadBatch reads the events of a batch row. A row with no events is done
// with nothing to launch. A read error retries the row later. Both give nil.
func (c *ControlPlane) loadBatch(ctx context.Context, item *dispatch.QueueItem) *batchRun {
	events, err := c.queue.BatchEvents(ctx, item.ID)
	if err == nil && len(events) == 0 {
		if merr := c.queue.MarkDone(ctx, item.ID, dispatch.ResultSkipped, ""); merr != nil {
			c.log.Warn("queue mark skipped", "queue_id", item.ID, "error", merr)
		}
		return nil
	}
	b := &batchRun{runID: ulid.Make().String()}
	if err == nil {
		b.batch = dispatch.Batch{Events: events, File: filepath.Join(home.RunDir(b.runID), batchFile)}
		b.data, err = b.batch.JSON()
	}
	if err != nil {
		c.log.Warn("batch events", "queue_id", item.ID, "error", err)
		if derr := c.queue.Defer(ctx, item.ID, c.now().Add(15*time.Second), dispatch.DeferRetry); derr != nil {
			c.log.Warn("queue defer", "queue_id", item.ID, "error", derr)
		}
		return nil
	}
	b.vars = b.batch.Vars()
	return b
}

// batchFile is the name of the events file of a batch run.
const batchFile = "events.json"

// prompt is the prompt of a job batch run: the job prompt with the batch
// vars, and the events block at the end when the prompt has no {{events}}.
// An operator gets the events block as its wake message.
func (b *batchRun) prompt(tmpl, kind string) string {
	if kind != runs.KindJob {
		return dispatch.BatchBlock(b.vars)
	}
	p := strings.TrimSpace(fillTemplate(tmpl, b.vars))
	if strings.Contains(tmpl, "{{events}}") {
		return p
	}
	return strings.TrimSpace(p + "\n\n" + dispatch.BatchBlock(b.vars))
}

func (b *batchRun) trigger(item *dispatch.QueueItem) *runs.TriggerCapture {
	return &runs.TriggerCapture{Connector: item.Connector, Vars: b.vars, Batch: &runs.BatchCapture{
		Count: len(b.batch.Events), EventIDs: b.batch.EventIDs(), QueueID: item.ID,
	}}
}

func (b *batchRun) files() map[string][]byte { return map[string][]byte{batchFile: b.data} }

// fillTemplate replaces each {{var}} of a fixed template in one pass, so a
// value that holds {{x}} stays as it is.
func fillTemplate(tmpl string, vars map[string]string) string {
	pairs := make([]string, 0, 2*len(vars))
	for k, v := range vars {
		pairs = append(pairs, "{{"+k+"}}", v)
	}
	return strings.NewReplacer(pairs...).Replace(tmpl)
}

// queuePrompt picks the item prompt (job prompt as fallback) and expands vars.
func queuePrompt(item *dispatch.QueueItem, j *job.Job) string {
	prompt := item.Prompt
	if prompt == "" {
		prompt = j.Prompt
	}
	return expandVars(prompt, item.PromptVars())
}

// expandVars applies {{var}} substitutions from the trigger extract.
func expandVars(prompt string, vars map[string]string) string {
	for k, v := range vars {
		prompt = replaceVar(prompt, k, v)
	}
	return prompt
}

// policyDefer applies the trigger policy window (plan E.2): a blocked row
// stays pending with the first allowed attempt time.
func (c *ControlPlane) policyDefer(ctx context.Context, item *dispatch.QueueItem) bool {
	if c.Policy == nil {
		return false
	}
	rate, key := c.itemPolicy(ctx, item)
	if key == "" {
		return false
	}
	until := c.Policy.NextAllowed(key, rate, c.now())
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
	convID, _, err := c.assistantConversation(ctx, item)
	if err != nil || convID == "" {
		return false
	}
	return c.asst.TurnActive(ctx, convID)
}

// assistantConversation resolves the conversation of one assistant item:
// the channel pointer of the event's channel address. An event with no room
// points by the conversation key and gets no reply room. An item with
// neither shares one trigger conversation per channel; its address keeps an
// empty room, so it gets no reply either.
func (c *ControlPlane) assistantConversation(ctx context.Context, item *dispatch.QueueItem) (convID string, addr conversations.Address, err error) {
	name := cmp.Or(item.Connector, conversations.ChannelAssistant)
	addr = c.eventAddress(ctx, item)
	addr.Connector, addr.Instance = name, cmp.Or(addr.Instance, name)
	pointer := addr
	switch {
	case addr.Room != "":
	case item.ConversationKey != "":
		pointer.Room = item.ConversationKey
	default:
		pointer.Room = "trigger:" + name
	}
	convID, err = c.Conversations.EnsureForChannel(ctx, item.WorkspaceID, "", pointer)
	if err != nil {
		c.noteOperatorConflict(ctx, err, item, pointer, "")
	}
	return convID, addr, err
}

// recordPolicy notes one fire for the trigger's rate limit.
func (c *ControlPlane) recordPolicy(ctx context.Context, item *dispatch.QueueItem) {
	if c.Policy == nil {
		return
	}
	rate, key := c.itemPolicy(ctx, item)
	if key != "" {
		c.Policy.Record(key, rate, c.now())
	}
}

// itemPolicy returns the rate limit and claim key of the trigger behind one
// queue item. The key matches TriggerRows: workspace, job, trigger index,
// conversation key. A resume item, a waiting launch, or an item whose
// connector is not the event of the trigger at OnIndex (escalation,
// conversation wake), has no policy.
func (c *ControlPlane) itemPolicy(ctx context.Context, item *dispatch.QueueItem) (*dispatch.RateLimit, string) {
	if item.ResumeOf != "" || item.Launch != nil {
		return nil, ""
	}
	key := item.WorkspaceID + "/" + item.JobID + "/" + strconv.Itoa(item.OnIndex) + "/" + item.ConversationKey
	// Triggers name the slug; the item holds the connection id.
	slug := item.Connector
	if c.Connectors != nil {
		slug = cmp.Or(c.Connectors.SlugOf(item.Connector), item.Connector)
	}
	if item.Kind == dispatch.KindAssistant {
		if rate := c.assistantLimit(ctx, item, slug); rate != nil {
			return rate, key
		}
		return nil, ""
	}
	j, _ := c.findJobKind(ctx, item.WorkspaceID, item.JobID, item.Kind)
	if j == nil {
		return nil, ""
	}
	for _, tr := range j.Triggers {
		if tr.Index == item.OnIndex && tr.Family == "event" && tr.Event == slug {
			return tr.RateLimit(), key
		}
	}
	return nil, ""
}

// assistantLimit is the limit of the assistant trigger of item.
func (c *ControlPlane) assistantLimit(ctx context.Context, item *dispatch.QueueItem, slug string) *dispatch.RateLimit {
	rows, err := c.Workspaces.AssistantTriggerRows(ctx)
	if err != nil {
		return nil
	}
	for _, at := range rows {
		if at.WorkspaceID != item.WorkspaceID {
			continue
		}
		for _, tr := range at.Triggers {
			// The pipeline takes an assistant trigger with no family.
			if tr.Index == item.OnIndex && (tr.Family == "" || tr.Family == "event") && tr.Event == slug {
				return tr.RateLimit()
			}
		}
	}
	return nil
}

// pruneEmailContent clears email text older than email.content_days. It
// runs with the channel off too, so a lower setting still takes effect.
func (c *ControlPlane) pruneEmailContent(ctx context.Context, now time.Time) {
	if c.Email == nil {
		return
	}
	cfg, err := config.Load()
	if err != nil {
		c.log.Warn("email content prune", "error", err)
		return
	}
	if _, err := c.Email.PruneContent(ctx, now, cfg.Email.ContentRetention()); err != nil {
		c.log.Warn("email content prune", "error", err)
	}
}
