package alerts

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
)

// Job health rules: a job that stopped working, a run that does not end, a
// skip that a live run blocks, and a request that waits too long.
const (
	healthEvery     = 10 * time.Minute
	stuckFloor      = time.Hour
	stuckNoLimit    = 24 * time.Hour
	stuckMinSamples = 3
	parkedBadAfter  = 24 * time.Hour
	requestRemindAt = 48 * time.Hour
	repeatOpenAfter = 72 * time.Hour
)

// automaticReasons are the run reasons that no person started.
const automaticReasons = `('schedule','trigger')`

// effectiveStatusSQL is the status of a run after a human outcome.
const effectiveStatusSQL = `CASE WHEN COALESCE(outcome,'') <> '' THEN outcome ELSE status END`

// StreakRun is one run of a job streak.
type StreakRun struct {
	ID        string
	Reason    string
	Status    string
	EndReason string
	Harness   string
	StartedAt time.Time
	EndedAt   time.Time
}

// Streak is the health of a job: its last complete run, the automatic runs
// after it that did not complete, and whether a run is live now.
type Streak struct {
	LastComplete *StreakRun
	// Bad is newest first.
	Bad  []StreakRun
	Live bool
}

// JobStreak reads the streak of a job at now. A skipped, exited, timed out,
// or failed automatic run is bad. A run parked on a request is bad after
// parkedBadAfter.
func (s *Service) JobStreak(ctx context.Context, jobID string, now time.Time) (Streak, error) {
	db := s.st.DB()
	var out Streak
	last, err := scanStreakRun(db.QueryRowContext(ctx, `SELECT id, COALESCE(reason,''), `+effectiveStatusSQL+`,
		COALESCE(end_reason,''), COALESCE(harness,''), started_at, COALESCE(ended_at,'')
		FROM runs WHERE job_id=? AND `+effectiveStatusSQL+`='complete'
		ORDER BY COALESCE(ended_at, started_at) DESC LIMIT 1`, jobID))
	switch {
	case err == nil:
		out.LastComplete = &last
	case !errors.Is(err, sql.ErrNoRows):
		return out, fmt.Errorf("streak last complete %s: %w", jobID, err)
	}
	var live int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE job_id=? AND status='running'`, jobID).Scan(&live); err != nil {
		return out, fmt.Errorf("streak live %s: %w", jobID, err)
	}
	out.Live = live > 0
	cutoff := ""
	if out.LastComplete != nil {
		cutoff = cmpTime(out.LastComplete.EndedAt, out.LastComplete.StartedAt).UTC().Format(time.RFC3339Nano)
	}
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(reason,''), `+effectiveStatusSQL+`,
		COALESCE(end_reason,''), COALESCE(harness,''), started_at, COALESCE(ended_at,'')
		FROM runs WHERE job_id=? AND reason IN `+automaticReasons+` AND started_at > ?
		ORDER BY started_at DESC LIMIT 50`, jobID, cutoff)
	if err != nil {
		return out, fmt.Errorf("streak runs %s: %w", jobID, err)
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanStreakRun(rows)
		if err != nil {
			return out, fmt.Errorf("streak scan %s: %w", jobID, err)
		}
		if r.bad(now) {
			out.Bad = append(out.Bad, r)
		}
	}
	return out, rows.Err()
}

func scanStreakRun(row scanner) (StreakRun, error) {
	var r StreakRun
	var started, ended string
	if err := row.Scan(&r.ID, &r.Reason, &r.Status, &r.EndReason, &r.Harness, &started, &ended); err != nil {
		return r, err
	}
	r.StartedAt, r.EndedAt = store.ParseTS(started), store.ParseTS(ended)
	return r, nil
}

func (r StreakRun) bad(now time.Time) bool {
	switch r.Status {
	case "skipped", "exited", "timed_out", "failed":
		return true
	case "parked":
		return r.EndReason == "request" && now.Sub(cmpTime(r.EndedAt, r.StartedAt)) > parkedBadAfter
	}
	return false
}

// cmpTime returns a, else b when a is zero.
func cmpTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	return a
}

