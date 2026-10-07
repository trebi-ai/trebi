package chains

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/runs"
)

// staleClaim is the age after which Stale treats a reply claim as lost.
const staleClaim = 2 * time.Minute

var _ runs.ParkGate = (*Service)(nil)

// transcripts stores the new turns of a run in its conversation now, so the
// reply text is there at turn end. The daemon implements it.
type transcripts interface {
	IngestTurns(ctx context.Context, runID string)
}

// WithTranscripts sets the transcript reader of reply capture.
func (s *Service) WithTranscripts(t transcripts) { s.transcripts = t }

// Name is the park gate name.
func (s *Service) Name() string { return runs.GateAsk }

// PromptHint is empty: the team section of the wake tells an agent how to ask.
func (s *Service) PromptHint(runs.LaunchOpts) string { return "" }

// Open reports asks of runID that wait for a reply (waiting) and replies to
// runID that are not delivered yet (ready). A claimed reply goes to the turn
// in flight, so it is not ready.
func (s *Service) Open(ctx context.Context, runID string) (waiting, ready bool, err error) {
	var asks, replies int
	err = s.st.DB().QueryRowContext(ctx, `SELECT
  COALESCE(SUM(CASE WHEN kind=? AND from_run_id=? AND status IN (?,?,?) THEN 1 ELSE 0 END),0),
  COALESCE(SUM(CASE WHEN kind=? AND to_run_id=? AND delivery_state=? THEN 1 ELSE 0 END),0)
FROM chain_hops WHERE from_run_id=? OR to_run_id=?`,
		string(KindAsk), runID, string(HopHeld), string(HopQueued), string(HopRunning),
		string(KindReply), runID, DeliveryNone, runID, runID).Scan(&asks, &replies)
	return asks > 0, replies > 0, err
}

// RunEnded captures the replies of a target turn that ended (C.4). A parked
// target still works on the ask. A hand-off is done at the end of its first
// target turn.
func (s *Service) RunEnded(ctx context.Context, runID string, st runs.Status) {
	hops, err := s.queryHops(ctx, `WHERE to_run_id=? AND status=? AND kind IN (?,?) ORDER BY created_at, id`,
		runID, string(HopRunning), string(KindAsk), string(KindHandoff))
	if err != nil || len(hops) == 0 {
		if err != nil {
			s.log.Warn("chain run ended", "run_id", runID, "err", err)
		}
		return
	}
	run, err := s.runs.Get(ctx, runID)
	if err != nil {
		s.log.Warn("chain run ended", "run_id", runID, "err", err)
		return
	}
	for _, h := range hops {
		if h.Kind == KindHandoff {
			if _, err := s.updateHop(ctx, h.ID, hopUpdate{"status": string(HopDone)}, HopRunning); err != nil {
				s.log.Warn("hand-off done", "hop_id", h.ID, "err", err)
			}
			continue
		}
		if st == runs.StatusParked {
			continue
		}
		if err := s.capture(ctx, h, run, st); err != nil {
			s.log.Warn("reply capture", "hop_id", h.ID, "err", err)
		}
	}
}

// capture closes an ask with the reply text of the target turn and sends the
// reply to the caller.
func (s *Service) capture(ctx context.Context, h *Hop, run *runs.Run, st runs.Status) error {
	name := s.agentNames(ctx, h.To.AgentID)[h.To.AgentID]
	status := HopAnswered
	var text string
	switch st {
	case runs.StatusFailed, runs.StatusTimedOut, runs.StatusExited:
		status, text = HopFailed, failedReplyText(name, string(cmp.Or(run.EndReason, runs.EndReason(st))))
	default:
		text = s.replyText(ctx, h, run)
		if text == "" {
			text = noReplyText(name)
		}
	}
	changed, err := s.updateHop(ctx, h.ID, hopUpdate{"status": string(status)}, HopRunning)
	if err != nil || !changed {
		return err
	}
	return s.reply(ctx, h, status, End{Kind: EndAgent, AgentID: h.To.AgentID, RunID: run.ID, Turn: run.TurnCount}, text)
}

// replyText reads the last assistant text of the turn that ended.
func (s *Service) replyText(ctx context.Context, h *Hop, run *runs.Run) string {
	if s.transcripts != nil {
		s.transcripts.IngestTurns(ctx, run.ID)
	}
	since := time.Time{}
	if t, err := s.runs.Turn(ctx, run.ID, run.TurnCount); err == nil {
		since = t.StartedAt
	}
	conv := run.ConversationID
	if conv == "" {
		conv = h.ConversationID
	}
	text, err := s.conv.TurnReply(ctx, conv, run.ID, since)
	if err != nil {
		s.log.Warn("reply text", "hop_id", h.ID, "err", err)
	}
	return text
}

