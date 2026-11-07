package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/artifacts"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/schedule"
)

// FireDue claims due schedule rows and launches runs (exported for tests).
func (c *ControlPlane) FireDue(ctx context.Context) {
	due, err := c.Schedule.ClaimDue(ctx, time.Now().UTC(), 20)
	if err != nil {
		c.log.Warn("schedule claim", "error", err)
		return
	}
	for _, r := range due {
		c.fireOne(ctx, r)
	}
}

func (c *ControlPlane) scheduleTicker(ctx context.Context) {
	iv := c.Opts.ScheduleInterval
	if iv <= 0 {
		iv = 15 * time.Second
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
			c.FireDue(ctx)
		}
	}
}

// fireOne launches one claimed schedule entry. ClaimDue already advanced next_due_at.
// The lookup carries no kind filter: assistant and agent rows fire with their own
// kind instead of being orphan-deleted. Auth/limit gates create a failed run so
// the attempt is visible; a false when: gate still skips without a run.
func (c *ControlPlane) fireOne(ctx context.Context, r schedule.Row) {
	if r.ArtifactID != "" {
		c.fireArtifact(ctx, r)
		return
	}
	j, path := c.findJob(ctx, r.WorkspaceID, r.JobID)
	if j == nil {
		c.log.Info("schedule orphan deleted", "workspace", r.WorkspaceID, "job", r.JobName, "idx", r.Idx)
		if err := c.Schedule.Delete(ctx, r.WorkspaceID, r.JobID, r.Idx); err != nil {
			c.log.Warn("schedule orphan delete", "job", r.JobName, "error", err)
		}
		return
	}
	if j.Disabled {
		return
	}
	if outsideAgent(j) {
		c.log.Warn("schedule skipped: the job is not in an agent", "workspace", r.WorkspaceID, "job", r.JobName)
		return
	}
	kit := c.Workspaces.OwnerKit(ctx, r.WorkspaceID, r.JobName)
	if c.heldFire(ctx, r, j.Kind, j.ResolvedHarness(kit)) {
		return
	}
	if r.Idx == -1 {
		since := time.Now().UTC().AddDate(0, 0, -7)
		if r.LastFiredAt != "" {
			if parsed, err := time.Parse(time.RFC3339, r.LastFiredAt); err == nil {
				since = parsed
			}
		}
		block, err := c.Workspaces.ReportBlock(ctx, r.WorkspaceID, r.JobName, since)
		if err != nil {
			c.log.Warn("report block", "job", r.JobName, "error", err)
			return
		}
		_, _, gateSkip := c.evalWhenGate(ctx, j, r, path)
		if gateSkip {
			return
		}
		convID, convKey := "", ""
		if c.Conversations != nil {
			if id, err := c.Conversations.EnsureForChannel(ctx, r.WorkspaceID, r.JobID,
				conversations.Address{Connector: conversations.ChannelEmail, Room: "report:" + r.JobID}); err == nil {
				if conv, err := c.Conversations.Get(ctx, id); err == nil {
					convID, convKey = conv.ID, conv.Key
				}
			}
		}
		d := j.LaunchDefaultsWithKit(kit)
		cwd := d.WorkingDir
		if cwd == "" {
			cwd = dirOf(path)
		}
		lopts := runs.LaunchOpts{
			CreateOpts: runs.CreateOpts{
				WorkspaceID: r.WorkspaceID, JobID: r.JobID, JobName: r.JobName, Kind: j.Kind,
				Reason: runs.ReasonReport, Harness: d.Harness, Model: d.Model,
				ConversationID: convID, ConversationKey: convKey,
				MaxDurationMS: d.MaxDurationMS, IdleTimeoutMS: d.IdleTimeoutMS,
				KeepAliveMS: d.KeepAliveMS, NudgeMS: d.NudgeMS,
				WorkingDir: cwd, Runtime: j.Runtime, FolderEnv: d.FolderEnv,
			},
			Wake: &runs.WakeOpts{Message: block}, Memory: c.memoryLaunch(ctx, r.WorkspaceID, j), StateDecl: j.State,
			Kit: kit, JobSkills: j.Skills, JobConnectors: j.Connectors,
			IfRunning: d.IfRunning, Env: d.Env, Effort: d.Effort,
		}
		_, _, err = c.Runs.CreateAndLaunch(ctx, lopts)
		if errors.Is(err, runs.ErrRunActive) {
			c.queueFire(ctx, &lopts)
		} else if err != nil {
			c.log.Warn("report fire", "job", r.JobName, "error", err)
			c.raiseActionsFault(ctx, r, err)
		}
		c.markFired(ctx, r)
		return
	}
	prompt, inputs, gateSkip := c.evalWhenGate(ctx, j, r, path)
	if gateSkip {
		return
	}
	d := j.LaunchDefaultsWithKit(kit)
	harness := d.Harness
	cwd := d.WorkingDir
	if cwd == "" {
		cwd = dirOf(path)
	}
	// Operators (assistant, agent) wake through the composer; the gate has
	// already var-expanded the body (Part F.3).
	var wake *runs.WakeOpts
	if j.Kind != runs.KindJob {
		wake = &runs.WakeOpts{Body: prompt}
		prompt = ""
	}
	lopts := runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID:   r.WorkspaceID,
			JobID:         r.JobID,
			JobName:       r.JobName,
			Kind:          j.Kind,
			Reason:        runs.ReasonSchedule,
			Harness:       harness,
			Model:         d.Model,
			PromptPreview: trunc(prompt, 200),
			Inputs:        inputs,
			MaxDurationMS: d.MaxDurationMS,
			IdleTimeoutMS: d.IdleTimeoutMS,
			KeepAliveMS:   d.KeepAliveMS,
			NudgeMS:       d.NudgeMS,
			WorkingDir:    cwd,
			Runtime:       j.Runtime,
			FolderEnv:     d.FolderEnv,
		},
		Prompt:    prompt,
		Wake:      wake,
		Memory:    c.memoryLaunch(ctx, r.WorkspaceID, j),
		StateDecl: j.State,
		Kit:       kit,
		JobSkills: j.Skills, JobConnectors: j.Connectors,
		IfRunning: d.IfRunning,
		Env:       d.Env,
		Effort:    d.Effort,
	}
	if reason := c.harnessBlocked(ctx, harness); reason != "" {
		end := runs.EndAuth
		if reason == dispatch.BlockLimitExceeded {
			end = runs.EndLimit
		}
		c.log.Info("schedule skip harness", "job", r.JobName, "harness", harness, "reason", reason)
		if _, err := c.Runs.FailBeforeLaunch(ctx, lopts, end, map[string]string{
			"harness": harness,
			"reason":  string(reason),
			"message": "schedule: harness " + harness + " blocked (" + string(reason) + ")",
		}); err != nil {
			c.log.Warn("schedule fail before launch", "job", r.JobName, "error", err)
		}
		c.markFired(ctx, r)
		return
	}
	run, _, err := c.Runs.CreateAndLaunch(ctx, lopts)
	if errors.Is(err, runs.ErrRunActive) {
		c.queueFire(ctx, &lopts)
		c.markFired(ctx, r)
		return
	}
	if errors.Is(err, runs.ErrPaused) {
		return
	}
	if err != nil {
		c.log.Warn("schedule fire", "job", r.JobName, "error", err)
		if c.raiseActionsFault(ctx, r, err) {
			c.markFired(ctx, r)
			return
		}
		if run == nil {
			if _, ferr := c.Runs.FailBeforeLaunch(ctx, lopts, runs.EndLaunch, map[string]string{
				"message": err.Error(),
			}); ferr != nil {
				c.log.Warn("schedule fail before launch", "job", r.JobName, "error", ferr)
			}
		}
	}
	c.markFired(ctx, r)
}