// jobRef names the job of a run event.
type jobRef struct {
	ws, id, name, trace string
}

// onRunEnded applies the job health rules to a terminal run frame.
func (s *Service) onRunEnded(ctx context.Context, ev store.StreamEvent, attrs map[string]string) {
	status := firstNonEmpty(attrs["outcome"], attrs["status"])
	if status == "" || status == "running" {
		return
	}
	s.autoResolve(ctx, KindRunStuck, "stuck/"+ev.ID, "auto")
	job := jobRef{ws: attrs["workspace"], id: attrs["job_id"], name: attrs["job_name"], trace: ev.TraceID}
	if job.ws == "" || job.id == "" {
		return
	}
	if status == "complete" {
		s.autoResolve(ctx, KindJobStalled, "stalled/"+job.ws+"/"+job.id, "auto")
		s.autoResolve(ctx, KindSkipBlocked, "blocked/"+job.ws+"/"+job.id, "auto")
		return
	}
	if status == "skipped" {
		s.checkBlocked(ctx, job)
	} else {
		s.autoResolve(ctx, KindSkipBlocked, "blocked/"+job.ws+"/"+job.id, "auto")
	}
	s.checkStalled(ctx, job)
}

// jobArchived reports whether the job has an effective archive. A run that
// ends after the archive raises no health alert.
func (s *Service) jobArchived(ctx context.Context, id string) bool {
	var one int
	err := s.st.DB().QueryRowContext(ctx, `SELECT 1 FROM (`+store.ArchivedJobIDsSQL+`) WHERE id=?`, id).Scan(&one)
	return err == nil
}

// checkStalled raises job_stalled when the job completed before and its
// last StalledAfter automatic runs did not. A live run waits: run_stuck and
// skip_blocked cover it. The owner agent never takes this alert.
func (s *Service) checkStalled(ctx context.Context, job jobRef) {
	if s.jobArchived(ctx, job.id) {
		return
	}
	now := s.opts.Now()
	st, err := s.JobStreak(ctx, job.id, now)
	if err != nil {
		s.log.Warn("job streak", "job_id", job.id, "error", err)
		return
	}
	if st.LastComplete == nil || st.Live || len(st.Bad) < s.opts.StalledAfter {
		return
	}
	subject := "stalled/" + job.ws + "/" + job.id
	if s.closedByUserSince(ctx, subject, cmpTime(st.LastComplete.EndedAt, st.LastComplete.StartedAt)) {
		return
	}
	_, err = s.Raise(ctx, RaiseOpts{
		Kind: KindJobStalled, Subject: subject, WorkspaceID: job.ws, JobID: job.id, JobName: job.name,
		RunID: st.Bad[0].ID, TraceID: job.trace,
		Body: st.body(), Hint: "Open the last run to see why.",
	})
	if err != nil {
		s.log.Warn("raise job_stalled", "job_id", job.id, "error", err)
	}
}

// streakStatus words count the bad runs of a streak by status.
var streakStatus = []struct{ status, words string }{ //nolint:gochecknoglobals // static table
	{"failed", "failed"},
	{"timed_out", "ran too long"},
	{"exited", "stopped early"},
	{"skipped", "skipped"},
	{"parked", "paused"},
}

// body tells the last good run, the bad runs by status, and each change
// of the AI app.
func (st Streak) body() string {
	var b strings.Builder
	lc := st.LastComplete
	runs := "runs"
	if len(st.Bad) == 1 {
		runs = "run"
	}
	fmt.Fprintf(&b, "The last good run was on %s. Since then, %d %s did not finish", dayText(cmpTime(lc.EndedAt, lc.StartedAt)), len(st.Bad), runs)
	counts := map[string]int{}
	for _, r := range st.Bad {
		counts[r.Status]++
	}
	var parts []string
	for _, w := range streakStatus {
		if n := counts[w.status]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, w.words))
		}
	}
	if len(parts) > 0 {
		b.WriteString(": " + strings.Join(parts, ", "))
	}
	b.WriteString(".")
	harness := lc.Harness
	for i := len(st.Bad) - 1; i >= 0; i-- {
		r := st.Bad[i]
		if r.Harness != "" && harness != "" && r.Harness != harness {
			fmt.Fprintf(&b, " On %s the job changed from %s to %s.", dayText(r.StartedAt), appLabel(harness), appLabel(r.Harness))
		}
		if r.Harness != "" {
			harness = r.Harness
		}
	}
	return b.String()
}

