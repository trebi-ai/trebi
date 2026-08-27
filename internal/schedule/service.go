// Package schedule maintains durable due-time rows derived from job triggers.
package schedule

import (
	"context"
	"database/sql"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
)

// Service owns schedules table rows.
type Service struct {
	st *store.Store
}

// New creates a schedule service.
func New(st *store.Store) *Service {
	return &Service{st: st}
}

// RecomputeFromJob rebuilds schedule rows for one job from its triggers.
func (s *Service) RecomputeFromJob(ctx context.Context, workspaceID string, j *job.Job, cutoverStartedAt time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.st.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM schedules WHERE workspace_id=? AND job=?`, workspaceID, j.Name); err != nil {
			return err
		}
		// schedules PK is (workspace, job, idx); idx from B21 schedule family (cron+every).
		for _, tr := range j.Triggers {
			if tr.Family != "cron" && tr.Family != "every" {
				continue
			}
			if tr.Disabled {
				continue // index still reserved on the job; row omitted until re-enabled
			}
			spec := tr.Cron
			catchUp := 1
			if tr.Family == "every" {
				spec = "@every " + tr.Every
				catchUp = 0
			}
			tz := tr.Timezone
			if tz == "" {
				tz = "UTC"
			}
			next := nextDue(spec, cutoverStartedAt)
			var nextStr *string
			if !next.IsZero() {
				s := next.UTC().Format(time.RFC3339)
				nextStr = &s
			}
			var anchor *string
			if tr.Anchor != "" {
				anchor = &tr.Anchor
			}
			if _, err := tx.Exec(`INSERT INTO schedules (workspace_id, job, idx, spec, timezone, catch_up, next_due_at, anchor, updated_at)
				VALUES (?,?,?,?,?,?,?,?,?)`,
				workspaceID, j.Name, tr.Index, spec, tz, catchUp, nextStr, anchor, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// NextDuePublic is the exported next-due calculator (daemon schedule fire).
func NextDuePublic(spec string, from time.Time) time.Time { return nextDue(spec, from) }

func nextDue(spec string, from time.Time) time.Time {
	if from.IsZero() {
		from = time.Now().UTC()
	}
	if len(spec) > 7 && spec[:7] == "@every " {
		d, err := time.ParseDuration(spec[7:])
		if err != nil || d <= 0 {
			return time.Time{}
		}
		return from.Add(d)
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse(spec)
	if err != nil {
		return time.Time{}
	}
	return sched.Next(from)
}

// ClaimDue atomically claims schedules with next_due_at <= now by advancing next_due_at.
func (s *Service) ClaimDue(ctx context.Context, now time.Time, limit int) ([]Row, error) {
	if limit <= 0 {
		limit = 10
	}
	nowStr := now.UTC().Format(time.RFC3339)
	var out []Row
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`
			SELECT workspace_id, job, idx, spec, timezone, catch_up, next_due_at, last_fired_at
			FROM schedules WHERE next_due_at IS NOT NULL AND next_due_at <= ?
			ORDER BY next_due_at ASC LIMIT ?`, nowStr, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		var candidates []Row
		for rows.Next() {
			var r Row
			var next, last sql.NullString
			var catch int
			if err := rows.Scan(&r.WorkspaceID, &r.Job, &r.Idx, &r.Spec, &r.Timezone, &catch, &next, &last); err != nil {
				return err
			}
			r.CatchUp = catch == 1
			r.NextDueAt = next.String
			r.LastFiredAt = last.String
			candidates = append(candidates, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range candidates {
			// Tentative next so another worker cannot re-claim until MarkFired.
			next := nextDue(r.Spec, now)
			var nextStr *string
			if !next.IsZero() {
				s := next.UTC().Format(time.RFC3339)
				nextStr = &s
			}
			res, err := tx.Exec(`UPDATE schedules SET next_due_at=?, updated_at=?
				WHERE workspace_id=? AND job=? AND idx=? AND next_due_at=?`,
				nextStr, nowStr, r.WorkspaceID, r.Job, r.Idx, r.NextDueAt)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n == 1 {
				out = append(out, r)
			}
		}
		return nil
	})
	return out, err
}

// Row is a schedules table row.
type Row struct {
	WorkspaceID string
	Job         string
	Idx         int
	Spec        string
	Timezone    string
	CatchUp     bool
	NextDueAt   string
	LastFiredAt string
}

// MarkFired updates last_fired_at and next_due_at after a fire.
func (s *Service) MarkFired(ctx context.Context, r Row, firedAt time.Time) error {
	next := nextDue(r.Spec, firedAt)
	now := firedAt.UTC().Format(time.RFC3339)
	var nextStr *string
	if !next.IsZero() {
		s := next.UTC().Format(time.RFC3339)
		nextStr = &s
	}
	_, err := s.st.DB().ExecContext(ctx, `UPDATE schedules SET last_fired_at=?, next_due_at=?, updated_at=? WHERE workspace_id=? AND job=? AND idx=?`,
		now, nextStr, now, r.WorkspaceID, r.Job, r.Idx)
	return err
}