// queueFire puts a schedule fire in the queue behind the live run of its
// job. The when: gate already ran, so the row holds the gated prompt. A full
// queue drops the fire and raises the flood alert.
func (c *ControlPlane) queueFire(ctx context.Context, lopts *runs.LaunchOpts) {
	err := c.queue.Enqueue(ctx, lopts.QueueItem(dispatch.ConnectorSchedule))
	if errors.Is(err, dispatch.ErrLaneFull) {
		c.raiseFlood(ctx, lopts.WorkspaceID, lopts.JobName, dispatch.ConnectorSchedule)
		return
	}
	if err != nil {
		c.log.Warn("schedule queue", "job", lopts.JobName, "error", err)
	}
}

// raiseFlood opens one inbox item for a job whose queue is full. The alert
// cooldown keeps it to one item for each job and hour.
func (c *ControlPlane) raiseFlood(ctx context.Context, workspaceID, jobName, source string) {
	if c.Alerts == nil {
		return
	}
	if _, err := c.Alerts.Raise(ctx, alerts.RaiseOpts{
		Kind:        alerts.KindTriggerFlood,
		WorkspaceID: workspaceID,
		JobName:     jobName,
		Body:        fmt.Sprintf("%d runs of this job already wait to start, so Trebi drops the new ones.", dispatch.LaneCap),
		Hint:        "Let the job run less often, or make its runs shorter.",
	}); err != nil {
		c.log.Warn("flood alert", "job", jobName, "source", source, "error", err)
	}
}

