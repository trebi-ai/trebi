package runs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// pruneBatch is the count of runs that one prune transaction changes.
const pruneBatch = 500

// finishedStatuses are the runs that keep_runs counts. A running or a
// parked run is never counted and never pruned.
const finishedStatuses = `'complete','failed','exited','skipped','timed_out'`

// Retention is the keep_runs policy: the finished runs of each job or agent
// that keep their details. 0 keeps all.
type Retention struct {
	Default int
	// Jobs holds the job and agent ids that set keep_runs.
	Jobs map[string]int
}

// For gives the keep count of one job or agent id.
func (r Retention) For(jobID string) int {
	if n, ok := r.Jobs[jobID]; ok {
		return n
	}
	return r.Default
}

// least is the smallest count that keeps less than all, or 0 when every
// count keeps all.
func (r Retention) least() int {
	least := r.Default
	for _, n := range r.Jobs {
		if n > 0 && (least == 0 || n < least) {
			least = n
		}
	}
	return least
}

// sessionPurger is the part of the session host that removes an ended
// session and its vendor files.
type sessionPurger interface {
	Purge(ctx context.Context, p agentvisor.PurgeSpec) error
}

// pruneRow is one run past the keep count of its job.
type pruneRow struct {
	id, jobID, sessionID, harness, visorID, pane, conversation, startedAt string
}

// hasSession reports if the run has files in the session host.
func (r pruneRow) hasSession() bool {
	return r.visorID != "" || r.pane != "" || (r.harness != "" && r.sessionID != "")
}

// PruneHistory removes the details of the finished runs past the keep count
// of their job or agent: the run events, the run log, the private files, and
// the agent-visor session with its vendor files. The run row, its tokens,
// turns, and segments stay, with pruned_at set. A second pass does no work.
func (s *Service) PruneHistory(ctx context.Context, r Retention, now time.Time) (int, error) {
	least := r.least()
	if least == 0 {
		return 0, nil
	}
	rows, err := s.pruneCandidates(ctx, r, least, now)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	purger, _ := s.host.(sessionPurger)
	s.mu.Unlock()
	pruned := 0
	for start := 0; start < len(rows); start += pruneBatch {
		batch := rows[start:min(start+pruneBatch, len(rows))]
		done := make([]string, 0, len(batch))
		for _, row := range batch {
			if purger == nil && row.hasSession() {
				continue // a later pass, with the session host set
			}
			if err := s.pruneFiles(ctx, purger, row, batch); err != nil {
				s.log.Warn("run prune", "run_id", row.id, "err", err)
				continue
			}
			done = append(done, row.id)
		}
		if err := s.markPruned(ctx, done, now); err != nil {
			return pruned, err
		}
		pruned += len(done)
	}
	if pruned > 0 {
		if err := s.st.IncrementalVacuum(ctx); err != nil {
			return pruned, fmt.Errorf("incremental vacuum: %w", err)
		}
	}
	return pruned, nil
}

// pruneCandidates lists the runs that are not pruned and are past the keep
// count of their job. A run in its resumable window waits for a later pass.
func (s *Service) pruneCandidates(ctx context.Context, r Retention, least int, now time.Time) ([]pruneRow, error) {
	q, err := s.st.DB().QueryContext(ctx, `
SELECT id, job_id, COALESCE(session_id,''), COALESCE(harness,''), COALESCE(visor_session_id,''),
	COALESCE(pane_session,''), COALESCE(conversation_key,''), started_at, COALESCE(ended_at,''), resumable_ms, n
FROM (SELECT runs.*, ROW_NUMBER() OVER (PARTITION BY job_id ORDER BY started_at DESC, id DESC) AS n
	FROM runs WHERE status IN (`+finishedStatuses+`)) ranked
WHERE pruned_at IS NULL AND n > ?
ORDER BY started_at, id`, least)
	if err != nil {
		return nil, fmt.Errorf("prune candidates: %w", err)
	}
	defer q.Close() //nolint:errcheck // read-only rows
	var out []pruneRow
	for q.Next() {
		var row pruneRow
		var endedAt string
		var resumableMS int64
		var n int
		if err := q.Scan(&row.id, &row.jobID, &row.sessionID, &row.harness, &row.visorID,
			&row.pane, &row.conversation, &row.startedAt, &endedAt, &resumableMS, &n); err != nil {
			return nil, err
		}
		if keep := r.For(row.jobID); keep == 0 || n <= keep {
			continue
		}
		if resumableMS > 0 && endedAt != "" && store.ParseTS(endedAt).Add(time.Duration(resumableMS)*time.Millisecond).After(now) {
			continue
		}
		out = append(out, row)
	}
	return out, q.Err()
}