// checkBlocked raises skip_blocked on the second skip in a row. It names
// the live run that blocks the job.
func (s *Service) checkBlocked(ctx context.Context, job jobRef) {
	if s.jobArchived(ctx, job.id) {
		return
	}
	db := s.st.DB()
	rows, err := db.QueryContext(ctx, `SELECT id, status FROM runs WHERE job_id=? AND reason IN `+automaticReasons+`
		ORDER BY started_at DESC LIMIT 2`, job.id)
	if err != nil {
		s.log.Warn("blocked skips", "job_id", job.id, "error", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id, status string
		if rows.Scan(&id, &status) == nil && status == "skipped" {
			ids = append(ids, id)
		}
	}
	_ = rows.Close() // read-only query
	if len(ids) < 2 {
		return
	}
	blocker := s.skipBlocker(ctx, ids[0], job.id)
	if blocker == "" {
		return
	}
	var started string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(turn_started_at, started_at) FROM runs WHERE id=? AND status='running'`,
		blocker).Scan(&started); err != nil {
		return
	}
	age := ageText(s.opts.Now().Sub(store.ParseTS(started)))
	opts := RaiseOpts{
		Kind: KindSkipBlocked, WorkspaceID: job.ws, JobID: job.id, JobName: job.name, RunID: blocker, TraceID: job.trace,
		Hint: "Stop that run if it does not make progress. The next run then starts.",
	}
	if err := s.named(ctx, &opts); err != nil {
		s.log.Warn("raise skip_blocked", "job_id", job.id, "error", err)
		return
	}
	opts.Body = fmt.Sprintf("%s still runs after %s, so Trebi skipped the next %d runs.",
		capFirst(s.runOf(ctx, opts.names.Owner(), blocker)), age, len(ids))
	_, err = s.Raise(ctx, opts)
	if err != nil {
		s.log.Warn("raise skip_blocked", "job_id", job.id, "error", err)
	}
}

// skipBlocker is the run that a skip names, else the oldest live run of the job.
func (s *Service) skipBlocker(ctx context.Context, skipID, jobID string) string {
	db := s.st.DB()
	var detail sql.NullString
	if db.QueryRowContext(ctx, `SELECT detail_json FROM run_events WHERE run_id=? AND kind='skip' ORDER BY id LIMIT 1`,
		skipID).Scan(&detail) == nil && detail.Valid {
		var d struct {
			Because string `json:"because"`
		}
		if json.Unmarshal([]byte(detail.String), &d) == nil && d.Because != "" {
			return d.Because
		}
	}
	var id string
	_ = db.QueryRowContext(ctx, `SELECT id FROM runs WHERE job_id=? AND status='running' ORDER BY started_at LIMIT 1`,
		jobID).Scan(&id) // no live run: no blocker
	return id
}

// sweepHealth runs the periodic rules once per healthEvery: stuck runs,
// stale requests, and jobs whose runs wait on a request too long.
func (s *Service) sweepHealth(ctx context.Context) {
	now := s.opts.Now()
	s.mu.Lock()
	due := now.Sub(s.lastHealth) >= healthEvery
	if due {
		s.lastHealth = now
	}
	s.mu.Unlock()
	if !due {
		return
	}
	s.SweepHealth(ctx)
}

// SweepHealth applies the periodic health rules now.
func (s *Service) SweepHealth(ctx context.Context) {
	s.sweepStuck(ctx)
	s.sweepRequests(ctx)
	s.sweepParked(ctx)
}

type liveRun struct {
	id, ws, jobID, jobName, trace string
	start                         time.Time
	maxMS                         int64
}

// sweepStuck raises run_stuck once per run that runs longer than
// StuckFactor times the median of the complete runs of its job, or longer
// than stuckNoLimit when it has no time limit. A run that waits for a person
// is not stuck. An agent run counts only when no person started it.
func (s *Service) sweepStuck(ctx context.Context) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT r.id, r.workspace_id, r.job_id, COALESCE(j.name,''), r.trace_id,
		COALESCE(r.turn_started_at, r.started_at), r.max_duration_ms
		FROM runs r LEFT JOIN job_index j ON j.id = r.job_id
		WHERE r.status='running' AND (COALESCE(r.kind,'job')='job' OR r.reason IN `+automaticReasons+`)`)
	if err != nil {
		s.log.Warn("stuck sweep", "error", err)
		return
	}
	var live []liveRun
	for rows.Next() {
		var r liveRun
		var started string
		if rows.Scan(&r.id, &r.ws, &r.jobID, &r.jobName, &r.trace, &started, &r.maxMS) != nil {
			continue
		}
		r.start = store.ParseTS(started)
		live = append(live, r)
	}
	_ = rows.Close() // read-only query
	now := s.opts.Now()
	for _, r := range live {
		s.mu.Lock()
		_, waiting := s.waiting[r.id]
		s.mu.Unlock()
		if waiting || r.start.IsZero() {
			continue
		}
		elapsed := now.Sub(r.start)
		median, ok := s.medianComplete(ctx, r.jobID)
		limit := time.Duration(s.opts.StuckFactor) * median
		var why string
		switch {
		case ok && elapsed > max(limit, stuckFloor):
			why = fmt.Sprintf("Its good runs take about %s.", ageText(median))
		case r.maxMS == 0 && elapsed > stuckNoLimit:
			why = "The job has no time limit."
		default:
			continue
		}
		subject := "stuck/" + r.id
		if s.everRaised(ctx, KindRunStuck, subject) {
			continue
		}
		opts := RaiseOpts{
			Kind: KindRunStuck, Subject: subject, WorkspaceID: r.ws, JobID: r.jobID, JobName: r.jobName,
			RunID: r.id, TraceID: r.trace, Hint: "Open the run to see what it does, or stop it.",
		}
		if err := s.named(ctx, &opts); err != nil {
			s.log.Warn("raise run_stuck", "run", r.id, "error", err)
			continue
		}
		opts.Body = fmt.Sprintf("%s still runs after %s. %s", capFirst(s.runOf(ctx, opts.names.Owner(), r.id)), ageText(elapsed), why)
		_, err := s.Raise(ctx, opts)
		if err != nil {
			s.log.Warn("raise run_stuck", "run", r.id, "error", err)
		}
	}
}

