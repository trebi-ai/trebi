package requests

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/trebi-ai/trebi/internal/runs"
)

// staleClaim is the age after which Recover treats a claim as lost.
const staleClaim = 2 * time.Minute

// Delivery is the set of closed requests of one run that one prompt carries.
type Delivery struct {
	Run      *runs.Run
	Requests []Request // creation order
}

// Claim takes the closed requests of runID that are not delivered. The claim
// lets only one caller launch. It records the turn of the run at the claim,
// so Stale can tell whether a later turn carried the answers.
func (s *Service) Claim(ctx context.Context, runID string) ([]runs.ParkDelivery, error) {
	run, err := s.runs.Get(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", runID, err)
	}
	now := s.now()
	res, err := s.st.DB().ExecContext(ctx, `UPDATE requests SET delivery_state=?, claimed_at=?, delivered_turn=?
WHERE run_id=? AND status IN (?,?) AND delivery_state=?
  AND NOT EXISTS (SELECT 1 FROM requests p WHERE p.run_id=? AND p.status=?)`,
		string(DeliveryClaimed), fmtTime(now), run.TurnCount, runID,
		string(StatusAnswered), string(StatusExpired), string(DeliveryNone),
		runID, string(StatusPending))
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	claimed, err := s.query(ctx, `WHERE run_id=? AND delivery_state=? ORDER BY created_at, id`,
		runID, string(DeliveryClaimed))
	if err != nil || len(claimed) == 0 {
		return nil, err
	}
	d := Delivery{Run: run, Requests: claimed}
	pd := runs.ParkDelivery{Gate: runs.GateRequest, Prompt: d.Prompt(), Capsule: d.CapsulePrompt(),
		At: claimed[0].CreatedAt, Reason: runs.ReasonRequest}
	for _, r := range claimed {
		pd.IDs = append(pd.IDs, r.ID)
	}
	return []runs.ParkDelivery{pd}, nil
}

// Settle records the resume result on each claimed request.
func (s *Service) Settle(ctx context.Context, _ string, ds []runs.ParkDelivery, res runs.ResumeResult) error {
	for _, d := range ds {
		for _, id := range d.IDs {
			r, err := s.Get(ctx, id)
			if err != nil {
				return err
			}
			if res.Err != nil {
				s.markDelivery(ctx, r, DeliveryFailed, "", "", res.Err.Error())
				continue
			}
			r.DeliveredTurn = res.Turn
			s.markDelivery(ctx, r, DeliveryDelivered, res.Path, res.RunID, "")
		}
	}
	return nil
}

func (s *Service) markDelivery(ctx context.Context, r Request, st DeliveryState, path, runID, msg string) {
	now := s.now()
	r.DeliveryState, r.DeliveryPath, r.DeliveredRunID, r.DeliveryError = st, path, runID, msg
	var turnArg any
	if st == DeliveryDelivered && r.DeliveredTurn > 0 {
		turnArg = r.DeliveredTurn
	}
	r.Revision++
	topic := "request.delivered"
	if st == DeliveryFailed {
		topic = "request.delivery_failed"
	}
	var runArg any
	if runID != "" {
		runArg = runID
	}
	_, err := s.st.AppendEntity(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE requests SET delivery_state=?, delivery_path=?, delivered_run_id=?,
  delivery_error=?, delivered_turn=COALESCE(?, delivered_turn), updated_at=?, revision=revision+1 WHERE id=?`,
			string(st), path, runArg, msg, turnArg, fmtTime(now), r.ID)
		return err
	}, r.event(topic, string(st)))
	if err != nil {
		s.log.Warn("mark delivery", "request_id", r.ID, "err", err)
	}
}

// Stale runs at boot. A claim older than two minutes either launched (the
// run has a turn after the turn of the claim) or did not (the claim is
// reset). It returns the runs with closed answers that are not delivered.
func (s *Service) Stale(ctx context.Context) ([]string, error) {
	cutoff := s.now().Add(-staleClaim)
	claimed, err := s.query(ctx, `WHERE delivery_state=? ORDER BY created_at, id`, string(DeliveryClaimed))
	if err != nil {
		return nil, err
	}
	for _, r := range claimed {
		if r.ClaimedAt == nil || r.ClaimedAt.After(cutoff) {
			continue
		}
		run, err := s.runs.Get(ctx, r.RunID)
		if err != nil {
			return nil, err
		}
		if run.TurnCount > r.DeliveredTurn {
			r.DeliveredTurn = r.DeliveredTurn + 1
			s.markDelivery(ctx, r, DeliveryDelivered, "resume", r.RunID, "")
			continue
		}
		if _, err := s.st.DB().ExecContext(ctx, `UPDATE requests SET delivery_state=?, claimed_at=NULL, delivered_turn=NULL WHERE id=? AND delivery_state=?`,
			string(DeliveryNone), r.ID, string(DeliveryClaimed)); err != nil {
			return nil, err
		}
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT DISTINCT run_id FROM requests WHERE status IN (?,?) AND delivery_state=?`,
		string(StatusAnswered), string(StatusExpired), string(DeliveryNone))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		runIDs = append(runIDs, id)
	}
	return runIDs, rows.Err()
}

// Sweep expires pending requests past expires_at and removes old report
// snapshots. The daemon calls it each minute.
func (s *Service) Sweep(ctx context.Context) error {
	now := s.now()
	due, err := s.query(ctx, `WHERE status=? AND expires_at IS NOT NULL AND expires_at<? ORDER BY created_at, id`,
		string(StatusPending), fmtTime(now))
	if err != nil {
		return err
	}
	for _, r := range due {
		closed, err := s.close(ctx, r, StatusExpired, &Answered{By: ViaDaemon, Via: ViaDaemon, At: now}, nil)
		if errors.Is(err, ErrClosed) || errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return err
		}
		s.afterClose(ctx, closed)
	}
	old, err := s.query(ctx, `WHERE status<>? AND updated_at<?
  AND EXISTS (SELECT 1 FROM request_forms f WHERE f.request_id = requests.id AND f.report_kind<>'')`,
		string(StatusPending), fmtTime(now.Add(-s.opts.Retain)))
	if err != nil {
		return err
	}
	for _, r := range old {
		if err := s.reports.Remove(r.ID); err != nil {
			return err
		}
		if _, err := s.st.DB().ExecContext(ctx, `UPDATE request_forms SET report_kind='' WHERE request_id=?`, r.ID); err != nil {
			return err
		}
	}
	return nil
}
