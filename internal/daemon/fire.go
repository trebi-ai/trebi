package daemon

import (
	"context"
	"time"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/schedule"
)

// due is one schedule row at or past its due time.
type due struct {
	ws, job, spec, tz string
	idx               int
}

// FireDue claims due schedule rows and launches runs (exported for tests).
func (c *ControlPlane) FireDue(ctx context.Context) {
	for _, d := range c.dueSchedules(ctx) {
		c.fireOne(ctx, d)
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

// dueSchedules lists schedule rows whose next_due_at has passed.
func (c *ControlPlane) dueSchedules(ctx context.Context) []due {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := c.Store.DB().QueryContext(ctx, `
		SELECT workspace_id, job, idx, spec, timezone FROM schedules
		WHERE next_due_at IS NOT NULL AND next_due_at <= ? LIMIT 20`, now)
	if err != nil {
		c.log.Warn("schedule due query", "error", err)
		return nil
	}
	defer rows.Close()
	var list []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.ws, &d.job, &d.idx, &d.spec, &d.tz) == nil {
			list = append(list, d)
		}
	}
	return list
}

// fireOne launches one due schedule entry and advances it.
func (c *ControlPlane) fireOne(ctx context.Context, d due) {
	j, path := c.findJob(ctx, d.ws, d.job)
	if j == nil {
		// Job gone: drop the orphan row so it does not stay due forever.
		c.log.Info("schedule orphan deleted", "workspace", d.ws, "job", d.job, "idx", d.idx)
		if _, derr := c.Store.DB().ExecContext(ctx,
			`DELETE FROM schedules WHERE workspace_id=? AND job=? AND idx=?`,
			d.ws, d.job, d.idx); derr != nil {
			c.log.Warn("schedule orphan delete", "job", d.job, "error", derr)
		}
		return
	}
	if j.Disabled {
		return
	}
	// Per-entry when: gate (B25). A skip or a gate error must not launch,
	// but the schedule still advances so the entry does not refire every tick.
	prompt, gateSkip := c.evalWhenGate(ctx, j, d, path)
	if gateSkip {
		c.advanceSchedule(ctx, d.ws, d.job, d.idx, d.spec, "")
		return
	}
	harness := j.Harness
	if harness == "" {
		harness = "claude"
	}
	maxMS, idleMS, keepMS, nudgeMS := j.Session.KnobsMS()
	_, _, err := c.Runs.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID:   d.ws,
			Job:           d.job,
			Kind:          j.Kind,
			Reason:        runs.ReasonSchedule,
			Harness:       harness,
			PromptPreview: trunc(prompt, 200),
			MaxDurationMS: maxMS,
			IdleTimeoutMS: idleMS,
			KeepAliveMS:   keepMS,
			NudgeMS:       nudgeMS,
		},
		Prompt:     prompt,
		WorkingDir: dirOf(path),
		IfRunning:  j.Session.IfRunning,
	})
	if err != nil {
		// Advance anyway: a persistent launch error must not refire every tick.
		c.log.Warn("schedule fire", "job", d.job, "error", err)
		c.advanceSchedule(ctx, d.ws, d.job, d.idx, d.spec, "")
		return
	}
	c.advanceSchedule(ctx, d.ws, d.job, d.idx, d.spec, time.Now().UTC().Format(time.RFC3339))
}

// advanceSchedule moves next_due_at forward; firedAt non-empty also stamps last_fired_at.
func (c *ControlPlane) advanceSchedule(ctx context.Context, ws, jobName string, idx int, spec, firedAt string) {
	now := time.Now().UTC().Format(time.RFC3339)
	next := schedule.NextDuePublic(spec, time.Now().UTC())
	nextStr := next.UTC().Format(time.RFC3339)
	var err error
	if firedAt != "" {
		_, err = c.Store.DB().ExecContext(ctx, `UPDATE schedules SET last_fired_at=?, next_due_at=?, updated_at=?
			WHERE workspace_id=? AND job=? AND idx=?`, firedAt, nextStr, now, ws, jobName, idx)
	} else {
		_, err = c.Store.DB().ExecContext(ctx, `UPDATE schedules SET next_due_at=?, updated_at=?
			WHERE workspace_id=? AND job=? AND idx=?`, nextStr, now, ws, jobName, idx)
	}
	if err != nil {
		c.log.Warn("schedule advance", "job", jobName, "error", err)
	}
}

// evalWhenGate runs the when: gate of the matching cron/every trigger.
// It returns the prompt with gate vars expanded, and whether to skip the fire.
func (c *ControlPlane) evalWhenGate(ctx context.Context, j *job.Job, d due, path string) (string, bool) {
	prompt := j.Prompt
	for _, tr := range j.Triggers {
		if (tr.Family != "cron" && tr.Family != "every") || tr.Index != d.idx {
			continue
		}
		if tr.When != nil {
			wr := job.EvalWhen(ctx, tr.When, dirOf(path), j.Env)
			if wr.Error != "" {
				c.log.Warn("when gate error", "job", d.job, "error", wr.Error)
				return prompt, true
			}
			if wr.Skip || !wr.Fire {
				c.log.Info("when gate skip", "job", d.job)
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