// medianComplete is the median length of the last 20 complete runs of a job.
func (s *Service) medianComplete(ctx context.Context, jobID string) (time.Duration, bool) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT started_at, ended_at FROM runs
		WHERE job_id=? AND status='complete' AND ended_at IS NOT NULL ORDER BY started_at DESC LIMIT 20`, jobID)
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	var ds []time.Duration
	for rows.Next() {
		var a, b string
		if rows.Scan(&a, &b) != nil {
			continue
		}
		if d := store.ParseTS(b).Sub(store.ParseTS(a)); d > 0 {
			ds = append(ds, d)
		}
	}
	if len(ds) < stuckMinSamples {
		return 0, false
	}
	slices.Sort(ds)
	return ds[len(ds)/2], true
}

// sweepRequests raises one reminder per request that is pending longer
// than requestRemindAt.
func (s *Service) sweepRequests(ctx context.Context) {
	now := s.opts.Now()
	cutoff := now.Add(-requestRemindAt).UTC().Format(time.RFC3339Nano)
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id, run_id, workspace_id, job_id, title, trace_id, created_at
		FROM requests WHERE status='pending' AND created_at < ?`, cutoff)
	if err != nil {
		s.log.Warn("request sweep", "error", err)
		return
	}
	type stale struct{ id, run, ws, job, title, trace, created string }
	var due []stale
	for rows.Next() {
		var r stale
		if rows.Scan(&r.id, &r.run, &r.ws, &r.job, &r.title, &r.trace, &r.created) == nil {
			due = append(due, r)
		}
	}
	_ = rows.Close() // read-only query
	for _, r := range due {
		subject := "request-reminder/" + r.id
		if s.everRaised(ctx, KindRequestReminder, subject) {
			continue
		}
		opts := RaiseOpts{
			Kind: KindRequestReminder, Subject: subject, RequestID: r.id, RunID: r.run,
			WorkspaceID: r.ws, JobID: r.job, TraceID: r.trace,
		}
		if err := s.named(ctx, &opts); err != nil {
			s.log.Warn("raise request_reminder", "request", r.id, "error", err)
			continue
		}
		who := cmp.Or(opts.names.Owner(), "The job")
		opts.Title = requestTitle(opts.names, r.title)
		opts.Body = fmt.Sprintf("Reminder: this question waits for your answer for %s. %s stays paused until you answer or dismiss it.",
			ageText(now.Sub(store.ParseTS(r.created))), who)
		_, err := s.Raise(ctx, opts)
		if err != nil {
			s.log.Warn("raise request_reminder", "request", r.id, "error", err)
		}
	}
}

