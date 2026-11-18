package runs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/store"
)

// turnQuery selects turn sums. The SQL expressions read the aliases t (turn),
// r (run), and ji (job index). An empty expression is a constant.
type turnQuery struct {
	key, key2, kind, bin string
	where                string
	args                 []any
}

// turnKey is the group of a turn sum.
type turnKey struct {
	a, b, kind string
	bin        int64
}

// turnSum is the sum of the priced turns of one group.
type turnSum struct {
	usage pricing.TokenUsage
	turns int
}

func (t *turnSum) add(u pricing.TokenUsage, turns int) {
	t.usage.Input += u.Input
	t.usage.Output += u.Output
	t.usage.CacheRead += u.CacheRead
	t.usage.CacheCreation += u.CacheCreation
	t.usage.Billed += u.Billed
	t.usage.Cached += u.Cached
	t.usage.CostUSD += u.CostUSD
	t.turns += turns
}

// turnUsage sums and prices the turns that match the query. SQLite groups the
// turns by model and by harness cost, so the price of a turn is the same as in
// any other report: the harness cost, else the rate of the model. Every cost
// report reads turns through this function.
func (s *Service) turnUsage(ctx context.Context, q turnQuery) (map[turnKey]turnSum, error) {
	or := func(expr, fallback string) string {
		if expr == "" {
			return fallback
		}
		return expr
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+or(q.key, "''")+`, `+or(q.key2, "''")+`, `+or(q.kind, "''")+`, `+or(q.bin, "0")+`,
		COALESCE(NULLIF(t.tokens_model,''), r.tokens_model, ''), CASE WHEN COALESCE(t.tokens_cost_usd,0) > 0 THEN 1 ELSE 0 END,
		SUM(COALESCE(t.tokens_input,0)), SUM(COALESCE(t.tokens_output,0)), SUM(COALESCE(t.tokens_cache_read,0)),
		SUM(COALESCE(t.tokens_cache_creation,0)), SUM(COALESCE(t.tokens_cost_usd,0)), COUNT(*)
		FROM run_turns t JOIN runs r ON r.id = t.run_id LEFT JOIN job_index ji ON ji.id = r.job_id
		WHERE `+q.where+` GROUP BY 1, 2, 3, 4, 5, 6`, q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[turnKey]turnSum{}
	for rows.Next() {
		var k turnKey
		var model string
		var harness, turns int
		var u pricing.TokenUsage
		if err := rows.Scan(&k.a, &k.b, &k.kind, &k.bin, &model, &harness, &u.Input, &u.Output, &u.CacheRead, &u.CacheCreation, &u.HarnessUSD, &turns); err != nil {
			return nil, err
		}
		u.Model = model
		if harness == 0 {
			u.HarnessUSD = 0
		}
		u.ApplyRates()
		sum := out[k]
		sum.add(u, turns)
		out[k] = sum
	}
	return out, rows.Err()
}

// Spend is the turn count, the priced cost, and the token totals of a set of turns.
type Spend struct {
	Turns   int
	CostUSD float64
	Tokens  pricing.TokenTotals
}

// add prices one turn and folds it in.
func (s *Spend) add(u pricing.TokenUsage) {
	u.ApplyRates()
	s.Turns++
	s.CostUSD += u.CostUSD
	s.Tokens.Add(u)
}

// OwnerSpend returns the spend per agent ID since each given time, including
// the jobs that the agent owns. The result has one map per time, in order. It
// reads the turns of the workspace in one query, so agent list pages do not
// issue one query per agent or per time. A turn counts at its start.
func (s *Service) OwnerSpend(ctx context.Context, workspaceID string, agentIDs []string, since ...time.Time) ([]map[string]Spend, error) {
	if len(since) == 0 {
		return nil, nil
	}
	cuts := make([]string, len(since))
	oldest := since[0]
	flag := make([]string, len(since))
	for i, t := range since {
		cuts[i] = t.UTC().Format(time.RFC3339)
		flag[i] = fmt.Sprintf("(t.started_at >= '%s') * %d", cuts[i], 1<<i)
		if t.Before(oldest) {
			oldest = t
		}
	}
	sums, err := s.turnUsage(ctx, turnQuery{
		key: ownerSQL, bin: strings.Join(flag, " + "), where: `r.workspace_id = ? AND t.started_at >= ?`,
		args: []any{workspaceID, oldest.UTC().Format(time.RFC3339)},
	})
	if err != nil {
		return nil, fmt.Errorf("owner spend: %w", err)
	}
	want := make(map[string]bool, len(agentIDs))
	for _, id := range agentIDs {
		want[id] = true
	}
	out := make([]map[string]Spend, len(since))
	for i := range out {
		out[i] = make(map[string]Spend, len(agentIDs))
	}
	for k, sum := range sums {
		if !want[k.a] {
			continue
		}
		for i := range since {
			if k.bin&(1<<i) == 0 {
				continue
			}
			sp := out[i][k.a]
			sp.Turns += sum.turns
			sp.CostUSD += sum.usage.CostUSD
			sp.Tokens.Add(sum.usage)
			out[i][k.a] = sp
		}
	}
	return out, nil
}

// RecentRun is one bar of a sparkline: a run with its effective status, its
// state, its active time, and its priced tokens.
type RecentRun struct {
	ID         string             `json:"id"`
	Status     Status             `json:"status"`
	State      ActivityState      `json:"state"`
	StartedAt  time.Time          `json:"started_at"`
	DurationMS int64              `json:"duration_ms"`
	Tokens     pricing.TokenUsage `json:"tokens"`
}

// recentSet is the set of the newest runs of each job. owners limits it to
// the jobs of those agents.
type recentSet struct {
	workspace string
	perJob    int
	owners    []string
}

// maxOwnerFilter bounds the owner list in SQL; a longer list reads all jobs.
const maxOwnerFilter = 500

// from returns the FROM and WHERE of the runs in the set. Each job reads its
// newest runs from idx_runs_job_started, so the cost follows the job count.
func (c recentSet) from(j, r string) (string, []any) {
	q := ` FROM job_index ` + j + ` CROSS JOIN runs ` + r + ` ON ` + r + `.job_id = ` + j + `.id WHERE 1=1`
	var args []any
	if c.workspace != "" {
		q += ` AND ` + j + `.workspace_id = ?`
		args = append(args, c.workspace)
	}
	if n := len(c.owners); n > 0 && n <= maxOwnerFilter {
		q += ` AND COALESCE(NULLIF(` + j + `.owner_id,''), ` + j + `.id) IN (?` + strings.Repeat(",?", n-1) + `)`
		for _, o := range c.owners {
			args = append(args, o)
		}
	}
	q += ` AND ` + r + `.started_at >= COALESCE((SELECT x.started_at FROM runs x WHERE x.job_id = ` + j + `.id
		ORDER BY x.started_at DESC LIMIT 1 OFFSET ` + fmt.Sprint(c.perJob-1) + `), '')`
	return q, args
}

// recentRow is a stored run of a recent set.
type recentRow struct {
	id, jobID, owner string
	brief            runBrief
	summary          RunSummary
}

// load reads the runs of the set, newest first, and the priced tokens of each run.
func (c recentSet) load(ctx context.Context, s *Service) ([]recentRow, map[string]pricing.TokenUsage, error) {
	inner, iargs := c.from("jj", "rj")
	var sums map[turnKey]turnSum
	var sumsErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		sums, sumsErr = s.turnUsage(ctx, turnQuery{key: "t.run_id", where: `t.run_id IN (SELECT rj.id` + inner + `)`, args: iargs})
	})
	defer wg.Wait()
	from, args := c.from("ji", "r")
	rows, err := s.st.DB().QueryContext(ctx, `SELECT r.id, r.job_id, `+ownerSQL+`, r.status, COALESCE(r.outcome,''), COALESCE(r.end_reason,''),
		r.started_at, r.active_ms, r.turn_started_at, COALESCE(r.tokens_input,0), COALESCE(r.tokens_output,0),
		COALESCE(r.tokens_cache_read,0), COALESCE(r.tokens_cache_creation,0), COALESCE(r.tokens_model,''), COALESCE(r.tokens_cost_usd,0)`+from, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var list []recentRow
	for rows.Next() {
		var row recentRow
		var u pricing.TokenUsage
		if err := rows.Scan(&row.id, &row.jobID, &row.owner, &row.brief.status, &row.brief.outcome, &row.brief.end, &row.brief.started,
			&row.brief.activeMS, &row.brief.turnStarted, &u.Input, &u.Output, &u.CacheRead, &u.CacheCreation, &u.Model, &u.HarnessUSD); err != nil {
			return nil, nil, err
		}
		u.ApplyRates()
		row.summary = RunSummary{ID: row.id, Status: Status(row.brief.status), EndReason: EndReason(row.brief.end),
			StartedAt: store.ParseTS(row.brief.started), DurationMS: row.brief.duration(), CostUSD: u.CostUSD, Tokens: u}
		list = append(list, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	slices.SortFunc(list, func(a, b recentRow) int {
		if c := strings.Compare(b.brief.started, a.brief.started); c != 0 {
			return c
		}
		return strings.Compare(b.id, a.id)
	})
	wg.Wait()
	if sumsErr != nil {
		return nil, nil, sumsErr
	}
	usage := make(map[string]pricing.TokenUsage, len(sums))
	for k, sum := range sums {
		usage[k.a] = sum.usage
	}
	return list, usage, nil
}

func (s *Service) recentRun(row recentRow, usage map[string]pricing.TokenUsage) RecentRun {
	return RecentRun{
		ID: row.id, Status: row.brief.effective(), State: s.stateOf(row.id, row.brief),
		StartedAt: row.summary.StartedAt, DurationMS: row.brief.duration(), Tokens: usage[row.id],
	}
}

// JobActivity is the list view of one job.
type JobActivity struct {
	LastRunAt  *time.Time
	LastStatus Status
	// Recent holds the newest runs, oldest first.
	Recent    []RecentRun
	Tokens30d pricing.TokenTotals
	// RunCount is all runs of the job, with no time limit.
	RunCount int
}

// JobActivity returns the last run, the newest runs, and the tokens since a
// time, for each job of the workspace. An empty workspace reads all of them.
func (s *Service) JobActivity(ctx context.Context, workspaceID string, recent int, since time.Time) (map[string]JobActivity, error) {
	recent = max(recent, 1)
	where, args := `t.started_at >= ?`, []any{since.UTC().Format(time.RFC3339)}
	if workspaceID != "" {
		where, args = `r.workspace_id = ? AND `+where, append([]any{workspaceID}, args...)
	}
	var sums map[turnKey]turnSum
	var sumsErr error
	var wg sync.WaitGroup
	wg.Go(func() { sums, sumsErr = s.turnUsage(ctx, turnQuery{key: "r.job_id", where: where, args: args}) })
	rows, usage, err := recentSet{workspace: workspaceID, perJob: recent}.load(ctx, s)
	wg.Wait()
	if err != nil {
		return nil, fmt.Errorf("job activity: %w", err)
	}
	if sumsErr != nil {
		return nil, fmt.Errorf("job activity: %w", sumsErr)
	}
	out := map[string]JobActivity{}
	for _, row := range rows {
		ja := out[row.jobID]
		if ja.LastRunAt == nil {
			at := row.summary.StartedAt
			ja.LastRunAt, ja.LastStatus = &at, row.brief.effective()
		}
		if len(ja.Recent) < recent {
			ja.Recent = append(ja.Recent, s.recentRun(row, usage))
		}
		out[row.jobID] = ja
	}
	for k, sum := range sums {
		ja := out[k.a]
		ja.Tokens30d.Add(sum.usage)
		out[k.a] = ja
	}
	counts, err := s.runCounts(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("job activity: %w", err)
	}
	for id, n := range counts {
		ja := out[id]
		ja.RunCount = n
		out[id] = ja
	}
	for id, ja := range out {
		slices.Reverse(ja.Recent)
		out[id] = ja
	}
	return out, nil
}

// JobRunCount counts all runs of one job.
func (s *Service) JobRunCount(ctx context.Context, jobID string) (int, error) {
	var n int
	err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE job_id = ?`, jobID).Scan(&n)
	return n, err
}

// runCounts counts all runs of each job. An empty workspace reads all of them.
func (s *Service) runCounts(ctx context.Context, workspaceID string) (map[string]int, error) {
	q, args := `SELECT job_id, COUNT(*) FROM runs GROUP BY job_id`, []any(nil)
	if workspaceID != "" {
		q, args = `SELECT job_id, COUNT(*) FROM runs WHERE workspace_id = ? GROUP BY job_id`, []any{workspaceID}
	}
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// OwnerActivity is the list view of one agent and the jobs that it owns.
type OwnerActivity struct {
	// Recent holds the newest runs, oldest first.
	Recent []RecentRun
	// Last is the newest run that is not live.
	Last         *RunSummary
	Runs7d, Ok7d int
	Running      bool
}

// OwnerActivity returns the run view of each agent in owners. Runs7d and Ok7d
// count the runs since since7d; Ok7d counts the effective status.
func (s *Service) OwnerActivity(ctx context.Context, workspaceID string, owners []string, recent int, since7d time.Time) (map[string]OwnerActivity, error) {
	want := make(map[string]bool, len(owners))
	for _, o := range owners {
		want[o] = true
	}
	out := make(map[string]OwnerActivity, len(owners))
	rows, usage, err := recentSet{workspace: workspaceID, perJob: max(recent, 1), owners: owners}.load(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("owner activity: %w", err)
	}
	for _, row := range rows {
		if !want[row.owner] {
			continue
		}
		oa := out[row.owner]
		if oa.Last == nil && !row.brief.live() {
			sum := row.summary
			oa.Last = &sum
		}
		if len(oa.Recent) < recent {
			oa.Recent = append(oa.Recent, s.recentRun(row, usage))
		}
		out[row.owner] = oa
	}
	for id, oa := range out {
		slices.Reverse(oa.Recent)
		out[id] = oa
	}
	if err := s.ownerCounts(ctx, workspaceID, want, since7d, out); err != nil {
		return nil, fmt.Errorf("owner activity: %w", err)
	}
	return out, nil
}

// ownerCounts fills the 7-day counts and the running flag with two grouped queries.
func (s *Service) ownerCounts(ctx context.Context, workspaceID string, want map[string]bool, since time.Time, out map[string]OwnerActivity) error {
	ws, args := "", []any{}
	if workspaceID != "" {
		ws, args = ` AND r.workspace_id = ?`, []any{workspaceID}
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+ownerSQL+`, COUNT(*),
		SUM(CASE WHEN `+effectiveSQL+` = '`+string(StatusComplete)+`' THEN 1 ELSE 0 END)
		FROM runs r LEFT JOIN job_index ji ON ji.id = r.job_id WHERE r.started_at >= ?`+ws+` GROUP BY 1`,
		append([]any{since.UTC().Format(time.RFC3339)}, args...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var owner string
		var n, ok int
		if err := rows.Scan(&owner, &n, &ok); err != nil {
			return err
		}
		if want[owner] {
			oa := out[owner]
			oa.Runs7d, oa.Ok7d = n, ok
			out[owner] = oa
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	live, err := s.st.DB().QueryContext(ctx, `SELECT DISTINCT `+ownerSQL+` FROM runs r LEFT JOIN job_index ji ON ji.id = r.job_id
		WHERE r.status = '`+string(StatusRunning)+`'`+ws, args...)
	if err != nil {
		return err
	}
	defer live.Close()
	for live.Next() {
		var owner string
		if err := live.Scan(&owner); err != nil {
			return err
		}
		if want[owner] {
			oa := out[owner]
			oa.Running = true
			out[owner] = oa
		}
	}
	return live.Err()
}
