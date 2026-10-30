package chains

import (
	"os"

	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/requests"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/visor"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// TestMain lets the test binary run as the holder of a visor session.
func TestMain(m *testing.M) {
	if code, ok := visor.Hold(os.Args); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

type teamStub struct {
	agents []workspaces.TeamAgent
	limits *Limits
}

func (t teamStub) TeamAgents(context.Context, string) ([]workspaces.TeamAgent, error) {
	return t.agents, nil
}

func (t teamStub) ChainLimits(context.Context, string) (Limits, bool, error) {
	return *t.limits, true, nil
}

// reqStub records system requests; plan false answers ErrPlan.
type reqStub struct {
	plan bool
	got  []requests.CreateOpts
}

func (r *reqStub) CreateSystem(_ context.Context, runID string, o requests.CreateOpts) (requests.Request, error) {
	if !r.plan {
		return requests.Request{}, requests.ErrPlan
	}
	r.got = append(r.got, o)
	return requests.Request{ID: "REQ" + runID, RunID: runID, HopID: o.HopID, Source: o.Source}, nil
}

type alertStub struct{ got []alerts.RaiseOpts }

func (a *alertStub) Raise(_ context.Context, o alerts.RaiseOpts) (string, error) {
	a.got = append(a.got, o)
	return "A1", nil
}

// convStub queues every hop and answers TurnReply from a map.
type convStub struct {
	mu      sync.Mutex
	replies map[string]string // run ID → reply text
}

func (c *convStub) SendHop(_ context.Context, o conversations.HopSend) (*conversations.HopResult, error) {
	return &conversations.HopResult{
		SendResult:     conversations.SendResult{Message: conversations.Message{ID: ulid.Make().String()}, QueueID: "q-" + o.HopID},
		ConversationID: "conv-" + o.OperatorID,
	}, nil
}

func (c *convStub) TurnReply(_ context.Context, _, runID string, _ time.Time) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.replies[runID], nil
}

type resumeStub struct {
	mu    sync.Mutex
	calls []runs.ResumeOpts
}

func (r *resumeStub) Resume(_ context.Context, prevID string, o runs.ResumeOpts) (*runs.Run, error) {
	r.mu.Lock()
	r.calls = append(r.calls, o)
	r.mu.Unlock()
	return &runs.Run{ID: prevID, TurnCount: 2}, nil
}

func (r *resumeStub) Fresh(ctx context.Context, prevID string, o runs.ResumeOpts) (*runs.Run, error) {
	return r.Resume(ctx, prevID, o)
}

type fixture struct {
	svc    *Service
	runs   *runs.Service
	conv   *convStub
	resume *resumeStub
	req    *reqStub
	alerts *alertStub
	limits *Limits
}

// TestReplyResumesCaller pins C.4 and E.2: the target turn ends with text, a
// reply hop goes to the parked caller, and the caller resumes once with the
// reply and the chain frame.
func TestReplyResumesCaller(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	ceo := f.start(t, "ceo", nil)
	ask := f.ask(t, ceo, "cfo", KindAsk)
	if ask.Next != "end_turn" || ask.Hop.Status != HopQueued {
		t.Fatalf("ask %+v", ask)
	}
	cfo := f.start(t, "cfo", ask)
	if turn, _ := f.runs.Turn(ctx, cfo.ID, 1); turn.TraceID != ask.Hop.TraceID || turn.HopID != ask.Hop.ID || turn.Depth != 1 {
		t.Fatalf("target turn %+v", turn)
	}
	f.end(t, ceo, runs.StatusComplete)
	if got, _ := f.runs.Get(ctx, ceo.ID); got.Status != runs.StatusParked || got.EndReason != runs.EndAsk {
		t.Fatalf("caller %s/%s", got.Status, got.EndReason)
	}
	f.conv.replies[cfo.ID] = "The budget is 10k."
	f.end(t, cfo, runs.StatusComplete)

	h, _ := f.svc.Hop(ctx, ask.Hop.ID)
	if h.Status != HopAnswered {
		t.Fatalf("ask status %s", h.Status)
	}
	replies, err := f.svc.queryHops(ctx, `WHERE reply_of=?`, ask.Hop.ID)
	if err != nil || len(replies) != 1 {
		t.Fatalf("replies %v %v", replies, err)
	}
	r := replies[0]
	if r.Kind != KindReply || r.Text != "The budget is 10k." || r.DeliveryState != DeliveryDelivered || r.To.RunID != ceo.ID || r.To.Turn != 2 {
		t.Fatalf("reply %+v", r)
	}
	if len(f.resume.calls) != 1 {
		t.Fatalf("resumes %d", len(f.resume.calls))
	}
	o := f.resume.calls[0]
	if o.Reason != runs.ReasonReply || o.HopID != r.ID || o.TraceID != ask.Hop.TraceID ||
		!strings.Contains(o.Prompt, "Reply from agents/cfo (hop "+r.ID) || !strings.Contains(o.Prompt, "The budget is 10k.") {
		t.Fatalf("resume %+v", o)
	}
	if len(o.Path) != 1 || o.Path[0] != "ag-ceo" {
		t.Fatalf("resume path %v", o.Path)
	}
}

// TestReplyBeforeCallerEnds: a reply that comes in while the caller turn is
// still open waits; the caller parks and resumes at its turn end.
func TestReplyBeforeCallerEnds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ceo := f.start(t, "ceo", nil)
	ask := f.ask(t, ceo, "cfo", KindAsk)
	cfo := f.start(t, "cfo", ask)
	f.end(t, cfo, runs.StatusComplete)
	if len(f.resume.calls) != 0 {
		t.Fatal("resumed a live caller")
	}
	f.end(t, ceo, runs.StatusComplete)
	if len(f.resume.calls) != 1 || !strings.Contains(f.resume.calls[0].Prompt, "ended the turn with no reply") {
		t.Fatalf("resumes %+v", f.resume.calls)
	}
}

