package runs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/trebi-ai/trebi/internal/pauses"
)

// ErrBudgetExceeded is returned when a budget gate refuses agent-owned work.
var ErrBudgetExceeded = errors.New("budget exceeded")

// ErrPaused is returned when a pause holds new work. The message names
// the pause.
var ErrPaused = errors.New("paused")

// ErrWorkspaceArchived refuses a launch in a workspace that moved to
// another machine.
var ErrWorkspaceArchived = errors.New("workspace archived")

// ArchivedMessage is the user text of ErrWorkspaceArchived.
func ArchivedMessage(err error) string {
	return "This workspace moved to " + strings.TrimPrefix(err.Error(), ErrWorkspaceArchived.Error()+": moved to ") + "."
}

func (s *Service) refuseArchived(ctx context.Context, workspaceID string) error {
	if workspaceID == "" {
		return nil
	}
	var name string
	err := s.st.DB().QueryRowContext(ctx, `SELECT COALESCE(m.name, w.moved_to, '') FROM workspaces w
		LEFT JOIN machines m ON m.id = w.moved_to WHERE w.id = ? AND w.archived_at IS NOT NULL`, workspaceID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: moved to %s", ErrWorkspaceArchived, name)
}

// BudgetGate is the consumer-defined check CreateAndLaunch calls for
// agent-owned work. The daemon wires the workspace implementation.
type BudgetGate interface {
	Allow(ctx context.Context, workspaceID, job, kind string) (ok bool, reason string, err error)
}

// SetBudgetGate wires the process-local gate. Nil disables enforcement.
func (s *Service) SetBudgetGate(g BudgetGate) { s.budgetGate = g }

// PauseGate finds the pause that holds new work. The daemon wires the
// pauses service.
type PauseGate interface {
	Cover(ctx context.Context, workspaceID, jobID, kind, harness string) (*pauses.Pause, error)
}

// SetPauseGate wires the pause check. Nil disables it.
func (s *Service) SetPauseGate(g PauseGate) { s.pauseGate = g }

// holdsFor reports whether a pause can hold this launch. A launch by a
// person and the next turn of existing work pass.
func (o *LaunchOpts) holdsFor() bool {
	if o.ActorToken == "" && (o.Reason == ReasonManual || o.Reason == ReasonReplay) {
		return false
	}
	return o.ResumeSessionID == "" && o.PreviousRunID == "" && o.ReopenRunID == ""
}

// checkPause refuses a launch that a pause holds.
func (s *Service) checkPause(ctx context.Context, opts *LaunchOpts) error {
	if s.pauseGate == nil || !opts.holdsFor() {
		return nil
	}
	p, err := s.pauseGate.Cover(ctx, opts.WorkspaceID, opts.JobID, opts.Kind, opts.Harness)
	if err != nil || p == nil {
		return err
	}
	return fmt.Errorf("%w: %s is paused", ErrPaused, p.Label)
}
