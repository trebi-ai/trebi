package chains

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/tools"
)

// ErrHopClosed reports a queued wake whose hop no longer waits (a stop).
var ErrHopClosed = errors.New("hop is closed")

// LaunchFrame is what the queue needs to launch the target turn of a hop.
type LaunchFrame struct {
	Reason runs.RunReason
	Path   []string // agent IDs, root first
	Wake   *runs.WakeChain
}

// LaunchFrame reads the launch frame of a queued hop.
func (s *Service) LaunchFrame(ctx context.Context, hopID string) (*LaunchFrame, error) {
	h, err := s.hop(ctx, hopID)
	if err != nil {
		return nil, err
	}
	if h.Status != HopQueued {
		return nil, ErrHopClosed
	}
	c, err := s.chain(ctx, h.TraceID)
	if err != nil {
		return nil, err
	}
	used, err := s.runs.TraceUsage(ctx, h.TraceID)
	if err != nil {
		return nil, err
	}
	lf := &LaunchFrame{Reason: runs.ReasonAsk, Path: h.pathIDs}
	if h.Kind == KindHandoff {
		lf.Reason = runs.ReasonHandoff
	}
	lf.Wake = &runs.WakeChain{Depth: h.Depth, MaxHops: c.Limits.MaxHops, Turns: used.Turns, MaxTurns: c.Limits.MaxTurns}
	if h.Kind == KindAsk && len(h.pathIDs) > 1 {
		lf.Wake.Waiting = s.AgentNames(ctx, h.pathIDs[:len(h.pathIDs)-1])
	}
	return lf, nil
}

// Bind sets the target run and turn of a hop after its launch.
func (s *Service) Bind(ctx context.Context, hopID, runID string, turn int) error {
	changed, err := s.updateHop(ctx, hopID, hopUpdate{
		"status": string(HopRunning), "to_run_id": runID, "to_turn": turn,
	}, HopQueued)
	if err != nil || !changed {
		return err
	}
	h, err := s.Hop(ctx, hopID)
	if err != nil {
		return err
	}
	s.runs.AddEvent(ctx, runID, "hop_received", "chains", map[string]string{
		"hop_id": h.ID, "kind": string(h.Kind), "from_agent": h.From.Agent, "from_run_id": h.From.RunID,
	})
	// A short turn can end before the launch returns: capture its reply now.
	if run, err := s.runs.Get(ctx, runID); err == nil && (run.Status != runs.StatusRunning || run.TurnCount > turn) {
		s.RunEnded(ctx, runID, run.Status)
	}
	return nil
}

// LaunchFailed fails a hop whose target run did not launch. The caller of
// an ask gets a failure reply.
func (s *Service) LaunchFailed(ctx context.Context, hopID string, launchErr error) error {
	set := hopUpdate{"status": string(HopFailed)}
	reason := "the run did not start"
	if errors.Is(launchErr, runs.ErrBudgetExceeded) {
		set["refuse_reason"] = string(RefuseBudget)
		reason = "the agent is over its budget"
	} else if errors.Is(launchErr, runs.ErrJobArchived) {
		set["refuse_reason"] = string(RefuseArchived)
		reason = "the agent is archived"
	}
	changed, err := s.updateHop(ctx, hopID, set, HopQueued)
	if err != nil || !changed {
		return err
	}
	h, err := s.hop(ctx, hopID)
	if err != nil || h.Kind != KindAsk {
		return err
	}
	name := s.agentNames(ctx, h.To.AgentID)[h.To.AgentID]
	return s.reply(ctx, h, HopFailed, End{Kind: EndAgent, AgentID: h.To.AgentID}, failedReplyText(name, reason))
}

// LaunchTrace gives the trace and the path of a run that a run starts. The
// new run takes the place of the caller in the path, because the caller does
// not wait for it. A caller that is not an agent run or a job run, or that
// is in another workspace, gets no trace: the launch starts a new one.
func (s *Service) LaunchTrace(ctx context.Context, a *tools.Actor, workspace, jobID string) (string, []string, error) {
	if a == nil || a.RunID == "" || (a.Kind != string(runs.KindAgent) && a.Kind != string(runs.KindJob)) {
		return "", nil, nil
	}
	run, err := s.runs.Get(ctx, a.RunID)
	if err != nil {
		return "", nil, fmt.Errorf("caller run %s: %w", a.RunID, err)
	}
	if run.Workspace != workspace {
		return "", nil, nil
	}
	trace, path := run.TraceID, []string{run.JobID}
	switch t, err := s.runs.Turn(ctx, run.ID, run.TurnCount); {
	case err == nil:
		trace = cmp.Or(t.TraceID, trace)
		if len(t.Path) > 0 {
			path = t.Path
		}
	case !errors.Is(err, sql.ErrNoRows):
		return "", nil, fmt.Errorf("caller turn %s/%d: %w", run.ID, run.TurnCount, err)
	}
	switch c, err := s.chain(ctx, trace); {
	case err == nil:
		if c.Status == StatusStopped {
			return "", nil, &Error{Code: CodeStopped, Reason: RefuseStopped,
				Message: "A person stopped this chain. Do not start more work in it."}
		}
	case !errors.Is(err, ErrNotFound):
		return "", nil, err
	}
	return trace, append(path[:len(path)-1:len(path)-1], jobID), nil
}
