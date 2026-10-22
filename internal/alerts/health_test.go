package alerts_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/store"
)

// healthFixture is one job with a clock that the test moves.
type healthFixture struct {
	t   *testing.T
	svc *alerts.Service
	st  *store.Store
	now time.Time
}

const (
	hWS  = "ws"
	hJob = "job_0000000001"
)

type hRun struct {
	id, reason, status, end, harness string
	start, end2                      time.Time
	maxMS                            int64
}

// put writes or replaces a run row.
func (f *healthFixture) put(r hRun) {
	f.t.Helper()
	if _, err := f.st.DB().Exec(`INSERT OR REPLACE INTO runs (id,workspace_id,job_id,kind,reason,status,end_reason,harness,
		trace_id,started_at,ended_at,max_duration_ms,updated_at) VALUES (?,?,?,'job',?,?,?,?,'trace',?,?,?,?)`,
		r.id, hWS, hJob, r.reason, r.status, r.end, r.harness, ts(r.start), ts(r.end2), r.maxMS, ts(r.start)); err != nil {
		f.t.Fatal(err)
	}
}

// end writes the run and sends its terminal frame at the end time.
func (f *healthFixture) end(r hRun, extra map[string]string) {
	f.t.Helper()
	f.put(r)
	f.now = r.end2
	attrs := map[string]string{
		"status": r.status, "end_reason": r.end, "job_id": hJob, "workspace": hWS,
		"job_name": "agents/ellis/notification-summary", "kind": "job", "harness": r.harness,
	}
	for k, v := range extra {
		attrs[k] = v
	}
	raw, _ := json.Marshal(attrs)
	topic := "run.updated"
	if r.status == "failed" {
		topic = "run.failed"
	}
	f.svc.HandleEvent(context.Background(), store.StreamEvent{Topic: topic, ID: r.id, Attrs: raw})
}

// skip writes a skipped run that names its blocker, and sends its frame.
func (f *healthFixture) skip(id string, at time.Time, because string) {
	f.t.Helper()
	if _, err := f.st.DB().Exec(`INSERT INTO run_events (run_id, at, kind, source, detail_json) VALUES (?,?,?,?,?)`,
		id, ts(at), "skip", "runner", `{"because":"`+because+`","policy":"skip"}`); err != nil {
		f.t.Fatal(err)
	}
	f.end(hRun{id: id, reason: "schedule", status: "skipped", end: "skip", harness: "grok", start: at, end2: at.Add(5 * time.Millisecond)}, nil)
}

// open returns the open items of kind.
func (f *healthFixture) open(kind alerts.Kind) []alerts.InboxItem {
	f.t.Helper()
	items, err := listAll(context.Background(), f.svc, alerts.ListOpts{Limit: 100, InboxFilter: alerts.InboxFilter{View: alerts.ViewInbox}})
	if err != nil {
		f.t.Fatal(err)
	}
	var out []alerts.InboxItem
	for _, it := range items {
		if it.AlertKind == string(kind) {
			full, err := f.svc.Get(context.Background(), it.ID)
			if err != nil {
				f.t.Fatal(err)
			}
			out = append(out, *full)
		}
	}
	return out
}

func (f *healthFixture) count(kind alerts.Kind) int {
	f.t.Helper()
	var n int
	if err := f.st.DB().QueryRow(`SELECT COUNT(*) FROM inbox_items WHERE alert_kind=?`, string(kind)).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

var hT0 = time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)

func TestJobStreak(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		runs     []hRun
		bad      int
		complete bool
		live     bool
	}{
		{"no runs", nil, 0, false, false},
		{"complete only", []hRun{{status: "complete", reason: "schedule"}}, 0, true, false},
		{"bad after complete", []hRun{
			{status: "complete", reason: "schedule"}, {status: "skipped", reason: "schedule"},
			{status: "exited", reason: "trigger"}, {status: "timed_out", reason: "schedule"}, {status: "failed", reason: "schedule"},
		}, 4, true, false},
		{"complete resets", []hRun{
			{status: "failed", reason: "schedule"}, {status: "failed", reason: "schedule"}, {status: "complete", reason: "manual"},
			{status: "skipped", reason: "schedule"},
		}, 1, true, false},
		{"manual and replay ignored", []hRun{
			{status: "complete", reason: "schedule"}, {status: "failed", reason: "manual"}, {status: "exited", reason: "replay"},
		}, 0, true, false},
		{"running is live, not bad", []hRun{
			{status: "complete", reason: "schedule"}, {status: "failed", reason: "schedule"}, {status: "running", reason: "schedule"},
		}, 1, true, true},
		{"parked on a request", []hRun{
			{status: "complete", reason: "schedule"}, {status: "parked", end: "request", reason: "schedule"},
			{status: "parked", end: "request", reason: "schedule"},
		}, 1, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newHealthFixture(t, day(30))
			for i, r := range tc.runs {
				r.id = "r" + string(rune('a'+i))
				r.start = day(i)
				if r.status != "running" {
					r.end2 = r.start.Add(time.Minute)
				}
				// The last parked run is fresh; the ones before it wait long.
				if r.status == "parked" && i == len(tc.runs)-1 {
					r.start, r.end2 = day(30).Add(-time.Hour), day(30).Add(-time.Hour)
				}
				f.put(r)
			}
			st, err := f.svc.JobStreak(context.Background(), hJob, f.now)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Bad) != tc.bad || (st.LastComplete != nil) != tc.complete || st.Live != tc.live {
				t.Fatalf("bad=%d complete=%v live=%v, want %d %v %v", len(st.Bad), st.LastComplete != nil, st.Live, tc.bad, tc.complete, tc.live)
			}
		})
	}
}