// reply inserts the reply hop of ask and delivers it: an agent caller wakes,
// a person or the assistant already has the text in the conversation.
func (s *Service) reply(ctx context.Context, ask *Hop, status HopStatus, from End, text string) error {
	r := &Hop{
		ID: "hop_" + ulid.Make().String(), TraceID: ask.TraceID, Workspace: ask.Workspace, Kind: KindReply,
		Status: status, Depth: max(ask.Depth-1, 0), From: from, To: ask.From, ReplyOf: ask.ID,
		ConversationID: ask.ConversationID, Text: text, Preview: preview(text), DeliveryState: DeliveryNone,
	}
	if n := len(ask.pathIDs); n > 0 {
		r.pathIDs = slices.Clone(ask.pathIDs[:n-1])
	}
	if ask.From.Kind != EndAgent {
		r.DeliveryState = DeliveryDelivered
	}
	if err := s.insertHop(ctx, r); err != nil {
		return err
	}
	if from.RunID != "" {
		s.runs.AddEvent(ctx, from.RunID, "hop_replied", "chains", map[string]string{
			"hop_id": r.ID, "reply_of": ask.ID, "to_run_id": ask.From.RunID, "to_agent": ask.From.AgentID,
		})
	}
	if ask.From.Kind != EndAgent || ask.From.RunID == "" {
		return nil
	}
	s.runs.AddEvent(ctx, ask.From.RunID, "hop_replied", "chains", map[string]string{
		"hop_id": r.ID, "reply_of": ask.ID, "from_run_id": from.RunID, "from_agent": from.AgentID,
	})
	_, err := s.runs.Deliver(ctx, ask.From.RunID)
	return err
}

// Claim takes the replies to runID that are not delivered yet.
func (s *Service) Claim(ctx context.Context, runID string) ([]runs.ParkDelivery, error) {
	res, err := s.st.DB().ExecContext(ctx, `UPDATE chain_hops SET delivery_state=?, updated_at=?
WHERE kind=? AND to_run_id=? AND delivery_state=?
  AND NOT EXISTS (SELECT 1 FROM chain_hops a WHERE a.kind=? AND a.from_run_id=? AND a.status IN (?,?,?))`,
		DeliveryClaimed, s.stamp(), string(KindReply), runID, DeliveryNone,
		string(KindAsk), runID, string(HopHeld), string(HopQueued), string(HopRunning))
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	hops, err := s.queryHops(ctx, `WHERE kind=? AND to_run_id=? AND delivery_state=? ORDER BY created_at, id`,
		string(KindReply), runID, DeliveryClaimed)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(hops))
	for _, h := range hops {
		ids = append(ids, h.From.AgentID)
	}
	names := s.agentNames(ctx, ids...)
	out := make([]runs.ParkDelivery, 0, len(hops))
	for _, h := range hops {
		out = append(out, runs.ParkDelivery{
			Gate: runs.GateAsk, IDs: []string{h.ID}, At: h.CreatedAt, Reason: runs.ReasonReply,
			Prompt: replyPrompt(names[h.From.AgentID], h.ID, h.From.RunID, h.From.Turn, h.Text),
			HopID:  h.ID, TraceID: h.TraceID, Path: h.pathIDs,
		})
	}
	return out, nil
}

// Settle marks the claimed replies delivered, with the run and turn that
// took them, or dropped when the resume failed.
func (s *Service) Settle(ctx context.Context, _ string, ds []runs.ParkDelivery, res runs.ResumeResult) error {
	set := hopUpdate{"delivery_state": DeliveryDelivered, "to_run_id": res.RunID, "to_turn": res.Turn}
	if res.Err != nil {
		set = hopUpdate{"delivery_state": DeliveryDropped}
	}
	var errs []string
	for _, d := range ds {
		for _, id := range d.IDs {
			if _, err := s.updateHop(ctx, id, set); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("settle replies: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Stale resets reply claims that did not launch and returns the runs with
// replies to deliver.
func (s *Service) Stale(ctx context.Context) ([]string, error) {
	cutoff := s.now().Add(-staleClaim).UTC().Format(time.RFC3339Nano)
	claimed, err := s.queryHops(ctx, `WHERE kind=? AND delivery_state=? AND updated_at<?`,
		string(KindReply), DeliveryClaimed, cutoff)
	if err != nil {
		return nil, err
	}
	for _, h := range claimed {
		n, err := s.runs.TurnAfter(ctx, h.To.RunID, runs.ReasonReply, h.UpdatedAt)
		if err != nil {
			return nil, err
		}
		state := DeliveryNone
		if n > 0 {
			state = DeliveryDelivered
		}
		if _, err := s.updateHop(ctx, h.ID, hopUpdate{"delivery_state": state}); err != nil {
			return nil, err
		}
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT DISTINCT to_run_id FROM chain_hops
WHERE kind=? AND delivery_state=? AND to_run_id<>''`, string(KindReply), DeliveryNone)
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