// pruneFiles removes the files of one run. It keeps the vendor files when
// another run can still resume the vendor session. purger is nil only for a
// run with no session.
func (s *Service) pruneFiles(ctx context.Context, purger sessionPurger, row pruneRow, batch []pruneRow) error {
	if row.hasSession() {
		vendor := agentvisor.PurgeSpec{Harness: row.harness, VendorID: row.sessionID}
		if row.harness == "" || row.sessionID == "" {
			vendor = agentvisor.PurgeSpec{}
		} else if keep, err := s.vendorSessionKept(ctx, row, batch); err != nil {
			return err
		} else if keep {
			vendor = agentvisor.PurgeSpec{}
		}
		for _, id := range []string{row.visorID, row.pane} {
			if id == "" {
				continue
			}
			spec := vendor
			spec.Session = id
			if err := purger.Purge(ctx, spec); err != nil && !errors.Is(err, agentvisor.ErrNotFound) {
				return fmt.Errorf("purge session %s: %w", id, err)
			}
			vendor = agentvisor.PurgeSpec{}
		}
		if vendor.VendorID != "" {
			if err := purger.Purge(ctx, vendor); err != nil && !errors.Is(err, agentvisor.ErrNotFound) {
				return fmt.Errorf("purge vendor session %s: %w", vendor.VendorID, err)
			}
		}
	}
	var errs []error
	for _, path := range []string{filepath.Join(home.RunsLogDir(), row.id+".log"), home.RunDir(row.id)} {
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, runFiles(row.id).Remove())
	return errors.Join(errs...)
}

// vendorSessionKept reports if a run that is not pruned uses the vendor
// session of row, or if row is the last run of its conversation, which a
// follow-up can resume.
func (s *Service) vendorSessionKept(ctx context.Context, row pruneRow, batch []pruneRow) (bool, error) {
	ids := make([]any, 0, len(batch)+1)
	ids = append(ids, row.sessionID)
	for _, b := range batch {
		ids = append(ids, b.id)
	}
	var shared bool
	err := s.st.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE session_id=? AND pruned_at IS NULL
		AND id NOT IN (`+strings.Repeat("?,", len(batch)-1)+`?))`, ids...).Scan(&shared)
	if err != nil || shared || row.conversation == "" {
		return shared, err
	}
	var later bool
	err = s.st.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE conversation_key=? AND started_at > ?)`,
		row.conversation, row.startedAt).Scan(&later)
	return !later, err
}

// markPruned deletes the run events of ids and sets their pruned_at. It is
// housekeeping, so it does not bump the revision or write the bus.
func (s *Service) markPruned(ctx context.Context, ids []string, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	in := `(` + strings.Repeat("?,", len(ids)-1) + `?)`
	return s.st.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM run_events WHERE run_id IN `+in, args...); err != nil {
			return fmt.Errorf("delete run events: %w", err)
		}
		_, err := tx.ExecContext(ctx, `UPDATE runs SET pruned_at=? WHERE id IN `+in, append([]any{now.UTC().Format(time.RFC3339Nano)}, args...)...)
		return err
	})
}