type handledRouter struct{ n int }

func (r *handledRouter) RouteFailure(context.Context, alerts.RunFailure) (bool, error) {
	r.n++
	return true, nil
}

func TestJobStalled(t *testing.T) {
	t.Parallel()
	f := newHealthFixture(t, day(0))
	router := &handledRouter{}
	f.svc.SetFailureRouter(router)
	owner := map[string]string{"owner_id": "agt_owner"}
	f.end(hRun{id: "ok", reason: "schedule", status: "complete", end: "sentinel", harness: "claude", start: day(0), end2: day(0).Add(time.Minute)}, nil)
	f.end(hRun{id: "m1", reason: "manual", status: "failed", end: "launch", harness: "grok", start: day(1), end2: day(1).Add(time.Minute)}, owner)
	f.end(hRun{id: "m2", reason: "manual", status: "failed", end: "launch", harness: "grok", start: day(2), end2: day(2).Add(time.Minute)}, owner)
	if n := f.count(alerts.KindJobStalled); n != 0 {
		t.Fatalf("manual runs raised job_stalled %d", n)
	}
	f.end(hRun{id: "s1", reason: "schedule", status: "failed", end: "launch", harness: "grok", start: day(3), end2: day(3).Add(time.Minute)}, owner)
	if n := f.count(alerts.KindJobStalled); n != 0 {
		t.Fatalf("one bad run raised job_stalled %d", n)
	}
	f.end(hRun{id: "s2", reason: "schedule", status: "timed_out", end: "max_duration", harness: "grok", start: day(4), end2: day(4).Add(time.Hour)}, owner)
	open := f.open(alerts.KindJobStalled)
	if len(open) != 1 {
		t.Fatalf("job_stalled open=%d want 1", len(open))
	}
	if router.n == 0 || f.count(alerts.KindRunFailed) != 0 {
		t.Fatalf("the owner takes run_failed: router=%d run_failed=%d", router.n, f.count(alerts.KindRunFailed))
	}
	it := open[0]
	if it.RunID != "s2" || !strings.Contains(it.Title, "notification-summary stopped working") ||
		!strings.Contains(it.Body, "claude → grok on 2026-10-04") || !strings.Contains(it.Body, "2 automatic runs") {
		t.Fatalf("item %+v", it)
	}
	f.end(hRun{id: "s3", reason: "trigger", status: "complete", end: "sentinel", harness: "grok", start: day(5), end2: day(5).Add(time.Minute)}, nil)
	if n := len(f.open(alerts.KindJobStalled)); n != 0 {
		t.Fatalf("complete run left job_stalled open=%d", n)
	}
}

func TestRunStuckSweep(t *testing.T) {
	t.Parallel()
	f := newHealthFixture(t, day(0))
	f.put(hRun{id: "hung", reason: "schedule", status: "running", harness: "grok", start: day(0)})
	f.now = day(0).Add(23 * time.Hour)
	f.svc.SweepHealth(context.Background())
	if n := f.count(alerts.KindRunStuck); n != 0 {
		t.Fatalf("23h raised run_stuck %d", n)
	}
	f.now = day(0).Add(25 * time.Hour)
	f.svc.SweepHealth(context.Background())
	f.now = day(0).Add(48 * time.Hour)
	f.svc.SweepHealth(context.Background())
	open := f.open(alerts.KindRunStuck)
	if len(open) != 1 || f.count(alerts.KindRunStuck) != 1 {
		t.Fatalf("run_stuck open=%d total=%d want 1", len(open), f.count(alerts.KindRunStuck))
	}
	if acts := open[0].Actions; len(acts) == 0 || acts[0].Kind != alerts.ActionStopRun || acts[0].Route != "/v1/runs/hung/kill" {
		t.Fatalf("actions %+v", acts)
	}
	f.end(hRun{id: "hung", reason: "schedule", status: "exited", end: "kill", harness: "grok", start: day(0), end2: day(2)}, nil)
	if n := len(f.open(alerts.KindRunStuck)); n != 0 {
		t.Fatalf("run end left run_stuck open=%d", n)
	}
}