// TestFailedTargetReply: a target that fails gives the caller a failure reply.
func TestFailedTargetReply(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	ceo := f.start(t, "ceo", nil)
	ask := f.ask(t, ceo, "cfo", KindAsk)
	cfo := f.start(t, "cfo", ask)
	f.end(t, ceo, runs.StatusComplete)
	f.end(t, cfo, runs.StatusFailed)
	if h, _ := f.svc.Hop(ctx, ask.Hop.ID); h.Status != HopFailed {
		t.Fatalf("ask status %s", h.Status)
	}
	if len(f.resume.calls) != 1 || !strings.Contains(f.resume.calls[0].Prompt, "agents/cfo did not answer") {
		t.Fatalf("resumes %+v", f.resume.calls)
	}
}

// TestLaunchFailedReply: a target that does not launch (budget) fails the
// hop and gives the caller a failure reply.
func TestLaunchFailedReply(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	ceo := f.start(t, "ceo", nil)
	ask := f.ask(t, ceo, "cfo", KindAsk)
	f.end(t, ceo, runs.StatusComplete)
	if err := f.svc.LaunchFailed(ctx, ask.Hop.ID, runs.ErrBudgetExceeded); err != nil {
		t.Fatal(err)
	}
	if h, _ := f.svc.Hop(ctx, ask.Hop.ID); h.Status != HopFailed || h.RefuseReason != RefuseBudget {
		t.Fatalf("hop %+v", h)
	}
	if len(f.resume.calls) != 1 || !strings.Contains(f.resume.calls[0].Prompt, "over its budget") {
		t.Fatalf("resumes %+v", f.resume.calls)
	}
}

// TestHandoffNoReply: a hand-off does not park the caller and has no reply.
// The hop is done after the first target turn.
func TestHandoffNoReply(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	ceo := f.start(t, "ceo", nil)
	ho := f.ask(t, ceo, "legal", KindHandoff)
	if ho.Next != "" {
		t.Fatalf("hand-off next %q", ho.Next)
	}
	legal := f.start(t, "legal", ho)
	f.end(t, ceo, runs.StatusComplete)
	if st := f.status(t, ceo.ID); st != runs.StatusComplete {
		t.Fatalf("caller %s", st)
	}
	f.end(t, legal, runs.StatusComplete)
	if h, _ := f.svc.Hop(ctx, ho.Hop.ID); h.Status != HopDone {
		t.Fatalf("hand-off status %s", h.Status)
	}
	if replies, _ := f.svc.queryHops(ctx, `WHERE reply_of=?`, ho.Hop.ID); len(replies) != 0 || len(f.resume.calls) != 0 {
		t.Fatalf("replies %d resumes %d", len(replies), len(f.resume.calls))
	}
}

// TestParkedTargetKeepsAsk: a target that asks in turn parks; its ask stays
// open until the target ends a turn with no open ask.
func TestParkedTargetKeepsAsk(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	ceo := f.start(t, "ceo", nil)
	a1 := f.ask(t, ceo, "cfo", KindAsk)
	cfo := f.start(t, "cfo", a1)
	f.end(t, ceo, runs.StatusComplete)
	a2 := f.ask(t, cfo, "legal", KindAsk)
	if a2.Hop.Depth != 2 || a2.Hop.TraceID != a1.Hop.TraceID {
		t.Fatalf("second hop %+v", a2.Hop)
	}
	f.end(t, cfo, runs.StatusComplete)
	if st := f.status(t, cfo.ID); st != runs.StatusParked {
		t.Fatalf("target %s", st)
	}
	if h, _ := f.svc.Hop(ctx, a1.Hop.ID); h.Status != HopRunning {
		t.Fatalf("first ask %s", h.Status)
	}
	// A cycle back to ceo is refused.
	if _, err := f.svc.Ask(ctx, AskOpts{Workspace: "ws1", To: "ceo", Text: "x", Actor: &tools.Actor{Kind: EndAgent, RunID: cfo.ID}}); err == nil {
		t.Fatal("cycle allowed")
	}
}

