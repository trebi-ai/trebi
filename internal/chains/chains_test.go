package chains

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

type teamStub struct{ agents []workspaces.TeamAgent }

func (t teamStub) TeamAgents(context.Context, string) ([]workspaces.TeamAgent, error) {
	return t.agents, nil
}

func (teamStub) ChainLimits(context.Context, string) (Limits, bool, error) {
	return workspaces.DefaultChainLimits(), true, nil
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

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	team := teamStub{}
	for _, n := range []string{"ceo", "cfo", "legal"} {
		if _, err := st.DB().Exec(`INSERT INTO job_index (id, workspace_id, name, path, kind, updated_at) VALUES (?,?,?,?,?,?)`,
			"ag-"+n, "ws1", "agents/"+n, "agents/"+n+"/AGENT.md", "agent", now); err != nil {
			t.Fatal(err)
		}
		team.agents = append(team.agents, workspaces.TeamAgent{ID: "ag-" + n, Name: n, Description: n})
	}
	f := &fixture{runs: runs.New(st, nil, nil), conv: &convStub{replies: map[string]string{}}, resume: &resumeStub{}}
	f.svc = NewService(st, team, f.conv, f.runs, nil)
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
