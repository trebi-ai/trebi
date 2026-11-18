package runs_test

import (
	"context"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

var actFrom = time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)

// turnSeed is one turn of a seeded run.
type turnSeed struct {
	at            time.Time
	input, output int64
}

// runSeed describes one stored run. The zero status is complete.
type runSeed struct {
	job, status, outcome string
	end                  runs.EndReason
	at                   time.Time
	turns                []turnSeed
}

func TestStateOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status  runs.Status
		outcome string
		end     runs.EndReason
		session runs.SessionState
		want    runs.ActivityState
	}{
		{runs.StatusRunning, "", "", runs.SessionRunning, runs.StateRunning},
		{runs.StatusRunning, "", "", runs.SessionIdle, runs.StateRunning},
		{runs.StatusRunning, "", "", runs.SessionWaiting, runs.StateWaiting},
		{runs.StatusComplete, "", runs.EndSentinel, "", runs.StateDone},
		{runs.StatusFailed, "", "", "", runs.StateFailed},
		{runs.StatusExited, "", "", "", runs.StateFailed},
		{runs.StatusTimedOut, "", "", "", runs.StateFailed},
		{runs.StatusSkipped, "", "", "", runs.StateSkipped},
		{runs.StatusParked, "", runs.EndRequest, "", runs.StateWaiting},
		{runs.StatusParked, "", runs.EndRestart, "", runs.StateSkipped},
		{runs.StatusFailed, "complete", "", "", runs.StateDone},
		{runs.StatusComplete, "failed", "", "", runs.StateFailed},
	}
	for _, c := range cases {
		if got := runs.StateOf(c.status, c.outcome, c.end, c.session); got != c.want {
			t.Errorf("StateOf(%s,%q,%s,%s) = %s, want %s", c.status, c.outcome, c.end, c.session, got, c.want)
		}
	}
}

func TestActivityOptsValidate(t *testing.T) {
	t.Parallel()
	ok := actOpts(runs.ActivityByAgent)
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid opts: %v", err)
	}
	mut := map[string]func(*runs.ActivityOpts){
		"from after to":   func(o *runs.ActivityOpts) { o.From = o.To.Add(time.Hour) },
		"bin too fine":    func(o *runs.ActivityOpts) { o.Bin = time.Minute },
		"bin has millis":  func(o *runs.ActivityOpts) { o.Bin = 5*time.Minute + time.Millisecond },
		"window too long": func(o *runs.ActivityOpts) { o.To = o.From.Add(33 * 24 * time.Hour); o.Bin = 4 * time.Hour },
		"too many bins":   func(o *runs.ActivityOpts) { o.To = o.From.Add(5 * 24 * time.Hour); o.Bin = 5 * time.Minute },
		"bad group":       func(o *runs.ActivityOpts) { o.Group = "team" },
	}
	for name, f := range mut {
		o := ok
		f(&o)
		if err := o.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// A run counts in the bin of its start, not the bin of its end.
func TestActivityBinsByStart(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(30 * time.Minute)})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(59*time.Minute + 59*time.Second)})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(time.Hour), status: "failed"})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(2 * time.Hour), status: "failed", outcome: "complete"})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(-time.Second)})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(24 * time.Hour)})
	a, err := svc.Activity(context.Background(), actOpts(runs.ActivityByAgent))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Rows) != 1 {
		t.Fatalf("rows %+v", a.Rows)
	}
	row := findRow(t, a, "agt_a")
	if b := binAt(row, 0); b.Done != 2 {
		t.Errorf("bin 0 %+v, want 2 done", b)
	}
	if b := binAt(row, 1); b.Failed != 1 || b.Done != 0 {
		t.Errorf("bin 1 %+v, want 1 failed", b)
	}
	if b := binAt(row, 2); b.Done != 1 || b.Failed != 0 {
		t.Errorf("bin 2 %+v, want the human outcome to win", b)
	}
	if len(row.Bins) != 3 {
		t.Errorf("bins outside the window leaked: %+v", row.Bins)
	}
}

