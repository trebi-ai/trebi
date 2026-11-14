// Package schedule maintains durable due-time rows derived from job triggers.
package schedule

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
)

// Service owns schedules table rows.
type Service struct {
	st        *store.Store
	defaultTZ func() string
}

// Option configures a Service.
type Option func(*Service)

// WithDefaultTimeZone sets the source of the zone for a trigger that names
// none. It runs on each use, so a later change shows. Without it, or when it
// returns "", the zone is UTC.
func WithDefaultTimeZone(f func() string) Option {
	return func(s *Service) { s.defaultTZ = f }
}

// New creates a schedule service.
func New(st *store.Store, opts ...Option) *Service {
	s := &Service{st: st}
	for _, o := range opts {
		o(s)
	}
	return s
}

// RecomputeDefaultZone moves the next due time of each cron row that follows
// the default zone. Call it when the default zone changes.
func (s *Service) RecomputeDefaultZone(ctx context.Context) error {
	now := time.Now().UTC()
	return s.st.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT ` + scheduleCols + ` FROM schedules WHERE timezone = '' AND spec NOT LIKE '@every %'`)
		if err != nil {
			return err
		}
		var list []Row
		for rows.Next() {
			r, err := scanSchedule(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			list = append(list, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, r := range list {
			var next *string
			if t := s.due(r.Spec, now, ""); !t.IsZero() {
				v := t.Format(time.RFC3339)
				next = &v
			}
			if _, err := tx.Exec(`UPDATE schedules SET next_due_at=?, updated_at=?
				WHERE workspace_id=? AND job_id=? AND idx=?`,
				next, now.Format(time.RFC3339), r.WorkspaceID, r.JobID, r.Idx); err != nil {
				return err
			}
		}
		return nil
	})
}

// RecomputeFromJob rebuilds schedule rows for one job from its triggers.
// Unchanged (workspace, job ID, idx, spec, timezone) rows keep next_due_at and
// last_fired_at so a periodic index rebuild cannot push @every into the future.
func (s *Service) RecomputeFromJob(ctx context.Context, workspaceID string, j *job.Job, cutoverStartedAt time.Time) error {
	report, hasReport := reportSpec(j)
	if !j.Disabled {
		existing, err := loadRowsDB(ctx, s.st.DB(), workspaceID, j.ID)
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
			_, err := tx.Exec(`DELETE FROM schedules WHERE workspace_id=? AND job_id=?`, workspaceID, j.ID)
			return err
		}

		existing, err := loadRows(tx, workspaceID, j.ID)
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
			keep[tr.Index] = true

			if old, ok := existing[tr.Index]; ok && old.Spec == spec && old.Timezone == tz {
				continue
			}

			next := s.due(spec, cutoverStartedAt, tz)
			var nextStr *string
			if !next.IsZero() {
				s := next.UTC().Format(time.RFC3339)
				nextStr = &s
			}
			if _, ok := existing[tr.Index]; ok {
				if _, err := tx.Exec(`UPDATE schedules SET spec=?, timezone=?, catch_up=?, next_due_at=?, last_fired_at=NULL, anchor=NULL, updated_at=?
					WHERE workspace_id=? AND job_id=? AND idx=?`,
					spec, tz, catchUp, nextStr, now, workspaceID, j.ID, tr.Index); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(`INSERT INTO schedules (workspace_id, job_id, idx, spec, timezone, catch_up, next_due_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?)`,
				workspaceID, j.ID, tr.Index, spec, tz, catchUp, nextStr, now); err != nil {
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
			if _, err := tx.Exec(`DELETE FROM schedules WHERE workspace_id=? AND job_id=? AND idx=?`,
				workspaceID, j.ID, idx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if hasReport && !j.Disabled {
		return s.upsertReport(ctx, workspaceID, j.ID, report)
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

const scheduleCols = `workspace_id, job_id, COALESCE((SELECT ji.name FROM job_index ji WHERE ji.id = schedules.job_id), ''), idx, spec, timezone, catch_up, next_due_at, last_fired_at, artifact_id`

func scanSchedule(rows *sql.Rows) (Row, error) {
	var r Row
	var next, last sql.NullString
	var catch int
	if err := rows.Scan(&r.WorkspaceID, &r.JobID, &r.JobName, &r.Idx, &r.Spec, &r.Timezone, &catch, &next, &last, &r.ArtifactID); err != nil {
		return Row{}, err
	}
	r.CatchUp = catch == 1
	r.NextDueAt = next.String
	r.LastFiredAt = last.String
	return r, nil
}

func loadRows(tx *sql.Tx, workspaceID, jobID string) (map[int]Row, error) {
	rows, err := tx.Query(`SELECT `+scheduleCols+`
		FROM schedules WHERE workspace_id=? AND job_id=?`, workspaceID, jobID)
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

func loadRowsDB(ctx context.Context, db *sql.DB, workspaceID, jobID string) (map[int]Row, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+scheduleCols+`
		FROM schedules WHERE workspace_id=? AND job_id=?`, workspaceID, jobID)
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
		old, ok := existing[tr.Index]
		if !ok || old.Spec != spec || old.Timezone != tr.Timezone {
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
			AND workspace_id NOT IN (SELECT id FROM workspaces WHERE archived_at IS NOT NULL)
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
			next := s.due(r.Spec, now, r.Timezone)
			var nextStr *string
			if !next.IsZero() {
				s := next.UTC().Format(time.RFC3339)
				nextStr = &s
			}
			res, err := tx.Exec(`UPDATE schedules SET next_due_at=?, updated_at=?
				WHERE workspace_id=? AND job_id=? AND idx=? AND next_due_at=?`,
				nextStr, nowStr, r.WorkspaceID, r.JobID, r.Idx, r.NextDueAt)
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
	JobID       string
	JobName     string
	Idx         int
	Spec        string
	Timezone    string
	CatchUp     bool
	NextDueAt   string
	LastFiredAt string
	ArtifactID  string // a live artifact refresh; JobID is the artifact id
}

// MarkFired updates last_fired_at and next_due_at after a fire.
func (s *Service) MarkFired(ctx context.Context, r Row, firedAt time.Time) error {
	next := s.due(r.Spec, firedAt, r.Timezone)
	now := firedAt.UTC().Format(time.RFC3339)
	var nextStr *string
	if !next.IsZero() {
		s := next.UTC().Format(time.RFC3339)
		nextStr = &s
	}
	_, err := s.st.DB().ExecContext(ctx, `UPDATE schedules SET last_fired_at=?, next_due_at=?, updated_at=? WHERE workspace_id=? AND job_id=? AND idx=?`,
		now, nextStr, now, r.WorkspaceID, r.JobID, r.Idx)
	return err
}

// DueNow makes one schedule due at once, so that the next tick fires it.
func (s *Service) DueNow(ctx context.Context, workspaceID, jobID string, idx int, now time.Time) error {
	ts := now.UTC().Format(time.RFC3339)
	_, err := s.st.DB().ExecContext(ctx, `UPDATE schedules SET next_due_at=?, updated_at=? WHERE workspace_id=? AND job_id=? AND idx=?`,
		ts, ts, workspaceID, jobID, idx)
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
		r.Timezone = s.zone(r.Timezone)
		list = append(list, r)
	}
	return list, rows.Err()
}

// Delete removes one schedule row (orphan job).
func (s *Service) Delete(ctx context.Context, workspaceID, jobID string, idx int) error {
	_, err := s.st.DB().ExecContext(ctx,
		`DELETE FROM schedules WHERE workspace_id=? AND job_id=? AND idx=?`, workspaceID, jobID, idx)
	return err
}

func (s *Service) upsertReport(ctx context.Context, workspaceID, jobID, spec string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.st.WithTx(ctx, func(tx *sql.Tx) error {
		next := s.due(spec, time.Time{}, "")
		var nextStr *string
		if !next.IsZero() {
			value := next.UTC().Format(time.RFC3339)
			nextStr = &value
		}
		var oldSpec string
		err := tx.QueryRow(`SELECT spec FROM schedules WHERE workspace_id=? AND job_id=? AND idx=-1`,
			workspaceID, jobID).Scan(&oldSpec)
		switch {
		case err == nil:
			if oldSpec == spec {
				return nil
			}
			_, err = tx.Exec(`UPDATE schedules SET spec=?, next_due_at=?, last_fired_at=NULL, updated_at=?
				WHERE workspace_id=? AND job_id=? AND idx=-1`, spec, nextStr, now, workspaceID, jobID)
			return err
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.Exec(`INSERT INTO schedules (workspace_id, job_id, idx, spec, timezone, catch_up, next_due_at, updated_at)
				VALUES (?,?,-1,?,'',1,?,?)`, workspaceID, jobID, spec, nextStr, now)
			return err
		default:
			return err
		}
	})
}

// ParseSpec checks a schedule: "@every <duration>" or a 5-field cron.
func ParseSpec(spec string) error {
	if d, ok := strings.CutPrefix(spec, "@every "); ok {
		v, err := time.ParseDuration(d)
		if err != nil || v < time.Minute {
			return fmt.Errorf("%q: every needs a duration of 1m or more, such as 24h", d)
		}
		return nil
	}
	if _, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(spec); err != nil {
		return fmt.Errorf("%q: %w", spec, err)
	}
	return nil
}

// SetArtifact writes the one schedule row of a live artifact. The row uses
// the artifact id as its job id.
func (s *Service) SetArtifact(ctx context.Context, workspaceID, artifactID, spec, tz string) error {
	if err := ParseSpec(spec); err != nil {
		return err
	}
	now := time.Now().UTC()
	var nextStr *string
	if next := s.due(spec, now, tz); !next.IsZero() {
		v := next.Format(time.RFC3339)
		nextStr = &v
	}
	_, err := s.st.DB().ExecContext(ctx, `INSERT INTO schedules (workspace_id, job_id, idx, spec, timezone, catch_up, next_due_at,
  updated_at, artifact_id) VALUES (?,?,0,?,?,0,?,?,?)
  ON CONFLICT (workspace_id, job_id, idx) DO UPDATE SET spec=excluded.spec, timezone=excluded.timezone,
  next_due_at=excluded.next_due_at, updated_at=excluded.updated_at`,
		workspaceID, artifactID, spec, tz, nextStr, now.Format(time.RFC3339), artifactID)
	return err
}

// DeleteArtifact removes the schedule row of a live artifact.
func (s *Service) DeleteArtifact(ctx context.Context, artifactID string) error {
	_, err := s.st.DB().ExecContext(ctx, `DELETE FROM schedules WHERE artifact_id=?`, artifactID)
	return err
}

// zone resolves the stored zone of a row. An empty zone follows the default.
func (s *Service) zone(tz string) string {
	if tz != "" {
		return tz
	}
	if s.defaultTZ != nil {
		return cmp.Or(s.defaultTZ(), "UTC")
	}
	return "UTC"
}

// due is nextDue in the resolved zone of a row.
func (s *Service) due(spec string, from time.Time, tz string) time.Time {
	return nextDue(spec, from, s.zone(tz))
}