func TestRunStuckMedian(t *testing.T) {
	t.Parallel()
	f := newHealthFixture(t, day(5))
	for i := range 3 {
		f.put(hRun{id: "c" + string(rune('a'+i)), reason: "schedule", status: "complete", start: day(i), end2: day(i).Add(2 * time.Minute)})
	}
	// A time limit set: only the median rule applies, with a floor of 1 hour.
	f.put(hRun{id: "slow", reason: "schedule", status: "running", start: day(5), maxMS: int64(48 * time.Hour / time.Millisecond)})
	f.now = day(5).Add(50 * time.Minute)
	f.svc.SweepHealth(context.Background())
	if n := f.count(alerts.KindRunStuck); n != 0 {
		t.Fatalf("under the floor raised %d", n)
	}
	f.now = day(5).Add(61 * time.Minute)
	f.svc.SweepHealth(context.Background())
	if open := f.open(alerts.KindRunStuck); len(open) != 1 || !strings.Contains(open[0].Body, "about 2 minutes") {
		t.Fatalf("run_stuck %+v", open)
	}
}

func TestSkipBlocked(t *testing.T) {
	t.Parallel()
	f := newHealthFixture(t, day(0))
	f.put(hRun{id: "blocker", reason: "schedule", status: "running", harness: "grok", start: day(0)})
	f.skip("k1", day(1), "blocker")
	if n := f.count(alerts.KindSkipBlocked); n != 0 {
		t.Fatalf("first skip raised %d", n)
	}
	f.skip("k2", day(3), "blocker")
	open := f.open(alerts.KindSkipBlocked)
	if len(open) != 1 || open[0].RunID != "blocker" || !strings.Contains(open[0].Body, "Run blocker is still running after 3 days") {
		t.Fatalf("skip_blocked %+v", open)
	}
	f.end(hRun{id: "blocker", reason: "schedule", status: "exited", end: "kill", harness: "grok", start: day(0), end2: day(3).Add(time.Hour)}, nil)
	if n := len(f.open(alerts.KindSkipBlocked)); n != 0 {
		t.Fatalf("blocker end left skip_blocked open=%d", n)
	}
}

func TestRequestReminder(t *testing.T) {
	t.Parallel()
	f := newHealthFixture(t, day(0))
	if _, err := f.st.DB().Exec(`INSERT INTO requests (id,run_id,workspace_id,job_id,kind,title,status,created_at,updated_at)
		VALUES ('req1','run1',?,?,'approval','Publish the post?','pending',?,?)`, hWS, hJob, ts(day(0)), ts(day(0))); err != nil {
		t.Fatal(err)
	}
	f.now = day(1)
	f.svc.SweepHealth(context.Background())
	if n := f.count(alerts.KindRequestReminder); n != 0 {
		t.Fatalf("1 day raised %d", n)
	}
	f.now = day(2).Add(time.Hour)
	f.svc.SweepHealth(context.Background())
	f.now = day(3)
	f.svc.SweepHealth(context.Background())
	if open := f.open(alerts.KindRequestReminder); len(open) != 1 || f.count(alerts.KindRequestReminder) != 1 ||
		!strings.Contains(open[0].Title, "Publish the post?") || !strings.Contains(open[0].Body, "2 days") {
		t.Fatalf("reminder %+v", open)
	}
	f.svc.HandleEvent(context.Background(), store.StreamEvent{Topic: "request.answered", ID: "req1", Attrs: json.RawMessage(`{}`)})
	if n := len(f.open(alerts.KindRequestReminder)); n != 0 {
		t.Fatalf("answer left the reminder open=%d", n)
	}
}