// Tokens count in the bin where the turn started.
func TestActivityTokensByTurnStart(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(10 * time.Minute), turns: []turnSeed{
		{at: actFrom.Add(10 * time.Minute), input: 1000, output: 500},
		{at: actFrom.Add(3*time.Hour + time.Minute), input: 2000, output: 0},
	}})
	a, err := svc.Activity(context.Background(), actOpts(runs.ActivityByAgent))
	if err != nil {
		t.Fatal(err)
	}
	row := findRow(t, a, "agt_a")
	if b := binAt(row, 0); b.Tokens != 1500 || b.CostUSD <= 0 || b.Done != 1 {
		t.Errorf("bin 0 %+v", b)
	}
	b3 := binAt(row, 3)
	if b3.Tokens != 2000 || b3.Done != 0 {
		t.Errorf("bin 3 %+v, want tokens and no run", b3)
	}
}

func TestActivityOwnerRows(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(time.Minute)})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a2", at: actFrom.Add(2 * time.Minute)})
	seedRun(t, svc, st, "ws", runSeed{job: "agt_a", at: actFrom.Add(3 * time.Minute)})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_b1", at: actFrom.Add(4 * time.Minute)})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_orphan", at: actFrom.Add(5 * time.Minute)})
	ctx := context.Background()
	a, err := svc.Activity(ctx, actOpts(runs.ActivityByAgent))
	if err != nil {
		t.Fatal(err)
	}
	if got := binAt(findRow(t, a, "agt_a"), 0); got.Done != 3 {
		t.Errorf("agent a %+v, want its own run and both job runs", got)
	}
	if got := binAt(findRow(t, a, "agt_b"), 0); got.Done != 1 {
		t.Errorf("agent b %+v", got)
	}
	orphan := findRow(t, a, "jb_orphan")
	if orphan.Kind != "job" {
		t.Errorf("an unowned job keeps its own row, kind job: %+v", orphan)
	}
	o := actOpts(runs.ActivityByAgent)
	o.Owner = "agents/a"
	a, err = svc.Activity(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Rows) != 1 || a.Rows[0].Key != "agt_a" {
		t.Errorf("owner filter by name: %+v", a.Rows)
	}
	o.Workspace = "other"
	if a, err = svc.Activity(ctx, o); err != nil || len(a.Rows) != 0 {
		t.Errorf("workspace filter: %+v %v", a, err)
	}
}

func TestActivityGroupJobRuns(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	id1 := seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(time.Minute)})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a2", at: actFrom.Add(2 * time.Hour), status: "failed",
		turns: []turnSeed{{at: actFrom.Add(2 * time.Hour), input: 10, output: 5}}})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_b1", at: actFrom.Add(time.Hour)})
	o := actOpts(runs.ActivityByJob)
	o.Owner = "agt_a"
	a, err := svc.Activity(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Rows) != 2 {
		t.Fatalf("rows %+v", a.Rows)
	}
	if len(a.Runs) != 2 || a.Runs[0].JobID != "jb_a2" || a.Runs[1].ID != id1 {
		t.Fatalf("runs, newest first: %+v", a.Runs)
	}
	if r := a.Runs[0]; r.State != runs.StateFailed || r.Tokens.Billed != 15 {
		t.Errorf("run %+v", r)
	}
	// The agent view keeps the top jobs of a bin.
	o.Group = runs.ActivityByAgent
	if a, err = svc.Activity(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	b := binAt(findRow(t, a, "agt_a"), 0)
	if len(b.Jobs) != 1 || b.Jobs[0].JobID != "jb_a1" || b.Jobs[0].Runs != 1 {
		t.Errorf("jobs of bin 0: %+v", b)
	}
	if a.Runs != nil {
		t.Errorf("the agent view lists no runs")
	}
}

