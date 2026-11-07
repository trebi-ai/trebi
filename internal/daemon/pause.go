package daemon

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/pauses"
	"github.com/trebi-ai/trebi/internal/schedule"
)

// defaultPauseInterval is the tick of the pause worker.
const defaultPauseInterval = 10 * time.Second

// pauseRecheck is the longest defer of a held queue row. A row of a pause
// with no end looks again at this interval.
const pauseRecheck = time.Minute

// pauseWorker ends the pauses whose end time is past and pauses a harness
// near its usage limit.
func (c *ControlPlane) pauseWorker(ctx context.Context) {
	every := c.Opts.PauseInterval
	if every <= 0 {
		every = defaultPauseInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		c.pauseTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *ControlPlane) pauseTick(ctx context.Context) {
	if _, err := c.Pauses.ExpireDue(ctx); err != nil {
		c.log.Warn("pause expiry", "error", err)
	}
	if c.Store.IsDraining() {
		return
	}
	c.autoPause(ctx)
}

// autoPause pauses each harness with a usage window at its pause_at
// percent, or with a hard limit.
func (c *ControlPlane) autoPause(ctx context.Context) {
	cfg, _ := config.Load()
	limits := map[string]*pauses.Limit{}
	get := func(name string) *pauses.Limit {
		if limits[name] == nil {
			limits[name] = &pauses.Limit{Harness: name}
		}
		return limits[name]
	}
	if c.Usage != nil {
		usage, err := c.Usage.Store().List(ctx)
		if err != nil {
			c.log.Warn("auto pause usage", "error", err)
			return
		}
		for _, u := range usage {
			get(u.Harness).Windows = u.Windows
		}
	}
	rows, err := c.Store.DB().QueryContext(ctx, `SELECT name FROM harness_state WHERE limit_exceeded = 1`)
	if err != nil {
		c.log.Warn("auto pause limits", "error", err)
		return
	}
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			get(name).Hit = true
		}
	}
	_ = rows.Close() // read-only rows
	for name, l := range limits {
		p, changed, err := c.Pauses.AutoPause(ctx, *l, cfg.HarnessPauseAt(name))
		if err != nil {
			c.log.Warn("auto pause", "harness", name, "error", err)
			continue
		}
		if changed {
			c.raiseHarnessPaused(ctx, p)
		}
	}
}

func (c *ControlPlane) raiseHarnessPaused(ctx context.Context, p pauses.Pause) {
	if c.Alerts == nil {
		return
	}
	var until time.Time
	if p.Until != nil {
		until, _ = time.Parse(time.RFC3339, *p.Until)
	}
	body := c.Alerts.PausedBody(p.Target, until)
	if _, err := c.Alerts.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindHarnessPaused, Harness: p.Target, PauseID: p.ID,
		Body: body, TrayOnly: true}); err != nil {
		c.log.Warn("raise harness_paused", "harness", p.Target, "error", err)
	}
}

// PauseEnded releases the work that p held: the waiting queue rows, the
// kept schedule fires, and the limit flag of a limit pause.
func (c *ControlPlane) PauseEnded(ctx context.Context, p pauses.Pause) {
	if _, err := c.queue.Release(ctx, dispatch.DeferPaused); err != nil {
		c.log.Warn("release paused queue", "pause", p.ID, "error", err)
	}
	misses := c.runMisses(ctx, p)
	if p.Scope == pauses.ScopeHarness && p.Source == pauses.SourceLimit {
		if err := c.Runs.ClearLimit(ctx, p.Target); err != nil {
			c.log.Warn("clear harness limit", "harness", p.Target, "error", err)
		}
	}
	if c.Alerts == nil {
		return
	}
	if p.Scope == pauses.ScopeHarness {
		if err := c.Alerts.ResolveSubject(ctx, "pause/"+p.Target, "auto"); err != nil {
			c.log.Warn("close harness_paused", "harness", p.Target, "error", err)
		}
	}
	if p.EndedBy == pauses.EndedByExpiry {
		c.raisePauseEnded(ctx, p, misses)
	}
}

// runMisses makes each schedule that p kept due now. It returns the count.
func (c *ControlPlane) runMisses(ctx context.Context, p pauses.Pause) int {
	misses, err := c.Pauses.TakeMisses(ctx, p.ID)
	if err != nil {
		c.log.Warn("pause misses", "pause", p.ID, "error", err)
	}
	for _, m := range misses {
		if err := c.Schedule.DueNow(ctx, m.WorkspaceID, m.JobID, m.Idx, c.now()); err != nil {
			c.log.Warn("pause miss due", "pause", p.ID, "job_id", m.JobID, "error", err)
		}
	}
	return len(misses)
}

func (c *ControlPlane) raisePauseEnded(ctx context.Context, p pauses.Pause, misses int) {
	body := "No runs were skipped."
	switch {
	case p.SkippedFires == 1:
		body = "1 scheduled run was skipped."
	case p.SkippedFires > 1:
		body = strconv.Itoa(p.SkippedFires) + " scheduled runs were skipped."
	}
	switch {
	case misses == 1:
		body += " 1 job runs now, one time."
	case misses > 1:
		body += fmt.Sprintf(" %d jobs run now, one time each.", misses)
	}
	if _, err := c.Alerts.Raise(ctx, alerts.RaiseOpts{Kind: alerts.KindPauseEnded, Subject: "pause-ended/" + p.ID,
		Title: p.Label + " resumed", Body: body, TrayOnly: true}); err != nil {
		c.log.Warn("raise pause_ended", "pause", p.ID, "error", err)
	}
}

// heldFire skips a schedule fire that a pause holds. It counts the skip
// and, with missed once, keeps the fire for the end of the pause.
func (c *ControlPlane) heldFire(ctx context.Context, r schedule.Row, kind, harnessName string) bool {
	p, err := c.Pauses.Cover(ctx, r.WorkspaceID, r.JobID, kind, harnessName)
	if err != nil {
		c.log.Warn("pause check", "job", r.JobName, "error", err)
		return false
	}
	if p == nil {
		return false
	}
	if err := c.Pauses.Skipped(ctx, *p, r.WorkspaceID, r.JobID, r.Idx); err != nil {
		c.log.Warn("pause skip", "job", r.JobName, "error", err)
	}
	c.log.InfoContext(ctx, "schedule.paused", "workspace", r.WorkspaceID, "job", r.JobName, "idx", r.Idx,
		"pause", p.ID, "missed", p.Missed)
	return true
}

// heldItem defers a queue row that a pause holds, until the pause ends or
// for pauseRecheck. A launch by a person passes.
func (c *ControlPlane) heldItem(ctx context.Context, item *dispatch.QueueItem, kind, harnessName string, manual bool) bool {
	if manual {
		return false
	}
	p, err := c.Pauses.Cover(ctx, item.WorkspaceID, item.JobID, kind, harnessName)
	if err != nil {
		c.log.Warn("pause check", "queue_id", item.ID, "error", err)
		return false
	}
	if p == nil {
		return false
	}
	until := c.now().Add(pauseRecheck)
	if p.Until != nil {
		if end, err := time.Parse(time.RFC3339, *p.Until); err == nil && end.Before(until) {
			until = end
		}
	}
	if err := c.queue.Defer(ctx, item.ID, until, dispatch.DeferPaused); err != nil {
		c.log.Warn("queue defer for pause", "queue_id", item.ID, "error", err)
	}
	return true
}

var _ pauses.EndHook = (*ControlPlane)(nil)
