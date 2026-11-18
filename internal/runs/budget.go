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

// ErrJobArchived refuses a new run of an archived job or agent.
var ErrJobArchived = errors.New("job archived")

// archivedError says which archive refused the launch.
type archivedError struct {
	agent bool   // the object is an agent
	owner string // short name of the archived owner agent of a job
}

func (e *archivedError) Error() string { return ErrJobArchived.Error() }

func (e *archivedError) Is(target error) bool { return target == ErrJobArchived }

// JobArchivedMessage is the user text of ErrJobArchived.
func JobArchivedMessage(err error) string {
	var ae *archivedError
	switch {
	case !errors.As(err, &ae):
		return "Unarchive this job first."
	case ae.owner != "":
		return "Unarchive the agent " + ae.owner + " first."
	case ae.agent:
		return "Unarchive this agent first."
	}
	return "Unarchive this job first."
}

// ArchivedErr is ErrJobArchived for an archived row. ownerName is the short
// name of the archived owner agent of a job, or "" for an own archive.
func ArchivedErr(agent bool, ownerName string) error {
	return &archivedError{agent: agent, owner: ownerName}
}

// RunAllowedWhenArchived reports whether a launch reason passes the archive
// gate. A request answer and a chat reply finish work that started before.
func RunAllowedWhenArchived(r RunReason) bool { return r == ReasonRequest || r == ReasonReply }

// refuseArchivedJob refuses a new run of an archived job, of a job of an
// archived agent, or of an archived agent. A request answer and a chat reply
// pass, because they finish a run that started before the archive.
func (s *Service) refuseArchivedJob(ctx context.Context, opts LaunchOpts) error {
	if opts.JobID == "" || RunAllowedWhenArchived(opts.Reason) {
		return nil
	}
	var kind, own, owner, ownerName string
	err := s.st.DB().QueryRowContext(ctx, `SELECT j.kind, COALESCE(j.archived_at,''), COALESCE(o.archived_at,''), COALESCE(o.name,'')
		FROM job_index j LEFT JOIN job_index o ON o.id = j.owner_id AND j.owner_id <> ''
		WHERE j.id = ? AND j.retired_at IS NULL`, opts.JobID).Scan(&kind, &own, &owner, &ownerName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case own != "":
		return &archivedError{agent: kind == "agent"}
	case owner != "":
		return &archivedError{owner: strings.TrimPrefix(ownerName, "agents/")}
	}
	return nil
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