func TestActivityRunCap(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	for i := range 5 {
		seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(time.Duration(i) * time.Minute)})
	}
	o := actOpts(runs.ActivityByJob)
	o.Owner, o.RunCap = "agt_a", 3
	a, err := svc.Activity(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Runs) != 3 || !a.Truncated {
		t.Fatalf("runs %d truncated %v", len(a.Runs), a.Truncated)
	}
}

func TestActivityDueBinned(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	o := actOpts(runs.ActivityByAgent)
	o.Due = []runs.DueFire{
		{WorkspaceID: "ws", JobID: "jb_a2", Spec: "@every 1h", At: actFrom.Add(100 * time.Minute)},
		{WorkspaceID: "ws", JobID: "jb_a1", Spec: "0 9 * * 1-5", At: actFrom.Add(90 * time.Minute)},
		{WorkspaceID: "ws", JobID: "jb_b1", At: actFrom.Add(3 * time.Hour)},
		{WorkspaceID: "ws", JobID: "jb_a1", At: actFrom.Add(25 * time.Hour)},
		{WorkspaceID: "ws", JobID: "jb_gone", At: actFrom.Add(time.Hour)},
	}
	a, err := svc.Activity(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	row := findRow(t, a, "agt_a")
	if len(row.Due) != 1 || row.Due[0].I != 1 || row.Due[0].N != 2 {
		t.Fatalf("agent a due %+v, want 2 fires in bin 1", row.Due)
	}
	want := []runs.ActivityFire{
		{JobID: "jb_a1", At: actFrom.Add(90 * time.Minute), Schedule: "0 9 * * 1-5"},
		{JobID: "jb_a2", At: actFrom.Add(100 * time.Minute), Schedule: "@every 1h"},
	}
	if got := row.Due[0].Fires; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("agent a fires %+v, want %+v in time order", got, want)
	}
	if rb := findRow(t, a, "agt_b"); len(rb.Due) != 1 || rb.Due[0].I != 3 {
		t.Errorf("agent b due %+v", rb.Due)
	}
	if len(a.Rows) != 2 {
		t.Errorf("a fire of an unknown job or outside the window adds no row: %+v", a.Rows)
	}
	_ = st
}

func TestListOwnerFilter(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	own := seedRun(t, svc, st, "ws", runSeed{job: "agt_a", at: actFrom})
	job := seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(time.Minute)})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_b1", at: actFrom.Add(2 * time.Minute)})
	for _, ref := range []string{"agt_a", "agents/a"} {
		got, err := svc.List(context.Background(), runs.ListOpts{Owner: ref})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].ID != job || got[1].ID != own {
			t.Errorf("owner %q: %d runs", ref, len(got))
		}
	}
}

func TestOwnerSpendSince(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom, turns: []turnSeed{
		{at: actFrom, input: 100}, {at: actFrom.Add(48 * time.Hour), input: 7},
	}})
	for _, c := range []struct {
		since time.Time
		want  int64
	}{{actFrom, 107}, {actFrom.Add(time.Hour), 7}, {actFrom.Add(72 * time.Hour), 0}} {
		sp, err := svc.OwnerSpend(context.Background(), "ws", []string{"agt_a"}, c.since)
		if err != nil {
			t.Fatal(err)
		}
		if got := sp[0]["agt_a"].Tokens.Input; got != c.want {
			t.Errorf("since %v: input %d, want %d", c.since, got, c.want)
		}
	}
}

// One call with two times gives the same sums as two calls.
func TestOwnerSpendManyTimes(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom, turns: []turnSeed{
		{at: actFrom, input: 100}, {at: actFrom.Add(48 * time.Hour), input: 7}, {at: actFrom.Add(96 * time.Hour), input: 3},
	}})
	got, err := svc.OwnerSpend(context.Background(), "ws", []string{"agt_a"}, actFrom.Add(72*time.Hour), actFrom.Add(24*time.Hour), actFrom.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{3, 10, 110} {
		if sp := got[i]["agt_a"]; sp.Tokens.Input != want || sp.Turns == 0 {
			t.Errorf("time %d: input %d, want %d", i, sp.Tokens.Input, want)
		}
	}
}

