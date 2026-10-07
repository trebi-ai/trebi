package chains

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/requests"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

type requester interface {
	CreateSystem(ctx context.Context, runID string, o requests.CreateOpts) (requests.Request, error)
}

type alerter interface {
	Raise(ctx context.Context, o alerts.RaiseOpts) (string, error)
}

// WithAlerts sets the inbox for chain_limited.
func (s *Service) WithAlerts(a alerter) { s.alerts = a }

// atLimit holds a hop at a chain limit and asks a person to raise it (F.1).
// With no request grant, or with no caller run, it refuses the hop (F.3).
func (s *Service) atLimit(ctx context.Context, c *Chain, f frame, h *Hop, reason RefuseReason) (*AskResult, error) {
	limitErr := &Error{Code: CodeLimit, Reason: reason, Message: limitText}
	if f.runID == "" || s.req == nil {
		return nil, s.refuse(ctx, h, limitErr)
	}
	turns, cost, err := s.runs.TraceUsage(ctx, c.TraceID)
	if err != nil {
		return nil, err
	}
	names := s.agentNames(ctx, h.pathIDs...)
	path := make([]string, len(h.pathIDs))
	for i, id := range h.pathIDs {
		path[i] = nameOr(names, id)
	}
	target := path[len(path)-1]
	req, err := s.req.CreateSystem(ctx, f.runID, requests.CreateOpts{
		Forms: []requests.FormOpts{{
			Type: requests.TypeConfirm, Title: limitTitle(reason, c.Limits),
			Summary: fmt.Sprintf("Path: %s\nUsed: %d steps, $%.2f\nWaiting ask to %s: %s", strings.Join(path, " → "), turns, cost, target, h.Preview),
			Target:  "agents/" + target,
			Allow:   []requests.Decision{requests.DecisionApprove, requests.DecisionReject, requests.DecisionRespond},
		}},
		Source: requests.SourceChain, HopID: h.ID, TraceID: h.TraceID,
	})
	if errors.Is(err, requests.ErrPlan) {
		if rerr := s.refuse(ctx, h, limitErr); !errors.As(rerr, new(*Error)) {
			return nil, rerr
		}
		s.raiseLimited(ctx, h, f)
		return nil, limitErr
	}
	if err != nil {
		return nil, err
	}
	h.Status, h.RefuseReason, h.RequestID = HopHeld, reason, req.ID
	if err := s.insertHop(ctx, h); err != nil {
		return nil, err
	}
	if err := s.setChain(ctx, h.TraceID, "chain.limited", map[string]any{"status": StatusHeld}); err != nil {
		return nil, err
	}
	return s.result(ctx, h, f, target, false), nil
}

func (s *Service) raiseLimited(ctx context.Context, h *Hop, f frame) {
	if s.alerts == nil {
		return
	}
	if _, err := s.alerts.Raise(ctx, alerts.RaiseOpts{
		Kind: alerts.KindChainLimited, WorkspaceID: h.Workspace, RunID: f.runID, JobID: f.agentID, TraceID: h.TraceID,
		Body: fmt.Sprintf("agents/%s could not ask further: the chain reached its limit (%s).", f.agent, h.RefuseReason),
	}); err != nil {
		s.log.Warn("chain limited alert", "trace_id", h.TraceID, "err", err)
	}
}

// setChain changes the chain row and emits topic.
func (s *Service) setChain(ctx context.Context, traceID, topic string, set map[string]any) error {
	cols := make([]string, 0, len(set)+2)
	args := make([]any, 0, len(set)+2)
	for _, k := range slices.Sorted(maps.Keys(set)) {
		cols = append(cols, k+"=?")
		args = append(args, set[k])
	}
	cols = append(cols, "updated_at=?", "revision=revision+1")
	args = append(args, s.stamp(), traceID)
	return s.tx(ctx, func(tx *sql.Tx) ([]store.BusEvent, error) {
		if _, err := tx.ExecContext(ctx, `UPDATE chains SET `+strings.Join(cols, ", ")+` WHERE trace_id=?`, args...); err != nil {
			return nil, err
		}
		c, err := scanChain(tx.QueryRowContext(ctx, `SELECT `+chainCols+` FROM chains WHERE trace_id=?`, traceID))
		if err != nil {
			return nil, err
		}
		action := strings.TrimPrefix(topic, "chain.")
		return []store.BusEvent{chainEvent(topic, action, c.TraceID, c.Workspace, c.Status, c.rootAgentID, c.Revision)}, nil
	})
}

