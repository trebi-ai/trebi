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

// Delivery is the set of closed resume requests of one parked run that one
// resume carries.
type Delivery struct {
	Run      *runs.Run
	Requests []Request // creation order
}

// deliver resumes the session of runID once every resume request of the run
// is closed. The claim lets only one caller launch.
func (s *Service) deliver(ctx context.Context, runID string) error {
	run, err := s.runs.Get(ctx, runID)
	if err != nil {
		return fmt.Errorf("run %s: %w", runID, err)
	}
	if !run.Status.IsTerminal() {
		return nil
	}
	now := s.now()
	res, err := s.st.DB().ExecContext(ctx, `UPDATE requests SET delivery_state=?, claimed_at=?
WHERE run_id=? AND delivery=? AND status IN (?,?) AND delivery_state=?
  AND NOT EXISTS (SELECT 1 FROM requests p WHERE p.run_id=? AND p.delivery=? AND p.status=?)`,
		string(DeliveryClaimed), fmtTime(now), runID, string(DeliveryResume),
		string(StatusAnswered), string(StatusExpired), string(DeliveryNone),
		runID, string(DeliveryResume), string(StatusPending))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	return s.launchClaimed(ctx, run)
}

// launchClaimed resumes run with its claimed requests and records the result
// on each row.
func (s *Service) launchClaimed(ctx context.Context, run *runs.Run) error {
	claimed, err := s.query(ctx, `WHERE run_id=? AND delivery_state=? ORDER BY created_at, id`,
		run.ID, string(DeliveryClaimed))
	if err != nil || len(claimed) == 0 {
		return err
	}
	d := Delivery{Run: run, Requests: claimed}
	path := "resume"
	var next *runs.Run
	err = runs.ErrNoSession
	if run.SessionID != "" {
		next, err = s.launch.Resume(ctx, run.ID, runs.ResumeOpts{Reason: runs.ReasonRequest, Prompt: d.Prompt()})
	}
	if errors.Is(err, runs.ErrResumeUnsupported) || errors.Is(err, runs.ErrNoSession) {
		s.log.Info("request fresh run", "run_id", run.ID, "reason", err.Error())
		path = "fresh"
		next, err = s.launch.Fresh(ctx, run.ID, d.CapsulePrompt(), runs.ReasonRequest)
	}
	for _, r := range claimed {
		if err != nil {
			s.markDelivery(ctx, r, DeliveryFailed, "", "", err.Error())
			continue
		}
		s.markDelivery(ctx, r, DeliveryDelivered, path, next.ID, "")
	}
	if err != nil {
		return fmt.Errorf("deliver run %s: %w", run.ID, err)
	}
	return nil
}

func (s *Service) markDelivery(ctx context.Context, r Request, st DeliveryState, path, runID, msg string) {
	now := s.now()
	r.DeliveryState, r.DeliveryPath, r.DeliveredRunID, r.DeliveryError = st, path, runID, msg
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
  delivery_error=?, updated_at=?, revision=revision+1 WHERE id=?`,
			string(st), path, runArg, msg, fmtTime(now), r.ID)
		return err
	}, r.event(topic, string(st)))
	if err != nil {
		s.log.Warn("mark delivery", "request_id", r.ID, "err", err)
	}
}

// Recover runs at boot. A claim older than two minutes either launched (a
// request run exists after it) or did not (the claim is reset). Then every
// closed answer that is not delivered goes out.
func (s *Service) Recover(ctx context.Context) error {
	cutoff := s.now().Add(-staleClaim)
	claimed, err := s.query(ctx, `WHERE delivery_state=? ORDER BY created_at, id`, string(DeliveryClaimed))
	if err != nil {
		return err
	}
	for _, r := range claimed {
		if r.ClaimedAt == nil || r.ClaimedAt.After(cutoff) {
			continue
		}
		child, err := s.runs.ResumedChild(ctx, r.RunID, runs.ReasonRequest, *r.ClaimedAt)
		if err != nil {
			return err
		}
		if child != "" {
			s.markDelivery(ctx, r, DeliveryDelivered, "resume", child, "")
			continue
		}
		if _, err := s.st.DB().ExecContext(ctx, `UPDATE requests SET delivery_state=?, claimed_at=NULL WHERE id=? AND delivery_state=?`,
			string(DeliveryNone), r.ID, string(DeliveryClaimed)); err != nil {
			return err
		}
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT DISTINCT run_id FROM requests WHERE delivery=? AND status IN (?,?) AND delivery_state=?`,
		string(DeliveryResume), string(StatusAnswered), string(StatusExpired), string(DeliveryNone))
	if err != nil {
		return err
	}
	var runIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		runIDs = append(runIDs, id)
	}
	rows.Close()
	for _, id := range runIDs {
		if err := s.deliver(ctx, id); err != nil {
			s.log.Warn("request recover", "run_id", id, "err", err)
		}
	}
	return rows.Err()
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
		a := &Answer{Decision: DecisionExpired, By: ViaDaemon, Via: ViaDaemon, At: now}
		closed, err := s.close(ctx, r, StatusExpired, a)
		if errors.Is(err, ErrClosed) || errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return err
		}
		s.afterClose(ctx, closed)
	}
	old, err := s.query(ctx, `WHERE status<>? AND report_kind<>'' AND updated_at<?`,
		string(StatusPending), fmtTime(now.Add(-s.opts.Retain)))
	if err != nil {
		return err
	}
	for _, r := range old {
		if err := s.reports.Remove(r.ID); err != nil {
			return err
		}
		if _, err := s.st.DB().ExecContext(ctx, `UPDATE requests SET report_kind='' WHERE id=?`, r.ID); err != nil {
			return err
		}
	}
	return nil
}