// raiseActionsFault opens one inbox item for a schedule fire that fails on
// its connector actions, in place of a failed run row. The alert cooldown
// keeps it to one item per job per hour. It reports whether err was an
// action error.
func (c *ControlPlane) raiseActionsFault(ctx context.Context, r schedule.Row, err error) bool {
	if !errors.Is(err, runs.ErrActionsInvalid) && !errors.Is(err, runs.ErrActionsUnsupported) {
		return false
	}
	if c.Alerts == nil {
		return true
	}
	if _, aerr := c.Alerts.Raise(ctx, alerts.RaiseOpts{
		Kind:        alerts.KindActionsInvalid,
		WorkspaceID: r.WorkspaceID,
		JobID:       r.JobID,
		JobName:     r.JobName,
		Body:        actionsFaultText(err),
		Hint:        "Open the job and fix its connections.",
	}); aerr != nil {
		c.log.Warn("actions fault alert", "job", r.JobName, "error", aerr)
	}
	return true
}

// actionsFaultText tells why a job cannot use its connections. The raw
// error goes last.
func actionsFaultText(err error) string {
	why := "A connection in its list is missing, or it cannot do what the job needs."
	if errors.Is(err, runs.ErrActionsUnsupported) {
		why = "Its AI app cannot use connections in this setup."
	}
	return "The job did not start. " + why + " Details: " + err.Error()
}

func (c *ControlPlane) markFired(ctx context.Context, r schedule.Row) {
	if err := c.Schedule.MarkFired(ctx, r, time.Now().UTC()); err != nil {
		c.log.Warn("schedule mark fired", "job", r.JobName, "error", err)
	}
}

