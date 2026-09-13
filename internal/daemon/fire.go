package daemon

import (
	"context"
	"time"

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
// kind instead of being orphan-deleted.
func (c *ControlPlane) fireOne(ctx context.Context, r schedule.Row) {
	j, path := c.findJob(ctx, r.WorkspaceID, r.Job)
	if j == nil {
		c.log.Info("schedule orphan deleted", "workspace", r.WorkspaceID, "job", r.Job, "idx", r.Idx)
		if err := c.Schedule.Delete(ctx, r.WorkspaceID, r.Job, r.Idx); err != nil {
			c.log.Warn("schedule orphan delete", "job", r.Job, "error", err)
		}
		return
	}
	if j.Disabled {
		return
	}
	prompt, gateSkip := c.evalWhenGate(ctx, j, r, path)
	if gateSkip {
		return
	}
	harness := j.ResolvedHarness()
	if reason := c.harnessBlocked(ctx, harness); reason != "" {
		c.log.Info("schedule skip harness", "job", r.Job, "harness", harness, "reason", reason)
		return
	}
	maxMS, idleMS, keepMS, nudgeMS := j.Session.KnobsMS()
	cwd := j.ResolvedWorkingDir()
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
	_, _, err := c.Runs.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID:   r.WorkspaceID,
			Job:           r.Job,
			Kind:          j.Kind,
			Reason:        runs.ReasonSchedule,
			Harness:       harness,
			PromptPreview: trunc(prompt, 200),
			MaxDurationMS: maxMS,
			IdleTimeoutMS: idleMS,
			KeepAliveMS:   keepMS,
			NudgeMS:       nudgeMS,
			WorkingDir:    cwd,
		},
		Prompt:    prompt,
		Wake:      wake,
		IfRunning: j.Session.IfRunning,
		Env:       j.LaunchEnv(),
	})
	if err != nil {
		c.log.Warn("schedule fire", "job", r.Job, "error", err)
		return
	}
	if err := c.Schedule.MarkFired(ctx, r, time.Now().UTC()); err != nil {
		c.log.Warn("schedule mark fired", "job", r.Job, "error", err)
	}
}

// evalWhenGate runs the when: gate of the matching cron/every trigger.
func (c *ControlPlane) evalWhenGate(ctx context.Context, j *job.Job, r schedule.Row, path string) (string, bool) {
	prompt := j.Prompt
	cwd := j.ResolvedWorkingDir()
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
				c.log.Warn("when gate error", "job", r.Job, "error", wr.Error)
				return prompt, true
			}
			if wr.Skip || !wr.Fire {
				c.log.Info("when gate skip", "job", r.Job)
				return prompt, true
			}
			for k, v := range wr.Vars {
				prompt = replaceVar(prompt, k, v)
			}
		}
		break
	}
	return prompt, false
}
