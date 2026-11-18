package runs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/store"
)

// ActivityGroup is the row key of the activity report.
type ActivityGroup string

const (
	ActivityByAgent ActivityGroup = "agent"
	ActivityByJob   ActivityGroup = "job"
)

// ActivityState is the closed set of states that a run shows in the activity
// report. The server owns the set.
type ActivityState string

const (
	StateRunning ActivityState = "running"
	StateFailed  ActivityState = "failed"
	StateWaiting ActivityState = "waiting"
	StateSkipped ActivityState = "skipped"
	StateDone    ActivityState = "done"
)

// ErrActivity marks a rejected activity request.
var ErrActivity = errors.New("invalid activity request")

const (
	minActivityBin  = 5 * time.Minute
	maxActivityBins = 1000
	maxActivitySpan = 32*24*time.Hour + time.Hour
	defaultRunCap   = 2000
	topActivityJobs = 4
)

// StateOf maps a run to one activity state. A human outcome wins over the
// machine status. The session state splits running from waiting.
func StateOf(status Status, outcome string, end EndReason, session SessionState) ActivityState {
	if outcome != "" {
		status = Status(outcome)
	}
	switch status {
	case StatusRunning:
		if session == SessionWaiting {
			return StateWaiting
		}
		return StateRunning
	case StatusParked:
		if end == EndRestart {
			return StateSkipped
		}
		return StateWaiting
	case StatusSkipped:
		return StateSkipped
	case StatusFailed, StatusExited, StatusTimedOut:
		return StateFailed
	}
	return StateDone
}

// activityStateSQL is StateOf for a stored run that is not running.
const activityStateSQL = `CASE ` + effectiveSQL + `
	WHEN '` + string(StatusFailed) + `' THEN '` + string(StateFailed) + `'
	WHEN '` + string(StatusExited) + `' THEN '` + string(StateFailed) + `'
	WHEN '` + string(StatusTimedOut) + `' THEN '` + string(StateFailed) + `'
	WHEN '` + string(StatusSkipped) + `' THEN '` + string(StateSkipped) + `'
	WHEN '` + string(StatusParked) + `' THEN CASE WHEN COALESCE(r.end_reason,'') = '` + string(EndRestart) + `'
		THEN '` + string(StateSkipped) + `' ELSE '` + string(StateWaiting) + `' END
	ELSE '` + string(StateDone) + `' END`

const (
	// effectiveSQL is the effective status of a run row: a human outcome wins.
	effectiveSQL = `COALESCE(NULLIF(r.outcome,''), r.status)`
	// ownerSQL is the owner ref of a run row: the agent that owns the job, else the job.
	ownerSQL = `COALESCE(NULLIF(ji.owner_id,''), r.job_id)`
)

// DueFire is one planned fire of a schedule.
type DueFire struct {
	WorkspaceID, JobID, Spec string
	At                       time.Time
}

// ActivityOpts selects the window, the bins, and the rows of Activity.
type ActivityOpts struct {
	Workspace string
	From, To  time.Time
	Bin       time.Duration
	Group     ActivityGroup
	// Owner is an agent ref (id or name). It limits the report to that agent and its jobs.
	Owner string
	Due   []DueFire
	// RunCap caps Runs; the default is 2000.
	RunCap int

	// owners holds the agent ids that Owner names. Activity fills it.
	owners []string
}

// Validate rejects a window that is empty, too long, or too fine.
func (o ActivityOpts) Validate() error {
	switch {
	case !o.From.Before(o.To):
		return fmt.Errorf("%w: from must be before to", ErrActivity)
	case o.Bin < minActivityBin || o.Bin%time.Second != 0:
		return fmt.Errorf("%w: bin must be a whole number of seconds, at least 5m", ErrActivity)
	case o.To.Sub(o.From) > maxActivitySpan:
		return fmt.Errorf("%w: the window is longer than 32 days", ErrActivity)
	case o.bins() > maxActivityBins:
		return fmt.Errorf("%w: the window has more than %d bins", ErrActivity, maxActivityBins)
	case o.Group != ActivityByAgent && o.Group != ActivityByJob:
		return fmt.Errorf("%w: group must be agent or job", ErrActivity)
	}
	return nil
}

// bins is the number of bins that cover the window.
func (o ActivityOpts) bins() int {
	return int((o.To.Sub(o.From) + o.Bin - 1) / o.Bin)
}