func TestJobActivityRecent14(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	var ids []string
	for i := range 20 {
		ids = append(ids, seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(time.Duration(i) * time.Hour)}))
	}
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a2", at: actFrom})
	got, err := svc.JobActivity(context.Background(), "ws", 14, actFrom.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rec := got["jb_a1"].Recent
	if len(rec) != 14 || rec[0].ID != ids[6] || rec[13].ID != ids[19] {
		t.Fatalf("recent: %d runs, want the newest 14, oldest first", len(rec))
	}
	if len(got["jb_a2"].Recent) != 1 || got["jb_b1"].Recent != nil {
		t.Errorf("per job caps: a2 %d b1 %v", len(got["jb_a2"].Recent), got["jb_b1"].Recent)
	}
	if got["jb_a1"].LastRunAt == nil || !got["jb_a1"].LastRunAt.Equal(actFrom.Add(19*time.Hour)) {
		t.Errorf("last run at %v", got["jb_a1"].LastRunAt)
	}
}

// Each bar carries the tokens of its own turns only.
func TestJobActivityRecentTokens(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	for i := range 4 {
		at := actFrom.Add(time.Duration(i) * time.Hour)
		seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: at, turns: []turnSeed{
			{at: at, input: 1000, output: 100}, {at: at.Add(time.Minute), input: 1000, output: 100},
		}})
	}
	got, err := svc.JobActivity(context.Background(), "ws", 14, actFrom.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range got["jb_a1"].Recent {
		if r.Tokens.Billed != 2200 || r.Tokens.Input != 2000 || r.Tokens.CostUSD <= 0 {
			t.Errorf("bar %d tokens %+v, want 2200 billed", i, r.Tokens)
		}
	}
}

func TestJobActivityRecentState(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom, status: "failed"})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(time.Hour), status: "failed", outcome: "complete"})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(2 * time.Hour), status: "parked", end: runs.EndRequest})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom.Add(3 * time.Hour), status: "parked", end: runs.EndRestart})
	got, err := svc.JobActivity(context.Background(), "ws", 14, actFrom.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []runs.ActivityState{runs.StateFailed, runs.StateDone, runs.StateWaiting, runs.StateSkipped}
	rec := got["jb_a1"].Recent
	if len(rec) != len(want) {
		t.Fatalf("recent %d", len(rec))
	}
	for i, w := range want {
		if rec[i].State != w {
			t.Errorf("bar %d state %s, want %s", i, rec[i].State, w)
		}
	}
	if rec[1].Status != runs.StatusComplete {
		t.Errorf("status shows the effective status, got %s", rec[1].Status)
	}
}

func TestJobActivityTokens30d(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: actFrom, turns: []turnSeed{
		{at: actFrom.Add(-48 * time.Hour), input: 5},
		{at: actFrom, input: 1000, output: 200},
		{at: actFrom.Add(time.Hour), input: 10},
	}})
	got, err := svc.JobActivity(context.Background(), "ws", 14, actFrom.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	tk := got["jb_a1"].Tokens30d
	if tk.Input != 1010 || tk.Output != 200 || tk.Billed != 1210 {
		t.Errorf("tokens %+v, want the turns since the cut only", tk)
	}
}

