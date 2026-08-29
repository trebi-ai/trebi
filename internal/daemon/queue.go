package daemon

import (
	"context"
	"database/sql"
	"os"
	"time"

	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
)

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
	c.releaseAuthBlocks(ctx)
	for _, l := range c.queueLanes(ctx) {
		c.dispatchLane(ctx, l)
	}
}

// releaseAuthBlocks requeues items blocked on auth_missing once the harness
// stamp is ok again — recovery must drain without a manual requeue.
func (c *ControlPlane) releaseAuthBlocks(ctx context.Context) {
	rows, err := c.Store.DB().QueryContext(ctx, `
		SELECT id, workspace_id, job FROM event_queue
		WHERE status='blocked' AND block_reason=?`, string(dispatch.BlockAuthMissing))
	if err != nil {
		c.log.Warn("list auth-blocked queue", "error", err)
		return
	}
	defer rows.Close()
	type blocked struct{ id, ws, job string }
	var list []blocked
	for rows.Next() {
		var r blocked
		if rows.Scan(&r.id, &r.ws, &r.job) == nil {
			list = append(list, r)
		}
	}
	if err := rows.Err(); err != nil {
		c.log.Warn("list auth-blocked queue", "error", err)
		return
	}
	for _, r := range list {
		j, _ := c.findJob(ctx, r.ws, r.job)
		name := "claude"
		if j != nil && j.Harness != "" {
			name = j.Harness
		}
		if c.harnessBlocked(ctx, name) == "" {
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
	j, path := c.findJob(ctx, l.ws, l.job)
	if j == nil {
		_ = c.queue.MarkBlocked(ctx, item.ID, dispatch.BlockJobMissing)
		return
	}
	prompt := queuePrompt(item, j)
	harness := j.Harness
	if harness == "" {
		harness = "claude"
	}
	// Readiness gate: auth / limit (harness_state)
	if reason := c.harnessBlocked(ctx, harness); reason != "" {
		_ = c.queue.MarkBlocked(ctx, item.ID, reason)
		return
	}
	ifRunning := j.Session.IfRunning
	if ifRunning == "" {
		ifRunning = "skip"
	}
	maxMS, idleMS, keepMS, nudgeMS := j.Session.KnobsMS()
	cwd := j.WorkingDir
	if cwd == "" {
		cwd = dirOf(path)
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
			ConversationID:  c.conversationID(ctx, item.WorkspaceID, item.ConversationKey),
			TraceID:         item.TraceID,
			MaxDurationMS:   maxMS,
			IdleTimeoutMS:   idleMS,
			KeepAliveMS:     keepMS,
			NudgeMS:         nudgeMS,
		},
		Prompt:     prompt,
		WorkingDir: cwd,
		IfRunning:  ifRunning,
	})
	if err != nil || run == nil {
		c.log.Warn("queue launch", "job", item.Job, "queue_id", item.ID, "error", err)
		if merr := c.queue.MarkDone(ctx, item.ID, dispatch.ResultDead, ""); merr != nil {
			c.log.Warn("queue mark dead", "queue_id", item.ID, "error", merr)
		}
		return
	}
	if merr := c.queue.MarkDone(ctx, item.ID, queueResult(outcome, run), run.ID); merr != nil {
		c.log.Warn("queue mark done", "queue_id", item.ID, "error", merr)
	}
	if item.Kind == runs.KindAssistant {
		c.log.Info("assistant.woken", "reason", string(runs.ReasonTrigger), "channel", "")
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
	for k, v := range item.Vars {
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