// ActivityBin counts the runs that started in one bin, and the tokens and
// cost of the turns that started in it. Tokens are the billed tokens.
type ActivityBin struct {
	I       int     `json:"i"`
	Done    int     `json:"done"`
	Failed  int     `json:"failed"`
	Waiting int     `json:"waiting"`
	Running int     `json:"running"`
	Skipped int     `json:"skipped"`
	Tokens  int64   `json:"tokens"`
	CostUSD float64 `json:"cost_usd"`
	// Jobs holds the top jobs of an agent bin by run count. OtherJobs is the
	// number of jobs left out.
	Jobs      []ActivityJobCount `json:"jobs,omitempty"`
	OtherJobs int                `json:"other_jobs,omitempty"`
}

func (b *ActivityBin) count(state ActivityState, n int) {
	switch state {
	case StateRunning:
		b.Running += n
	case StateFailed:
		b.Failed += n
	case StateWaiting:
		b.Waiting += n
	case StateSkipped:
		b.Skipped += n
	default:
		b.Done += n
	}
}

type ActivityJobCount struct {
	JobID string `json:"job_id"`
	Runs  int    `json:"runs"`
}

// ActivityDue is the planned fires of one bin.
type ActivityDue struct {
	I     int            `json:"i"`
	N     int            `json:"n"`
	Fires []ActivityFire `json:"fires"`
}

// ActivityFire is one planned fire in a bin.
type ActivityFire struct {
	JobID string    `json:"job_id"`
	At    time.Time `json:"at"`
	// Schedule is the spec of the schedule that plans the fire.
	Schedule string `json:"schedule"`
}

// ActivityRow is one agent or one job. Bins and Due are sparse.
type ActivityRow struct {
	Key         string        `json:"key"`
	Kind        string        `json:"kind"`
	WorkspaceID string        `json:"workspace_id"`
	Bins        []ActivityBin `json:"bins"`
	Due         []ActivityDue `json:"due"`
}

// ActivityRun is one run of the Runs list of a job row.
type ActivityRun struct {
	ID         string             `json:"id"`
	JobID      string             `json:"job_id"`
	State      ActivityState      `json:"state"`
	Status     Status             `json:"status"`
	StartedAt  time.Time          `json:"started_at"`
	DurationMS int64              `json:"duration_ms"`
	Tokens     pricing.TokenUsage `json:"tokens"`
	Reason     RunReason          `json:"reason"`
	Harness    string             `json:"harness"`
	Model      string             `json:"model"`
}

