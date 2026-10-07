package chains

import (
	"context"
	"errors"

	"github.com/trebi-ai/trebi/internal/runs"
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
	turns, _, err := s.runs.TraceUsage(ctx, h.TraceID)
	if err != nil {
		return nil, err
	}
	lf := &LaunchFrame{Reason: runs.ReasonAsk, Path: h.pathIDs}
	if h.Kind == KindHandoff {
		lf.Reason = runs.ReasonHandoff
	}
	lf.Wake = &runs.WakeChain{Depth: h.Depth, MaxHops: c.Limits.MaxHops, Turns: turns, MaxTurns: c.Limits.MaxTurns}
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