func TestRepeatAlertShowsAge(t *testing.T) {
	t.Parallel()
	f := newHealthFixture(t, day(0))
	ctx := context.Background()
	opts := alerts.RaiseOpts{Kind: alerts.KindMemoryInvalid, WorkspaceID: hWS, JobID: hJob, Body: "bad"}
	id, err := f.svc.Raise(ctx, opts)
	if err != nil || id == "" {
		t.Fatalf("%q %v", id, err)
	}
	if _, err := f.st.DB().Exec(`UPDATE inbox_items SET read_at=? WHERE id=?`, ts(day(0)), id); err != nil {
		t.Fatal(err)
	}
	f.now = day(4)
	if _, err := f.svc.Raise(ctx, opts); err != nil {
		t.Fatal(err)
	}
	it, err := f.svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if it.Read || !strings.HasSuffix(it.Title, "(still open for 4 days)") {
		t.Fatalf("repeat item read=%v title=%q", it.Read, it.Title)
	}
}

// TestReplayNotificationSummary loads the 2026-10 fault of
// agents/ellis/notification-summary and checks that the user gets an alert
// on 2026-10-12 at the latest.
func TestReplayNotificationSummary(t *testing.T) {
	t.Parallel()
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	f := newHealthFixture(t, at("2026-10-05T11:00:00Z"))
	owner := map[string]string{"owner_id": "agt_ellis"}
	f.svc.SetFailureRouter(&handledRouter{})
	sweep := func(s string) {
		f.now = at(s)
		f.svc.SweepHealth(context.Background())
	}
	f.end(hRun{id: "01M45VGX", reason: "manual", status: "complete", end: "sentinel", harness: "claude",
		start: at("2026-10-05T11:00:00Z"), end2: at("2026-10-05T11:01:31Z")}, nil)
	f.end(hRun{id: "01M4B0AB", reason: "manual", status: "complete", end: "sentinel", harness: "claude",
		start: at("2026-10-07T11:00:00Z"), end2: at("2026-10-07T11:01:31Z")}, nil)
	f.end(hRun{id: "01M4JQGV", reason: "schedule", status: "failed", end: "launch", harness: "grok",
		start: at("2026-10-10T11:00:11Z"), end2: at("2026-10-10T11:01:42Z")}, owner)
	replay := hRun{id: "01M4JRH9", reason: "replay", status: "running", harness: "grok", start: at("2026-10-10T11:17:54Z")}
	f.put(replay)
	sweep("2026-10-11T12:00:00Z")
	f.skip("01M4QWA8", at("2026-10-12T11:00:11Z"), replay.id)
	replay.status, replay.end, replay.end2 = "exited", "session_gone", at("2026-10-12T15:33:46Z")
	f.end(replay, owner)

	deadline := at("2026-10-13T00:00:00Z")
	var stalled alerts.InboxItem
	for _, it := range f.open(alerts.KindJobStalled) {
		stalled = it
	}
	if stalled.ID == "" || !stalled.CreatedAt.Before(deadline) {
		t.Fatalf("job_stalled %+v, want one before %s", stalled, deadline)
	}
	if !strings.Contains(stalled.Body, "claude → grok on 2026-10-10") {
		t.Fatalf("body %q", stalled.Body)
	}
	if n := f.count(alerts.KindRunStuck); n != 1 {
		t.Fatalf("the replay hung 2 days: run_stuck=%d want 1", n)
	}

	// 2026-10-14: a run hangs, and the next runs skip.
	hung := hRun{id: "01M4X13T", reason: "schedule", status: "running", harness: "grok", start: at("2026-10-14T11:00:14Z")}
	f.put(hung)
	sweep("2026-10-15T12:00:00Z")
	f.skip("01M54R9S", at("2026-10-17T11:00:08Z"), hung.id)
	f.skip("01M59X3B", at("2026-10-19T11:00:12Z"), hung.id)
	blocked := f.open(alerts.KindSkipBlocked)
	if len(blocked) != 1 || blocked[0].RunID != hung.id || !strings.Contains(blocked[0].Body, "after 4 days") {
		t.Fatalf("skip_blocked %+v", blocked)
	}
	if n := f.count(alerts.KindRunStuck); n != 2 {
		t.Fatalf("run_stuck=%d want 2", n)
	}
}

func newHealthFixture(t *testing.T, start time.Time) *healthFixture {
	t.Helper()
	f := &healthFixture{t: t, now: start}
	f.svc, f.st = testSvc(t, alerts.Options{Now: func() time.Time { return f.now }})
	if _, err := f.st.DB().Exec(`INSERT INTO job_index (id,workspace_id,name,path,kind,active,updated_at)
		VALUES (?,?,'agents/ellis/notification-summary','/x','job',1,datetime())`, hJob, hWS); err != nil {
		t.Fatal(err)
	}
	return f
}

func ts(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func day(n int) time.Time { return hT0.Add(time.Duration(n) * 24 * time.Hour) }
