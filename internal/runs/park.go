package runs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
)

var (
	// ErrNoSession reports a resume of a run with no session id.
	ErrNoSession = errors.New("run has no session to resume")
	// ErrResumeUnsupported wraps the harness errors for a session that cannot
	// resume (agentwire and tmux). A caller falls back to a fresh run.
	ErrResumeUnsupported = errors.New("session resume not supported")
	// ErrNotParked reports Unpark of a run that is not parked.
	ErrNotParked = errors.New("run is not parked")
)

// RequestGate is the requests seam of runs. A run with an open request parks
// when its session ends; RunEnded tells the requests side that a run is done.
type RequestGate interface {
	HasOpen(ctx context.Context, runID string) (bool, error)
	RunEnded(ctx context.Context, runID string, st Status)
}

// SetRequestGate wires the requests seam. Nil (the default) never parks.
func (s *Service) SetRequestGate(g RequestGate) {
	s.mu.Lock()
	s.gate = g
	s.mu.Unlock()
}

func (s *Service) requestGate() RequestGate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gate
}

// hasOpenRequest reports whether a run waits for a request answer. An error
// reads as false: a run must not park on a store fault.
func (s *Service) hasOpenRequest(ctx context.Context, runID string) bool {
	g := s.requestGate()
	if g == nil {
		return false
	}
	open, err := g.HasOpen(ctx, runID)
	if err != nil {
		s.log.Warn("request gate", "run", runID, "error", err)
		return false
	}
	return open
}

// parkStatus changes a clean end to parked when the run has an open request.
// A failure status stays as it is.
func (s *Service) parkStatus(ctx context.Context, runID string, status Status, end EndReason) (Status, EndReason) {
	if status == StatusParked {
		return StatusParked, EndRequest
	}
	if status != StatusComplete && status != StatusExited {
		return status, end
	}
	if s.hasOpenRequest(ctx, runID) {
		return StatusParked, EndRequest
	}
	return status, end
}

// Unpark completes a parked run whose requests closed without an answer to
// deliver (a dismiss).
func (s *Service) Unpark(ctx context.Context, runID string, end EndReason) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, end_reason = ?, revision = revision + 1, updated_at = ?
			WHERE id = ? AND status = ?`, string(StatusComplete), string(end), now, runID, string(StatusParked))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("unpark %s: %w", runID, ErrNotParked)
		}
		return nil
	}, store.BusEvent{
		Topic: "run.updated", Entity: "run", EntityID: runID, Action: "updated",
		Attrs: map[string]string{"status": string(StatusComplete), "end_reason": string(end)},
	})
	return err
}