func TestOwnerActivityCounts7d(t *testing.T) {
	t.Parallel()
	svc, st := newActivityEnv(t)
	since := actFrom
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: since.Add(time.Hour)})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a2", at: since.Add(2 * time.Hour), status: "failed"})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a2", at: since.Add(3 * time.Hour), status: "failed", outcome: "complete"})
	seedRun(t, svc, st, "ws", runSeed{job: "agt_a", at: since.Add(4 * time.Hour)})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_a1", at: since.Add(-time.Hour)})
	seedRun(t, svc, st, "ws", runSeed{job: "jb_b1", at: since.Add(time.Hour)})
	for i := range 30 {
		seedRun(t, svc, st, "ws", runSeed{job: "jb_b1", at: since.Add(-time.Duration(i+2) * time.Hour)})
	}
	got, err := svc.OwnerActivity(context.Background(), "ws", []string{"agt_a", "agt_b"}, 20, since)
	if err != nil {
		t.Fatal(err)
	}
	a := got["agt_a"]
	if a.Runs7d != 4 || a.Ok7d != 3 {
		t.Errorf("agent a runs %d ok %d, want 4 and 3", a.Runs7d, a.Ok7d)
	}
	if len(a.Recent) != 5 || a.Last == nil {
		t.Fatalf("agent a recent %d last %v", len(a.Recent), a.Last)
	}
	if !a.Recent[0].StartedAt.Before(a.Recent[4].StartedAt) {
		t.Errorf("recent must be oldest first")
	}
	if b := got["agt_b"]; len(b.Recent) != 20 || b.Runs7d != 1 {
		t.Errorf("agent b recent %d runs7d %d, want the cap of 20 and an exact count of 1", len(b.Recent), b.Runs7d)
	}
}

// seedRun stores one run with its turns and returns the run id.
func seedRun(t *testing.T, svc *runs.Service, st *store.Store, ws string, s runSeed) string {
	t.Helper()
	ctx := context.Background()
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: ws, JobID: s.job, JobName: s.job, Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	status := cmpOr(s.status, "complete")
	ts := s.at.UTC().Format(time.RFC3339)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.DB().Exec(q, append(args, run.ID)...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE runs SET status=?, outcome=NULLIF(?,''), end_reason=?, started_at=?, ended_at=? WHERE id=?`,
		status, s.outcome, string(s.end), ts, ts)
	exec(`DELETE FROM run_turns WHERE run_id=?`)
	var in, out int64
	for _, u := range s.turns {
		in, out = in+u.input, out+u.output
	}
	exec(`UPDATE runs SET tokens_input=?, tokens_output=?, tokens_model='default' WHERE id=?`, in, out)
	for i, u := range s.turns {
		if _, err := st.DB().Exec(`INSERT INTO run_turns (run_id, n, reason, status, started_at, tokens_input, tokens_output, tokens_model)
			VALUES (?,?,?,?,?,?,?,?)`, run.ID, i+1, "manual", status, u.at.UTC().Format(time.RFC3339), u.input, u.output, "default"); err != nil {
			t.Fatal(err)
		}
	}
	return run.ID
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func newActivityEnv(t *testing.T) (*runs.Service, *store.Store) {
	t.Helper()
	svc, st, _ := openTurnSvc(t)
	seedIndex(t, st, "agt_a", "ws", "agents/a", "agent", "")
	seedIndex(t, st, "agt_b", "ws", "agents/b", "agent", "")
	seedIndex(t, st, "jb_a1", "ws", "agents/a/one", "job", "agt_a")
	seedIndex(t, st, "jb_a2", "ws", "agents/a/two", "job", "agt_a")
	seedIndex(t, st, "jb_b1", "ws", "agents/b/one", "job", "agt_b")
	return svc, st
}

func actOpts(group runs.ActivityGroup) runs.ActivityOpts {
	return runs.ActivityOpts{From: actFrom, To: actFrom.Add(24 * time.Hour), Bin: time.Hour, Group: group}
}

func findRow(t *testing.T, a *runs.Activity, key string) runs.ActivityRow {
	t.Helper()
	for _, r := range a.Rows {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("no row %q in %+v", key, a.Rows)
	return runs.ActivityRow{}
}

func binAt(r runs.ActivityRow, i int) runs.ActivityBin {
	for _, b := range r.Bins {
		if b.I == i {
			return b
		}
	}
	return runs.ActivityBin{I: -1}
}
