// Package launch builds the launch options of a run that already exists from
// its current job file. Retry, replay, the queue, and requests share it.
package launch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// ErrJobMissing reports a run whose job file is gone or does not parse.
var ErrJobMissing = errors.New("job file for run no longer resolvable")

type Launcher struct {
	ws   *workspaces.Service
	runs *runs.Service
}

func NewLauncher(ws *workspaces.Service, rs *runs.Service) *Launcher {
	return &Launcher{ws: ws, runs: rs}
}

// FromRun rebuilds the launch options of prev from its current job file: the
// prompt with trigger vars and inputs applied, the working dir, the env, the
// model, the session knobs, the memory, and the kit. The trigger capture and
// the conversation key carry over.
func (l *Launcher) FromRun(ctx context.Context, prev *runs.Run, reason runs.RunReason) (runs.LaunchOpts, error) {
	row, err := l.jobRow(ctx, prev)
	if err != nil {
		return runs.LaunchOpts{}, err
	}
	j, err := job.ParseFile(row.Path)
	if err != nil || j == nil {
		return runs.LaunchOpts{}, fmt.Errorf("%w: %s", ErrJobMissing, prev.Job)
	}
	extra := map[string]string{}
	if prev.Trigger != nil {
		for k, v := range prev.Trigger.Vars {
			extra[k] = v
		}
	}
	for k, v := range prev.Inputs {
		extra[k] = v
	}
	wsPath := ""
	if ws, err := l.ws.Get(ctx, prev.Workspace); err == nil {
		wsPath = ws.Path
	}
	prompt := job.ExpandPrompt(j.Prompt, j.Dir, wsPath, extra)
	lopts := runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: prev.Workspace, Job: prev.Job, Kind: prev.Kind,
			Reason: reason, Harness: prev.Harness,
			PromptPreview:   prompt,
			Inputs:          prev.Inputs,
			Trigger:         prev.Trigger,
			ConversationKey: prev.ConversationKey,
		},
		Prompt: prompt,
	}
	kit := l.ws.OwnerKit(ctx, prev.Workspace, prev.Job)
	d := j.LaunchDefaultsWithKit(kit)
	lopts.WorkingDir = d.WorkingDir
	lopts.Env = d.Env
	lopts.Model, lopts.Effort = d.Model, d.Effort
	lopts.IfRunning = d.IfRunning
	lopts.MaxDurationMS, lopts.IdleTimeoutMS, lopts.KeepAliveMS, lopts.NudgeMS = d.MaxDurationMS, d.IdleTimeoutMS, d.KeepAliveMS, d.NudgeMS
	// A replay or retry honors the document runtime (Part J): a job that says
	// runtime: tui reopens the pane instead of silently flipping to the wire.
	lopts.Runtime = j.Runtime
	if wsPath != "" {
		lopts.Memory = runs.NewMemoryLaunch(j, wsPath)
	}
	lopts.Kit = kit
	lopts.JobSkills = j.Skills
	lopts.JobConnectors = j.Connectors
	return lopts, nil
}

// Resume continues the vendor session of prevID with o.Prompt.
func (l *Launcher) Resume(ctx context.Context, prevID string, o runs.ResumeOpts) (*runs.Run, error) {
	prev, err := l.runs.Get(ctx, prevID)
	if err != nil {
		return nil, err
	}
	base, err := l.FromRun(ctx, prev, o.Reason)
	if err != nil {
		return nil, err
	}
	return l.runs.Resume(ctx, prevID, base, o)
}

// Fresh starts a new run of the job of prevID with prompt and no session.
func (l *Launcher) Fresh(ctx context.Context, prevID, prompt string, reason runs.RunReason) (*runs.Run, error) {
	prev, err := l.runs.Get(ctx, prevID)
	if err != nil {
		return nil, err
	}
	lopts, err := l.FromRun(ctx, prev, reason)
	if err != nil {
		return nil, err
	}
	lopts.ResumedFrom = prev.ID
	lopts.ResumeAttempts = prev.ResumeAttempts
	lopts.ConversationID = prev.ConversationID
	lopts.Prompt = prompt
	lopts.PromptPreview = prompt
	lopts.BypassOverlap = true
	run, _, err := l.runs.CreateAndLaunch(ctx, lopts)
	return run, err
}

// jobRow resolves the markdown that produced prev: JOB.md for a job run,
// AGENT.md for an agent run, TREBI.md for an assistant run.
func (l *Launcher) jobRow(ctx context.Context, prev *runs.Run) (workspaces.JobRow, error) {
	rows, err := l.ws.ListJobs(ctx, prev.Workspace)
	if err != nil {
		return workspaces.JobRow{}, err
	}
	for _, row := range rows {
		switch {
		case prev.Kind == runs.KindAgent && row.Kind == "agent" &&
			(row.Name == prev.Job || strings.TrimPrefix(row.Name, "agents/") == prev.Job):
			return row, nil
		case prev.Kind == dispatch.KindAssistant && row.Kind == "assistant":
			return row, nil
		case prev.Kind != runs.KindAgent && prev.Kind != dispatch.KindAssistant && row.Kind == "job" && row.Name == prev.Job:
			return row, nil
		}
	}
	return workspaces.JobRow{}, fmt.Errorf("%w: %s", ErrJobMissing, prev.Job)
}