// sweepParked checks the jobs whose automatic runs wait on a request
// longer than parkedBadAfter, because no run event marks that time.
func (s *Service) sweepParked(ctx context.Context) {
	cutoff := s.opts.Now().Add(-parkedBadAfter).UTC().Format(time.RFC3339Nano)
	rows, err := s.st.DB().QueryContext(ctx, `SELECT DISTINCT r.workspace_id, r.job_id, COALESCE(j.name,'')
		FROM runs r LEFT JOIN job_index j ON j.id = r.job_id
		WHERE r.status='parked' AND r.end_reason='request' AND r.reason IN `+automaticReasons+`
		AND COALESCE(r.ended_at, r.started_at) < ?`, cutoff)
	if err != nil {
		s.log.Warn("parked sweep", "error", err)
		return
	}
	var jobs []jobRef
	for rows.Next() {
		var j jobRef
		if rows.Scan(&j.ws, &j.id, &j.name) == nil {
			jobs = append(jobs, j)
		}
	}
	_ = rows.Close() // read-only query
	for _, j := range jobs {
		s.checkStalled(ctx, j)
	}
}

// everRaised reports whether any item of kind and subject exists, open or closed.
func (s *Service) everRaised(ctx context.Context, kind Kind, subject string) bool {
	var one int
	err := s.st.DB().QueryRowContext(ctx, `SELECT 1 FROM inbox_items WHERE alert_kind=? AND subject=? LIMIT 1`,
		string(kind), subject).Scan(&one)
	return err == nil
}

// closedByUserSince reports whether the user closed an item of subject that
// was raised after since.
func (s *Service) closedByUserSince(ctx context.Context, subject string, since time.Time) bool {
	var one int
	err := s.st.DB().QueryRowContext(ctx, `SELECT 1 FROM inbox_items WHERE subject=? AND done_by=? AND created_at > ? LIMIT 1`,
		subject, DoneByUser, since.UTC().Format(time.RFC3339Nano)).Scan(&one)
	return err == nil
}

// repeatTitle is the title of an open item that comes back. An item open
// longer than repeatOpenAfter tells its age, so the user sees that nobody
// fixed it.
func repeatTitle(title string, n int, created, now time.Time) string {
	if open := now.Sub(created); !created.IsZero() && open > repeatOpenAfter {
		return fmt.Sprintf("%s (still open for %d days)", title, int(open.Hours()/24))
	}
	if n > 1 {
		return fmt.Sprintf("%s (%d×)", title, n)
	}
	return title
}

// dayText is a short date for an alert body.
func dayText(t time.Time) string { return t.Local().Format("Jan 2") }

// ageText is a rounded length for an alert body.
func ageText(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
	return fmt.Sprintf("%d seconds", int(d.Seconds()))
}