// RequestDecided applies the decision on a chain limit request (F.2).
func (s *Service) RequestDecided(ctx context.Context, r requests.Request) error {
	h, err := s.hop(ctx, r.HopID)
	if err != nil {
		return err
	}
	if h.Status != HopHeld {
		return nil
	}
	decision := requests.DecisionExpired
	if r.Status == requests.StatusAnswered && len(r.Forms) > 0 && r.Forms[0].Answer != nil {
		decision = r.Forms[0].Answer.Decision
	}
	switch decision {
	case requests.DecisionApprove:
		return s.approve(ctx, h)
	case requests.DecisionReject, requests.DecisionExpired:
		_, err := s.Stop(ctx, h.TraceID, "user", "A person did not raise the chain limit.")
		return err
	default:
		if _, err := s.updateHop(ctx, h.ID, hopUpdate{"status": string(HopStopped)}, HopHeld); err != nil {
			return err
		}
		return s.setChain(ctx, h.TraceID, "chain.updated", map[string]any{"status": StatusOpen})
	}
}

// approve raises the limit that held h, opens the chain, and sends h.
func (s *Service) approve(ctx context.Context, h *Hop) error {
	c, err := s.chain(ctx, h.TraceID)
	if err != nil {
		return err
	}
	ws, _, err := s.dir.ChainLimits(ctx, h.Workspace)
	if err != nil {
		return err
	}
	set := map[string]any{"status": StatusOpen}
	switch h.RefuseReason {
	case RefuseHops:
		set["max_hops"] = max(c.Limits.MaxHops, h.Depth)
	case RefuseUSD:
		set["max_usd"] = c.Limits.MaxUSD + ws.MaxUSD
	default:
		set["max_turns"] = c.Limits.MaxTurns + ws.MaxTurns
	}
	if err := s.setChain(ctx, h.TraceID, "chain.updated", set); err != nil {
		return err
	}
	changed, err := s.updateHop(ctx, h.ID, hopUpdate{"status": string(HopQueued), "refuse_reason": ""}, HopHeld)
	if err != nil || !changed {
		return err
	}
	h.Status, h.RefuseReason = HopQueued, ""
	ids := []string{h.From.AgentID}
	if n := len(h.pathIDs); n > 0 {
		ids = append(ids, h.pathIDs[n-1])
	}
	names := s.agentNames(ctx, ids...)
	f := frame{kind: h.From.Kind, agentID: h.From.AgentID, agent: names[h.From.AgentID], runID: h.From.RunID,
		turn: h.From.Turn, convID: h.From.ConversationID, traceID: h.TraceID}
	return s.send(ctx, h, f, names[ids[len(ids)-1]])
}

// StopResult is the reply of Stop.
type StopResult struct {
	Stopped       bool     `json:"stopped"`
	KilledRuns    []string `json:"killed_runs"`
	CancelledHops []string `json:"cancelled_hops"`
	Unwinding     []string `json:"unwinding"`
}