// Activity is the report of Activity.
type Activity struct {
	From      time.Time     `json:"from"`
	To        time.Time     `json:"to"`
	BinMS     int64         `json:"bin_ms"`
	Now       time.Time     `json:"now"`
	Group     ActivityGroup `json:"group"`
	Rows      []ActivityRow `json:"rows"`
	Runs      []ActivityRun `json:"runs,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
}

// activityRow collects one row while the queries run.
type activityRow struct {
	ActivityRow
	bins map[int]*ActivityBin
	jobs map[int]map[string]int
	due  map[int][]ActivityFire
}

// activityBuilder folds the query rows into rows and bins.
type activityBuilder struct {
	o    ActivityOpts
	rows map[string]*activityRow
}

func (b *activityBuilder) row(key, ws, kind string) *activityRow {
	r, ok := b.rows[key]
	if !ok {
		r = &activityRow{ActivityRow: ActivityRow{Key: key, Kind: kind, WorkspaceID: ws},
			bins: map[int]*ActivityBin{}, jobs: map[int]map[string]int{}, due: map[int][]ActivityFire{}}
		b.rows[key] = r
	}
	return r
}

func (r *activityRow) bin(i int) *ActivityBin {
	bin, ok := r.bins[i]
	if !ok {
		bin = &ActivityBin{I: i}
		r.bins[i] = bin
	}
	return bin
}

// addRuns counts n runs of one job in bin i.
func (b *activityBuilder) addRuns(key, ws, kind, jobID string, i int, state ActivityState, n int) {
	r := b.row(key, ws, kind)
	r.bin(i).count(state, n)
	if b.o.Group == ActivityByAgent && r.Kind == string(KindAgent) {
		if r.jobs[i] == nil {
			r.jobs[i] = map[string]int{}
		}
		r.jobs[i][jobID] += n
	}
}

// finish sorts the rows and the bins, and cuts the job lists.
func (b *activityBuilder) finish() []ActivityRow {
	out := make([]ActivityRow, 0, len(b.rows))
	for _, r := range b.rows {
		r.Bins = make([]ActivityBin, 0, len(r.bins))
		for i, bin := range r.bins {
			bin.Jobs, bin.OtherJobs = topJobs(r.jobs[i])
			r.Bins = append(r.Bins, *bin)
		}
		slices.SortFunc(r.Bins, func(a, c ActivityBin) int { return a.I - c.I })
		r.Due = make([]ActivityDue, 0, len(r.due))
		for i, fires := range r.due {
			slices.SortFunc(fires, func(a, c ActivityFire) int {
				return cmp.Or(a.At.Compare(c.At), strings.Compare(a.JobID, c.JobID))
			})
			r.Due = append(r.Due, ActivityDue{I: i, N: len(fires), Fires: fires})
		}
		slices.SortFunc(r.Due, func(a, c ActivityDue) int { return a.I - c.I })
		out = append(out, r.ActivityRow)
	}
	slices.SortFunc(out, func(a, c ActivityRow) int { return strings.Compare(a.Key, c.Key) })
	return out
}

// topJobs returns the jobs with the most runs, and the count of the others.
func topJobs(counts map[string]int) ([]ActivityJobCount, int) {
	if len(counts) == 0 {
		return nil, 0
	}
	list := make([]ActivityJobCount, 0, len(counts))
	for id, n := range counts {
		list = append(list, ActivityJobCount{JobID: id, Runs: n})
	}
	slices.SortFunc(list, func(a, c ActivityJobCount) int {
		if a.Runs != c.Runs {
			return c.Runs - a.Runs
		}
		return strings.Compare(a.JobID, c.JobID)
	})
	if len(list) <= topActivityJobs {
		return list, 0
	}
	return list[:topActivityJobs], len(list) - topActivityJobs
}

// activityWindow holds the SQL pieces of one window. The string bounds use
// the index of started_at. The epoch bounds are exact to the second.
type activityWindow struct {
	where string
	args  []any
	bin   func(col string) string
}

func (o ActivityOpts) window(col string) activityWindow {
	fromU, toU := o.From.Unix(), (o.To.Unix() + boolInt(o.To.Nanosecond() > 0))
	epoch := store.EpochSQL(col)
	return activityWindow{
		where: col + ` >= ? AND ` + col + ` < ? AND ` + epoch + ` >= ` + strconv.FormatInt(fromU, 10) +
			` AND ` + epoch + ` < ` + strconv.FormatInt(toU, 10),
		args: []any{o.From.Add(-time.Second).UTC().Format(time.RFC3339Nano), o.To.Add(time.Second).UTC().Format(time.RFC3339Nano)},
		bin: func(c string) string {
			return `((` + store.EpochSQL(c) + ` - ` + strconv.FormatInt(fromU, 10) + `) / ` +
				strconv.FormatInt(int64(o.Bin/time.Second), 10) + `)`
		},
	}
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// keySQL is the row key, and kindSQL the row kind, of a run row.
func (o ActivityOpts) keySQL() (key, kind string) {
	if o.Group == ActivityByJob {
		return `r.job_id`, `r.kind`
	}
	return ownerSQL, `CASE WHEN COALESCE(ji.owner_id,'') <> '' OR r.kind = '` + KindAgent + `' THEN '` + KindAgent + `' ELSE '` + KindJob + `' END`
}

// filter is the workspace and owner condition on the aliases r and ji.
func (o ActivityOpts) filter() (string, []any) {
	var q string
	var args []any
	if o.Workspace != "" {
		q += ` AND r.workspace_id = ?`
		args = append(args, o.Workspace)
	}
	if o.Owner != "" {
		if len(o.owners) == 0 {
			return q + ` AND 0`, args
		}
		q += ` AND ` + ownerSQL + ` IN (` + strings.TrimSuffix(strings.Repeat("?,", len(o.owners)), ",") + `)`
		for _, id := range o.owners {
			args = append(args, id)
		}
	}
	return q, args
}

// Activity reports the runs, the tokens, and the planned fires per bin. A run
// counts in the bin of its start. A turn counts in the bin of its start.
// SQLite groups the rows, so the cost does not grow with the run count.
func (s *Service) Activity(ctx context.Context, o ActivityOpts) (*Activity, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	owners, err := s.ownerIDs(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("runs activity: %w", err)
	}
	o.owners = owners
	o.From, o.To = o.From.UTC(), o.To.UTC()
	b := &activityBuilder{o: o, rows: map[string]*activityRow{}}
	out := &Activity{From: o.From, To: o.To, BinMS: o.Bin.Milliseconds(), Now: time.Now().UTC(), Group: o.Group}
	for _, step := range []func(context.Context, *activityBuilder) error{s.activityRuns, s.activityLive, s.activityTokens, s.activityDue} {
		if err := step(ctx, b); err != nil {
			return nil, fmt.Errorf("runs activity: %w", err)
		}
	}
	out.Rows = b.finish()
	if o.Group == ActivityByJob && o.Owner != "" {
		var err error
		if out.Runs, out.Truncated, err = s.activityRunList(ctx, o); err != nil {
			return nil, fmt.Errorf("runs activity: %w", err)
		}
	}
	return out, nil
}

// activityRuns counts the runs that are not live, grouped in SQL.
func (s *Service) activityRuns(ctx context.Context, b *activityBuilder) error {
	w := b.o.window(`r.started_at`)
	key, kind := b.o.keySQL()
	filter, fargs := b.o.filter()
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+key+`, r.workspace_id, `+kind+`, r.job_id, `+w.bin(`r.started_at`)+`, `+
		activityStateSQL+`, COUNT(*)
		FROM runs r LEFT JOIN job_index ji ON ji.id = r.job_id
		WHERE `+w.where+` AND r.status <> '`+string(StatusRunning)+`'`+filter+`
		GROUP BY 1, 2, 3, 4, 5, 6`, append(w.args, fargs...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k, ws, kd, jobID, state string
		var i, n int
		if err := rows.Scan(&k, &ws, &kd, &jobID, &i, &state, &n); err != nil {
			return err
		}
		b.addRuns(k, ws, kd, jobID, i, ActivityState(state), n)
	}
	return rows.Err()
}

// activityLive counts the running runs. The session state of each one splits
// running from waiting, and only the daemon knows it.
func (s *Service) activityLive(ctx context.Context, b *activityBuilder) error {
	w := b.o.window(`r.started_at`)
	key, kind := b.o.keySQL()
	filter, fargs := b.o.filter()
	rows, err := s.st.DB().QueryContext(ctx, `SELECT r.id, `+key+`, r.workspace_id, `+kind+`, r.job_id, `+w.bin(`r.started_at`)+`
		FROM runs r LEFT JOIN job_index ji ON ji.id = r.job_id
		WHERE `+w.where+` AND r.status = '`+string(StatusRunning)+`'`+filter, append(w.args, fargs...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, k, ws, kd, jobID string
		var i int
		if err := rows.Scan(&id, &k, &ws, &kd, &jobID, &i); err != nil {
			return err
		}
		b.addRuns(k, ws, kd, jobID, i, StateOf(StatusRunning, "", "", s.SessionState(id)), 1)
	}
	return rows.Err()
}

// activityTokens adds the tokens and the cost of the turns to the bin of their start.
func (s *Service) activityTokens(ctx context.Context, b *activityBuilder) error {
	w := b.o.window(`t.started_at`)
	key, kind := b.o.keySQL()
	filter, fargs := b.o.filter()
	sums, err := s.turnUsage(ctx, turnQuery{
		key: key, key2: `r.workspace_id`, bin: w.bin(`t.started_at`), kind: kind,
		where: w.where + filter, args: append(w.args, fargs...),
	})
	if err != nil {
		return err
	}
	for k, sum := range sums {
		r := b.row(k.a, k.b, k.kind)
		bin := r.bin(int(k.bin))
		bin.Tokens += sum.usage.Billed
		bin.CostUSD += sum.usage.CostUSD
	}
	return nil
}

// jobMeta is the part of a job_index row that the due fires need.
type jobMeta struct{ owner, kind string }

// jobMetas reads the owner and the kind of each job of the workspace.
func (s *Service) jobMetas(ctx context.Context, workspace string) (map[string]jobMeta, error) {
	q := `SELECT id, COALESCE(owner_id,''), kind FROM job_index`
	var args []any
	if workspace != "" {
		q += ` WHERE workspace_id = ?`
		args = append(args, workspace)
	}
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	meta := map[string]jobMeta{}
	for rows.Next() {
		var id string
		var m jobMeta
		if err := rows.Scan(&id, &m.owner, &m.kind); err != nil {
			return nil, err
		}
		meta[id] = m
	}
	return meta, rows.Err()
}

// activityDue counts the planned fires into the bins.
func (s *Service) activityDue(ctx context.Context, b *activityBuilder) error {
	if len(b.o.Due) == 0 {
		return nil
	}
	meta, err := s.jobMetas(ctx, b.o.Workspace)
	if err != nil {
		return err
	}
	var owners map[string]bool
	if b.o.Owner != "" {
		owners = make(map[string]bool, len(b.o.owners))
		for _, id := range b.o.owners {
			owners[id] = true
		}
	}
	for _, f := range b.o.Due {
		m, ok := meta[f.JobID]
		if !ok || f.At.Before(b.o.From) || !f.At.Before(b.o.To) {
			continue
		}
		b.addDue(f, m, owners)
	}
	return nil
}

// addDue counts one fire in its row. A nil owners keeps every owner.
func (b *activityBuilder) addDue(f DueFire, m jobMeta, owners map[string]bool) {
	owner := cmp.Or(m.owner, f.JobID)
	if owners != nil && !owners[owner] {
		return
	}
	key, kind := owner, string(KindJob)
	if m.owner != "" || m.kind == string(KindAgent) {
		kind = string(KindAgent)
	}
	if b.o.Group == ActivityByJob {
		key, kind = f.JobID, m.kind
	}
	r := b.row(key, f.WorkspaceID, kind)
	i := int(f.At.Sub(b.o.From) / b.o.Bin)
	r.due[i] = append(r.due[i], ActivityFire{JobID: f.JobID, At: f.At.UTC(), Schedule: f.Spec})
}

// ownerIDs resolves the Owner ref to agent ids. It returns nil when no agent matches.
func (s *Service) ownerIDs(ctx context.Context, o ActivityOpts) ([]string, error) {
	if o.Owner == "" {
		return nil, nil
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id FROM job_index WHERE id = ? OR (name = ? AND retired_at IS NULL)`, o.Owner, o.Owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// activityRunList lists the runs of the window for the Owner, newest first.
func (s *Service) activityRunList(ctx context.Context, o ActivityOpts) ([]ActivityRun, bool, error) {
	limit := o.RunCap
	if limit <= 0 {
		limit = defaultRunCap
	}
	w := o.window(`r.started_at`)
	filter, fargs := o.filter()
	rows, err := s.st.DB().QueryContext(ctx, `SELECT r.id, r.job_id, r.status, COALESCE(r.outcome,''), COALESCE(r.end_reason,''),
		r.started_at, r.active_ms, r.turn_started_at, COALESCE(r.reason,''), COALESCE(r.harness,''),
		COALESCE(r.tokens_input,0), COALESCE(r.tokens_output,0), COALESCE(r.tokens_cache_read,0),
		COALESCE(r.tokens_cache_creation,0), COALESCE(r.tokens_model,''), COALESCE(r.tokens_cost_usd,0)
		FROM runs r LEFT JOIN job_index ji ON ji.id = r.job_id
		WHERE `+w.where+filter+` ORDER BY r.started_at DESC, r.id DESC LIMIT ?`, append(append(w.args, fargs...), limit+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []ActivityRun
	for rows.Next() {
		var p runBrief
		var run ActivityRun
		var reason, harness string
		var u pricing.TokenUsage
		if err := rows.Scan(&run.ID, &run.JobID, &p.status, &p.outcome, &p.end, &p.started, &p.activeMS, &p.turnStarted,
			&reason, &harness, &u.Input, &u.Output, &u.CacheRead, &u.CacheCreation, &u.Model, &u.HarnessUSD); err != nil {
			return nil, false, err
		}
		u.ApplyRates()
		run.State, run.Status, run.StartedAt = s.stateOf(run.ID, p), p.effective(), store.ParseTS(p.started)
		run.DurationMS = p.duration()
		run.Tokens, run.Reason, run.Harness, run.Model = u, RunReason(reason), harness, u.Model
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// runBrief is the stored part of a run that the state and the duration need.
type runBrief struct {
	status, outcome, end, started string
	activeMS                      int64
	turnStarted                   *string
}

// effective is the effective status: a human outcome wins.
func (p runBrief) effective() Status {
	if p.outcome != "" {
		return Status(p.outcome)
	}
	return Status(p.status)
}

// duration is the active time. A running run adds the open part of its turn.
func (p runBrief) duration() int64 {
	ms := p.activeMS
	if p.turnStarted != nil && *p.turnStarted != "" && Status(p.status).IsLive() {
		ms += time.Since(store.ParseTS(*p.turnStarted)).Milliseconds()
	}
	return ms
}

func (p runBrief) live() bool { return Status(p.status).IsLive() }

func (s *Service) stateOf(id string, p runBrief) ActivityState {
	var session SessionState
	if Status(p.status).IsLive() {
		session = s.SessionState(id)
	}
	return StateOf(Status(p.status), p.outcome, EndReason(p.end), session)
}
