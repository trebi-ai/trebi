package runs

import (
	"context"
	"errors"
)

// ErrBudgetExceeded is returned when a budget gate refuses agent-owned work.
var ErrBudgetExceeded = errors.New("budget exceeded")

// BudgetGate is the consumer-defined check CreateAndLaunch calls for
// agent-owned work. The daemon wires the workspace implementation.
type BudgetGate interface {
	Allow(ctx context.Context, workspaceID, job, kind string) (ok bool, reason string, err error)
}

// SetBudgetGate wires the process-local gate. Nil disables enforcement.
func (s *Service) SetBudgetGate(g BudgetGate) { s.budgetGate = g }