// heldAsk builds ceo → cfo → legal with max_hops 1, so the ask to legal is
// held for a person.
func (f *fixture) heldAsk(t *testing.T) (cfo *runs.Run, held *AskResult) {
	t.Helper()
	f.limits.MaxHops = 1
	ceo := f.start(t, "ceo", nil)
	a1 := f.ask(t, ceo, "cfo", KindAsk)
	cfo = f.start(t, "cfo", a1)
	f.end(t, ceo, runs.StatusComplete)
	held = f.ask(t, cfo, "legal", KindAsk)
	return cfo, held
}

// TestLimitHoldsHop pins F.1: the hop at a limit is held and a chain request
// goes to the caller run.
func TestLimitHoldsHop(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfo, held := f.heldAsk(t)
	if !held.Held || held.Hop.Status != HopHeld || held.Next != "end_turn" || held.RequestID != "REQ"+cfo.ID || held.Hop.RefuseReason != RefuseHops {
		t.Fatalf("held %+v", held)
	}
	o := f.req.got[0]
	if o.Source != requests.SourceChain || o.HopID != held.Hop.ID || len(o.Forms) != 1 {
		t.Fatalf("request %+v", o)
	}
	if fo := o.Forms[0]; fo.Title != "Agents reached the limit of 1 hand-overs" || !strings.Contains(fo.Summary, "ceo → cfo → legal") || fo.Target != "agents/legal" {
		t.Fatalf("form %+v", fo)
	}
	if c, _ := f.svc.chain(context.Background(), held.Hop.TraceID); c.Status != StatusHeld {
		t.Fatalf("chain %s", c.Status)
	}
}

// TestLimitDecisions pins F.2.
func TestLimitDecisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, c := range []struct {
		decision requests.Decision
		hop      HopStatus
		chain    string
		maxHops  int
	}{
		{requests.DecisionApprove, HopQueued, StatusOpen, 2},
		{requests.DecisionReject, HopStopped, StatusStopped, 1},
		{requests.DecisionRespond, HopStopped, StatusOpen, 1},
		{requests.DecisionExpired, HopStopped, StatusStopped, 1},
	} {
		f := newFixture(t)
		_, held := f.heldAsk(t)
		r := requests.Request{HopID: held.Hop.ID, Source: requests.SourceChain, Status: requests.StatusAnswered,
			Forms: []requests.Form{{Type: requests.TypeConfirm, Answer: &requests.FormAnswer{Decision: c.decision}}}}
		if c.decision == requests.DecisionExpired {
			r.Status, r.Forms[0].Answer = requests.StatusExpired, nil
		}
		if err := f.svc.RequestDecided(ctx, r); err != nil {
			t.Fatalf("%s: %v", c.decision, err)
		}
		h, _ := f.svc.Hop(ctx, held.Hop.ID)
		ch, _ := f.svc.chain(ctx, held.Hop.TraceID)
		if h.Status != c.hop || ch.Status != c.chain || ch.Limits.MaxHops != c.maxHops {
			t.Fatalf("%s: hop %s chain %s max_hops %d", c.decision, h.Status, ch.Status, ch.Limits.MaxHops)
		}
	}
}

// TestLimitNoGrant pins F.3: no request grant refuses the hop and raises
// chain_limited.
func TestLimitNoGrant(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.req.plan = false
	f.limits.MaxHops = 1
	ceo := f.start(t, "ceo", nil)
	a1 := f.ask(t, ceo, "cfo", KindAsk)
	cfo := f.start(t, "cfo", a1)
	_, err := f.svc.Ask(context.Background(), AskOpts{Workspace: "ws1", To: "legal", Text: "x", Actor: &tools.Actor{Kind: EndAgent, RunID: cfo.ID}})
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != CodeLimit || ce.Reason != RefuseHops || ce.HopID == "" {
		t.Fatalf("err %v", err)
	}
	if h, _ := f.svc.Hop(context.Background(), ce.HopID); h.Status != HopRefused {
		t.Fatalf("hop %s", h.Status)
	}
	if len(f.alerts.got) != 1 || f.alerts.got[0].Kind != alerts.KindChainLimited || f.alerts.got[0].TraceID != a1.Hop.TraceID {
		t.Fatalf("alerts %+v", f.alerts.got)
	}
}