// evalWhenGate runs the when: gate of the matching cron/every trigger, and
// fills the prompt with the when vars and the fixed inputs of the trigger.
func (c *ControlPlane) evalWhenGate(ctx context.Context, j *job.Job, r schedule.Row, path string) (string, map[string]string, bool) {
	prompt := j.Prompt
	cwd, _ := j.ResolvedWorkingDir(c.Workspaces.OwnerKit(ctx, r.WorkspaceID, r.JobName))
	if cwd == "" {
		cwd = dirOf(path)
	}
	for _, tr := range j.Triggers {
		if (tr.Family != "cron" && tr.Family != "every") || tr.Index != r.Idx {
			continue
		}
		if tr.When != nil {
			wr := job.EvalWhen(ctx, tr.When, cwd, j.Env)
			if wr.Error != "" {
				c.log.Warn("when gate error", "job", r.JobName, "error", wr.Error)
				return prompt, nil, true
			}
			if wr.Skip || !wr.Fire {
				c.log.Info("when gate skip", "job", r.JobName)
				return prompt, nil, true
			}
			for k, v := range wr.Vars {
				prompt = replaceVar(prompt, k, v)
			}
		}
		inputs := j.Fill(&tr, nil, nil).Declared(j.Inputs)
		for k, v := range inputs {
			prompt = replaceVar(prompt, k, v)
		}
		return prompt, inputs, false
	}
	return prompt, nil, false
}

// fireArtifact refreshes a live artifact with no LLM and sends the snapshot
// when the schedule names a delivery (D18). A deleted artifact drops its row.
func (c *ControlPlane) fireArtifact(ctx context.Context, r schedule.Row) {
	defer c.markFired(ctx, r)
	a, err := c.Artifacts.Get(ctx, r.ArtifactID)
	if errors.Is(err, artifacts.ErrNotFound) {
		if err := c.Schedule.DeleteArtifact(ctx, r.ArtifactID); err != nil {
			c.log.Warn("schedule orphan delete", "artifact_id", r.ArtifactID, "error", err)
		}
		return
	}
	if err != nil {
		c.log.Warn("artifact schedule", "artifact_id", r.ArtifactID, "error", err)
		return
	}
	snap, err := c.Artifacts.Refresh(ctx, artifacts.RefreshOpts{ID: a.ID, Reason: artifacts.ReasonSchedule, Actor: "schedule"})
	if err != nil {
		c.log.Warn("artifact refresh", "artifact_id", a.ID, "error", err)
		return
	}
	if a.Deliver == nil || a.Deliver.Email == nil || c.Email == nil {
		return
	}
	ws, err := c.Workspaces.Get(ctx, a.Workspace)
	if err != nil {
		c.log.Warn("artifact delivery", "artifact_id", a.ID, "error", err)
		return
	}
	if _, err := c.Email.EnsureAddress(ctx, ws.ID, ws.Handle); err != nil {
		c.log.Warn("artifact delivery", "artifact_id", a.ID, "error", err)
		return
	}
	agent := c.artifactAgent(ctx, a)
	if agent == "" && a.OwnerKind == artifacts.OwnerJob {
		c.log.Warn("artifact delivery skipped: the job has no owner agent", "artifact_id", a.ID)
		return
	}
	opts := email.SendOpts{
		WorkspaceID: ws.ID, To: a.Deliver.Email.To, Subject: a.Deliver.Email.Subject, ByUser: true, // the user set the recipients
		Artifact: &email.ArtifactRef{ID: a.ID, Version: snap.VersionN, SnapshotID: snap.ID,
			URL: artifacts.PageURL(a.ID, snap.VersionN, snap.ID)},
		Agent: agent,
	}
	if _, err := c.Email.Send(ctx, opts); err != nil {
		c.log.Warn("artifact delivery", "artifact_id", a.ID, "error", err)
	}
}

// artifactAgent is the agent that sends a: the agent that owns it, or the
// owner agent of the job that owns it. The assistant has no agent; it
// sends from the workspace address.
func (c *ControlPlane) artifactAgent(ctx context.Context, a artifacts.Artifact) string {
	if a.OwnerKind == artifacts.OwnerAssistant || a.OwnerID == "" {
		return ""
	}
	row, err := c.Workspaces.JobByID(ctx, a.OwnerID)
	if err != nil {
		return ""
	}
	name := row.Name
	if row.Kind != "agent" {
		name = job.OwnerOf(row.Name)
	}
	return strings.TrimPrefix(name, "agents/")
}
