// Package schedule maintains durable due-time rows derived from job triggers.
package schedule

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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
// Unchanged (workspace, job, idx, spec, timezone) rows keep next_due_at and
// last_fired_at so a periodic index rebuild cannot push @every into the future.
func (s *Service) RecomputeFromJob(ctx context.Context, workspaceID string, j *job.Job, cutoverStartedAt time.Time) error {
	report, hasReport := reportSpec(j)
	if !j.Disabled {
		existing, err := loadRowsDB(ctx, s.st.DB(), workspaceID, j.Name)
		if err != nil {
			return err
		}
		if scheduleSpecUnchanged(existing, j) && (!hasReport || reportUnchanged(existing, report)) {
			return nil
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		if j.Disabled {
			_, err := tx.Exec(`DELETE FROM schedules WHERE workspace_id=? AND job=?`, workspaceID, j.Name)
			return err
		}

		existing, err := loadRows(tx, workspaceID, j.Name)
		if err != nil {
			return err
		}

		keep := map[int]bool{}
		for _, tr := range j.Triggers {
			if tr.Family != "cron" && tr.Family != "every" {
				continue
			}
			if tr.Disabled {
				continue
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
			keep[tr.Index] = true

			if old, ok := existing[tr.Index]; ok && old.Spec == spec && old.Timezone == tz {
				continue
			}

			next := nextDue(spec, cutoverStartedAt, tz)
			var nextStr *string
			if !next.IsZero() {
				s := next.UTC().Format(time.RFC3339)
				nextStr = &s
			}
			if _, ok := existing[tr.Index]; ok {
				if _, err := tx.Exec(`UPDATE schedules SET spec=?, timezone=?, catch_up=?, next_due_at=?, last_fired_at=NULL, anchor=NULL, updated_at=?
					WHERE workspace_id=? AND job=? AND idx=?`,
					spec, tz, catchUp, nextStr, now, workspaceID, j.Name, tr.Index); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(`INSERT INTO schedules (workspace_id, job, idx, spec, timezone, catch_up, next_due_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?)`,
				workspaceID, j.Name, tr.Index, spec, tz, catchUp, nextStr, now); err != nil {
				return err
			}
		}
		for idx := range existing {
			if idx == -1 && hasReport {
				continue
			}
			if keep[idx] {
				continue
			}
			if _, err := tx.Exec(`DELETE FROM schedules WHERE workspace_id=? AND job=? AND idx=?`,
				workspaceID, j.Name, idx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if hasReport && !j.Disabled {
		return s.upsertReport(ctx, workspaceID, j.Name, report)
	}
	return nil
}

func reportSpec(j *job.Job) (string, bool) {
	if j == nil || j.Report == nil {
		return "", false
	}
	if j.Report.Cron != "" {
		return j.Report.Cron, true
	}
	if j.Report.Every != "" {
		return "@every " + j.Report.Every, true
	}
	return "", false
}

func reportUnchanged(rows map[int]Row, spec string) bool {
	old, ok := rows[-1]
	return ok && old.Spec == spec
}

func (s *Service) upsertReport(ctx context.Context, workspaceID, jobName, spec string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.st.WithTx(ctx, func(tx *sql.Tx) error {
		next := nextDue(spec, time.Time{}, "UTC")
		var nextStr *string
		if !next.IsZero() {
			value := next.UTC().Format(time.RFC3339)
			nextStr = &value
		}
		var oldSpec string
		err := tx.QueryRow(`SELECT spec FROM schedules WHERE workspace_id=? AND job=? AND idx=-1`,
			workspaceID, jobName).Scan(&oldSpec)
		switch {
		case err == nil:
			if oldSpec == spec {
				return nil
			}
			_, err = tx.Exec(`UPDATE schedules SET spec=?, next_due_at=?, last_fired_at=NULL, updated_at=?
				WHERE workspace_id=? AND job=? AND idx=-1`, spec, nextStr, now, workspaceID, jobName)
			return err
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.Exec(`INSERT INTO schedules (workspace_id, job, idx, spec, timezone, catch_up, next_due_at, updated_at)
				VALUES (?,?,-1,?,'UTC',1,?,?)`, workspaceID, jobName, spec, nextStr, now)
			return err
		default:
			return err
		}
	})
}

const scheduleCols = `workspace_id, job, idx, spec, timezone, catch_up, next_due_at, last_fired_at`

func scanSchedule(rows *sql.Rows) (Row, error) {
	var r Row
	var next, last sql.NullString
	var catch int
	if err := rows.Scan(&r.WorkspaceID, &r.Job, &r.Idx, &r.Spec, &r.Timezone, &catch, &next, &last); err != nil {
		return Row{}, err
	}
	r.CatchUp = catch == 1
	r.NextDueAt = next.String
	r.LastFiredAt = last.String
	return r, nil
}

func loadRows(tx *sql.Tx, workspaceID, jobName string) (map[int]Row, error) {
	rows, err := tx.Query(`SELECT `+scheduleCols+`
		FROM schedules WHERE workspace_id=? AND job=?`, workspaceID, jobName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]Row{}
	for rows.Next() {
		r, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out[r.Idx] = r
	}
	return out, rows.Err()
}

func loadRowsDB(ctx context.Context, db *sql.DB, workspaceID, jobName string) (map[int]Row, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+scheduleCols+`
		FROM schedules WHERE workspace_id=? AND job=?`, workspaceID, jobName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]Row{}
	for rows.Next() {
		r, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out[r.Idx] = r
	}
	return out, rows.Err()
}

func scheduleSpecUnchanged(existing map[int]Row, j *job.Job) bool {
	want := 0
	for _, tr := range j.Triggers {
		if tr.Family != "cron" && tr.Family != "every" {
			continue
		}
		if tr.Disabled {
			continue
		}
		spec := tr.Cron
		if tr.Family == "every" {
			spec = "@every " + tr.Every
		}
		tz := tr.Timezone
		if tz == "" {
			tz = "UTC"
		}
		old, ok := existing[tr.Index]
		if !ok || old.Spec != spec || old.Timezone != tz {
			return false
		}
		want++
	}
	return want == len(existing)
}

func nextDue(spec string, from time.Time, tz string) time.Time {
	loc := time.UTC
	if tz != "" && !strings.EqualFold(tz, "UTC") {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	if from.IsZero() {
		from = time.Now().In(loc)
	} else {
		from = from.In(loc)
	}
	if len(spec) > 7 && spec[:7] == "@every " {
		d, err := time.ParseDuration(spec[7:])
		if err != nil || d <= 0 {
			return time.Time{}
		}
		return from.Add(d).UTC()
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse(spec)
	if err != nil {
		return time.Time{}
	}
	return sched.Next(from).UTC()
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
			SELECT `+scheduleCols+`
			FROM schedules WHERE next_due_at IS NOT NULL AND next_due_at <= ?
			ORDER BY next_due_at ASC LIMIT ?`, nowStr, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		var candidates []Row
		for rows.Next() {
			r, err := scanSchedule(rows)
			if err != nil {
				return err
			}
			candidates = append(candidates, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range candidates {
			next := nextDue(r.Spec, now, r.Timezone)
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
	next := nextDue(r.Spec, firedAt, r.Timezone)
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

// ListOpts filters the schedule list.
type ListOpts struct {
	Workspace string
	// DueBefore keeps rows due at or before this RFC3339 time.
	DueBefore string
	Limit     int
}

// List returns schedule rows ordered by next_due_at.
func (s *Service) List(ctx context.Context, opts ListOpts) ([]Row, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	q := `SELECT ` + scheduleCols + `
		 FROM schedules WHERE 1=1`
	var args []any
	if opts.Workspace != "" {
		q += ` AND workspace_id = ?`
		args = append(args, opts.Workspace)
	}
	if opts.DueBefore != "" {
		q += ` AND next_due_at IS NOT NULL AND next_due_at <= ?`
		args = append(args, opts.DueBefore)
	}
	q += ` ORDER BY next_due_at LIMIT ?`
	args = append(args, opts.Limit)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Row
	for rows.Next() {
		r, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

// Delete removes one schedule row (orphan job).
func (s *Service) Delete(ctx context.Context, workspaceID, jobName string, idx int) error {
	_, err := s.st.DB().ExecContext(ctx,
		`DELETE FROM schedules WHERE workspace_id=? AND job=? AND idx=?`, workspaceID, jobName, idx)
	return err
}