// TestStopUnwinds pins C.5: the live target ends, the parked caller gets one
// stop notice, and a later ask is refused.
func TestStopUnwinds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	ceo := f.start(t, "ceo", nil)
	a1 := f.ask(t, ceo, "cfo", KindAsk)
	cfo := f.start(t, "cfo", a1)
	a2 := f.ask(t, ceo, "legal", KindHandoff)
	f.end(t, ceo, runs.StatusComplete)
	res, err := f.svc.Stop(ctx, a1.Hop.TraceID, "user", "Wrong plan.")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.KilledRuns) != 1 || res.KilledRuns[0] != cfo.ID || len(res.CancelledHops) != 1 || res.CancelledHops[0] != a2.Hop.ID ||
		len(res.Unwinding) != 1 || res.Unwinding[0] != ceo.ID {
		t.Fatalf("stop %+v", res)
	}
	if f.status(t, cfo.ID) == runs.StatusRunning {
		t.Fatal("target still running")
	}
	if len(f.resume.calls) != 1 || !strings.Contains(f.resume.calls[0].Prompt, stoppedReplyText) {
		t.Fatalf("resumes %+v", f.resume.calls)
	}
	_, err = f.svc.Ask(ctx, AskOpts{Workspace: "ws1", To: "legal", Text: "again", Actor: &tools.Actor{Kind: EndAgent, RunID: ceo.ID}})
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != CodeStopped {
		t.Fatalf("ask after stop: %v", err)
	}
}

// TestDoctorRateHits pins chain_rate_hits: three rate refusals of one pair in
// 24 h give one finding.
func TestDoctorRateHits(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.limits.PairPerHour = 1
	ceo := f.start(t, "ceo", nil)
	f.ask(t, ceo, "cfo", KindAsk)
	for i := range 3 {
		if got := f.svc.Doctor(context.Background()); len(got) != 0 {
			t.Fatalf("finding after %d refusals: %+v", i, got)
		}
		_, err := f.svc.Ask(context.Background(), AskOpts{Workspace: "ws1", To: "cfo", Text: "Again?", Actor: &tools.Actor{Kind: EndAgent, RunID: ceo.ID}})
		var ce *Error
		if !errors.As(err, &ce) || ce.Reason != RefuseRate {
			t.Fatalf("ask %d: %v", i, err)
		}
	}
	got := f.svc.Doctor(context.Background())
	if len(got) != 1 || got[0].Code != "chain_rate_hits" || !strings.Contains(got[0].Message, "agents/ceo asked agents/cfo too often 3 times") {
		t.Fatalf("findings %+v", got)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	limits := workspaces.DefaultChainLimits()
	team := teamStub{limits: &limits}
	for _, n := range []string{"ceo", "cfo", "legal"} {
		if _, err := st.DB().Exec(`INSERT INTO job_index (id, workspace_id, name, path, kind, updated_at) VALUES (?,?,?,?,?,?)`,
			"ag-"+n, "ws1", "agents/"+n, "agents/"+n+"/AGENT.md", "agent", now); err != nil {
			t.Fatal(err)
		}
		team.agents = append(team.agents, workspaces.TeamAgent{ID: "ag-" + n, Name: n, Description: n})
	}
	f := &fixture{runs: runs.New(st, nil, nil), conv: &convStub{replies: map[string]string{}}, resume: &resumeStub{},
		req: &reqStub{plan: true}, alerts: &alertStub{}, limits: &limits}
	f.svc = NewService(st, team, f.conv, f.runs, f.req, nil)
	f.svc.WithAlerts(f.alerts)
	f.runs.SetParker(runs.NewParker(f.runs, f.resume, f.svc))
	return f
}

// start creates a running agent run, as the queue does for a hop.
func (f *fixture) start(t *testing.T, agent string, h *AskResult) *runs.Run {
	t.Helper()
	o := runs.CreateOpts{WorkspaceID: "ws1", JobID: "ag-" + agent, JobName: "agents/" + agent, Kind: runs.KindAgent, Harness: "claude", SessionID: "s-" + agent}
	if h != nil {
		o.TraceID, o.HopID, o.ChainPath = h.Hop.TraceID, h.Hop.ID, h.Hop.pathIDs
	}
	run, err := f.runs.Create(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if h != nil {
		if err := f.svc.Bind(context.Background(), h.Hop.ID, run.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	return run
}

func (f *fixture) ask(t *testing.T, from *runs.Run, to string, kind Kind) *AskResult {
	t.Helper()
	res, err := f.svc.Ask(context.Background(), AskOpts{Workspace: "ws1", To: to, Kind: kind, Text: "Question for " + to,
		Actor: &tools.Actor{Kind: EndAgent, RunID: from.ID, WorkspaceID: "ws1"}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (f *fixture) end(t *testing.T, run *runs.Run, st runs.Status) {
	t.Helper()
	if err := f.runs.Terminate(context.Background(), run.ID, st, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) status(t *testing.T, id string) runs.Status {
	t.Helper()
	r, err := f.runs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r.Status
}
