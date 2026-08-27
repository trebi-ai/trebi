package runs

import (
	"context"
	"fmt"

	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// applyIfRunning enforces session.if_running against live runs of the same job.
// It returns the outcome ("" when no live run, else skip|coalesce|replace|allow)
// and, for skip/coalesce, the run row the caller must return instead of a launch.
func (s *Service) applyIfRunning(ctx context.Context, opts *LaunchOpts) (string, *Run, error) {
	policy := opts.IfRunning
	if policy == "" {
		policy = "skip"
	}

	live, err := s.List(ctx, ListOpts{
		Workspace: opts.WorkspaceID,
		Job:       opts.Job,
		Status:    string(StatusRunning),
		Limit:     20,
	})
	if err != nil {
		return "", nil, err
	}
	if len(live) == 0 {
		return "", nil, nil
	}
	switch policy {
	case "allow":
		return "allow", nil, nil
	case "skip":
		row, err := s.overlapSkip(ctx, opts, live[0])
		if err != nil {
			return "", nil, err
		}
		return "skip", row, nil
	case "replace":
		s.overlapReplace(ctx, live)
		return "replace", nil, nil
	case "coalesce":
		s.overlapCoalesce(ctx, opts, live[0])
		return "coalesce", live[0], nil
	default:
		return "", nil, fmt.Errorf("unknown if_running policy %q", policy)
	}
}

// overlapSkip records a terminal skipped row instead of a launch.
func (s *Service) overlapSkip(ctx context.Context, opts *LaunchOpts, existing *Run) (*Run, error) {
	skipped, err := s.Create(ctx, CreateOpts{
		WorkspaceID:   opts.WorkspaceID,
		Job:           opts.Job,
		Kind:          opts.Kind,
		Reason:        opts.Reason,
		Harness:       opts.Harness,
		PromptPreview: opts.PromptPreview,
		TraceID:       opts.TraceID,
		QueueID:       opts.QueueID,
	})
	if err != nil {
		return nil, err
	}
	_ = s.Terminate(ctx, skipped.ID, StatusSkipped, EndReplace, nil)
	s.AddEvent(ctx, skipped.ID, "skip", "runner", map[string]string{
		"because": existing.ID, "policy": "skip",
	})
	return s.Get(ctx, skipped.ID)
}

// overlapReplace kills every live run so a new launch can start.
func (s *Service) overlapReplace(ctx context.Context, live []*Run) {
	for _, r := range live {
		if r.TmuxSession != "" {
			_ = tmux.KillSession(r.TmuxSession)
		}
		_ = s.Terminate(ctx, r.ID, StatusExited, EndReplace, nil)
		s.AddEvent(ctx, r.ID, "replace", "runner", map[string]string{"by": "new-launch"})
	}
}

// overlapCoalesce types the new prompt into the live run's pane.
func (s *Service) overlapCoalesce(ctx context.Context, opts *LaunchOpts, existing *Run) {
	text := opts.Prompt
	if text == "" {
		text = opts.PromptPreview
	}
	if text == "" {
		text = "(coalesced event)"
	}
	if existing.TmuxSession != "" {
		_ = tmux.Send(existing.TmuxSession, text)
		_ = tmux.SendKey(existing.TmuxSession, "enter")
	}
	s.AddEvent(ctx, existing.ID, "coalesce", "runner", map[string]string{
		"prompt_preview": trunc(text, 200),
	})
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "run.updated", Entity: "run", EntityID: existing.ID, Action: "updated",
		Attrs: map[string]any{"coalesced": true},
	}); err != nil {
		s.log.Warn("coalesce bus append", "run", existing.ID, "error", err)
	}
}