// Stop ends a chain (C.5). Open hops stop, live runs of the chain end, and
// each waiting caller gets one stop notice, so the chain unwinds.
func (s *Service) Stop(ctx context.Context, traceID, by, reason string) (*StopResult, error) {
	c, err := s.chain(ctx, traceID)
	if err != nil {
		return nil, err
	}
	out := &StopResult{Stopped: true, KilledRuns: []string{}, CancelledHops: []string{}, Unwinding: []string{}}
	if c.Status != StatusStopped {
		if err := s.setChain(ctx, traceID, "chain.stopped", map[string]any{
			"status": StatusStopped, "stopped_by": by, "stop_reason": strings.TrimSpace(reason),
		}); err != nil {
			return nil, err
		}
	}
	asks, err := s.stopHops(ctx, traceID, out)
	if err != nil {
		return nil, err
	}
	live, err := s.liveRuns(ctx, traceID)
	if err != nil {
		return nil, err
	}
	for _, id := range live {
		if err := s.runs.Kill(ctx, id); err != nil {
			s.log.Warn("chain stop kill", "run_id", id, "err", err)
			continue
		}
		out.KilledRuns = append(out.KilledRuns, id)
	}
	for _, h := range asks {
		if h.From.Kind != EndAgent || h.From.RunID == "" {
			continue
		}
		if err := s.reply(ctx, h, HopStopped, End{Kind: EndAgent, AgentID: h.To.AgentID, RunID: h.To.RunID, Turn: h.To.Turn}, stoppedReplyText); err != nil {
			s.log.Warn("chain stop notice", "hop_id", h.ID, "err", err)
			continue
		}
		if !slices.Contains(out.Unwinding, h.From.RunID) {
			out.Unwinding = append(out.Unwinding, h.From.RunID)
		}
	}
	return out, nil
}

// rateHits is the number of rate refusals for one pair in 24 h that the
// doctor reports. It is a sign of a loop in the prompts.
const rateHits = 3

// Doctor reports agent pairs that hit the ask rate limit rateHits times in
// the last 24 h (chain_rate_hits).
func (s *Service) Doctor(ctx context.Context) []alerts.DoctorFinding {
	since := s.now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	rows, err := s.st.DB().QueryContext(ctx, `SELECT workspace_id, from_agent_id, to_agent_id, COUNT(*) FROM chain_hops
WHERE status=? AND refuse_reason=? AND created_at>=?
GROUP BY workspace_id, from_agent_id, to_agent_id HAVING COUNT(*)>=? ORDER BY workspace_id, from_agent_id, to_agent_id`,
		string(HopRefused), string(RefuseRate), since, rateHits)
	if err != nil {
		s.log.Warn("chain doctor", "err", err)
		return nil
	}
	defer rows.Close()
	type pair struct {
		ws, from, to string
		n            int
	}
	var pairs []pair
	var ids []string
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.ws, &p.from, &p.to, &p.n); err != nil {
			s.log.Warn("chain doctor", "err", err)
			return nil
		}
		pairs = append(pairs, p)
		ids = append(ids, p.from, p.to)
	}
	names := s.agentNames(ctx, ids...)
	out := make([]alerts.DoctorFinding, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, alerts.DoctorFinding{
			ID: "chain-rate:" + p.ws + ":" + p.from + ":" + p.to, Severity: "warn", Code: "chain_rate_hits",
			Message: fmt.Sprintf("agents/%s asked agents/%s too often %d times in 24 h. The two prompts can cause a loop.",
				nameOr(names, p.from), nameOr(names, p.to), p.n),
		})
	}
	return out
}

// stopHops sets the open asks and hand-offs of the chain stopped and returns
// the asks.
func (s *Service) stopHops(ctx context.Context, traceID string, out *StopResult) ([]*Hop, error) {
	open, err := s.queryHops(ctx, `WHERE trace_id=? AND kind IN (?,?) AND status IN (?,?,?) ORDER BY created_at, id`,
		traceID, string(KindAsk), string(KindHandoff), string(HopHeld), string(HopQueued), string(HopRunning))
	if err != nil {
		return nil, err
	}
	var asks []*Hop
	for _, h := range open {
		changed, err := s.updateHop(ctx, h.ID, hopUpdate{"status": string(HopStopped)}, h.Status)
		if err != nil {
			return nil, err
		}
		if !changed {
			continue
		}
		if h.Status == HopQueued || h.Status == HopHeld {
			out.CancelledHops = append(out.CancelledHops, h.ID)
		}
		if h.Kind == KindAsk {
			asks = append(asks, h)
		}
	}
	return asks, nil
}

// liveRuns lists the running runs whose open turn is in the chain.
func (s *Service) liveRuns(ctx context.Context, traceID string) ([]string, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT r.id FROM runs r JOIN run_turns t ON t.run_id=r.id AND t.n=r.turn_count
WHERE t.trace_id=? AND r.status=?`, traceID, string(runs.StatusRunning))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
