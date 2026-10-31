package runs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrBudgetExceeded is returned when a budget gate refuses agent-owned work.
var ErrBudgetExceeded = errors.New("budget exceeded")

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
