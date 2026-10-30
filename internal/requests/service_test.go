package requests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/entitle"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
)

type fakeRuns struct {
	mu     sync.Mutex
	runs   map[string]*runs.Run
	gate   runs.ParkGate
	parker *runs.Parker
	events []runEvent
}

func (f *fakeRuns) put(r runs.Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[r.ID] = &r
}

func (f *fakeRuns) setStatus(id string, st runs.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[id].Status = st
}

func (f *fakeRuns) Get(_ context.Context, id string) (*runs.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, fmt.Errorf("run %s: not found", id)
	}
	c := *r
	return &c, nil
}

func (f *fakeRuns) Unpark(ctx context.Context, id string, end runs.EndReason) error {
	if waiting, ready, _ := f.gate.Open(ctx, id); waiting || ready {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runs[id]
	if r == nil || r.Status != runs.StatusParked {
		return runs.ErrNotParked
	}
	r.Status, r.EndReason = runs.StatusComplete, end
	return nil
}

func (f *fakeRuns) AddEvent(_ context.Context, runID, kind, _ string, detail any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, runEvent{runID, kind, detail})
}

type runEvent struct {
	runID, kind string
	detail      any
}

func (f *fakeRuns) Deliver(ctx context.Context, id string, extra ...runs.ParkDelivery) (runs.DeliverResult, error) {
	return f.parker.Deliver(ctx, id, extra...)
}

type launchCall struct {
	path, prevID, prompt string
	reason               runs.RunReason
}

type fakeLauncher struct {
	mu        sync.Mutex
	calls     []launchCall
	resumeErr error
	freshErr  error
	rs        *fakeRuns
}

func (f *fakeLauncher) record(c launchCall, err error) (*runs.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if err != nil {
		return nil, err
	}
	prev, _ := f.rs.Get(context.Background(), c.prevID) // the test put it
	// A resume reopens the run as its next turn; a fresh run is a new row.
	next := runs.Run{ID: c.prevID, Status: runs.StatusRunning, Reason: c.reason, TurnCount: 2}
	if c.path == "fresh" {
		next = runs.Run{ID: fmt.Sprintf("%s-next%d", c.prevID, len(f.calls)), Status: runs.StatusRunning, Reason: c.reason, PreviousRunID: c.prevID, TurnCount: 1}
	}
	if prev != nil {
		next.JobID, next.JobName, next.Workspace, next.Kind, next.SessionID, next.WorkingDir = prev.JobID, prev.JobName, prev.Workspace, prev.Kind, prev.SessionID, prev.WorkingDir
		if c.path != "fresh" {
			next.Reason, next.TurnCount = prev.Reason, prev.TurnCount+1
		}
	}
	f.rs.put(next)
	return &next, nil
}

func (f *fakeLauncher) Resume(_ context.Context, prevID string, o runs.ResumeOpts) (*runs.Run, error) {
	return f.record(launchCall{path: "resume", prevID: prevID, prompt: o.Prompt, reason: o.Reason}, f.resumeErr)
}

func (f *fakeLauncher) Fresh(_ context.Context, prevID string, o runs.ResumeOpts) (*runs.Run, error) {
	return f.record(launchCall{path: "fresh", prevID: prevID, prompt: o.Prompt, reason: o.Reason}, f.freshErr)
}

func (f *fakeLauncher) Calls() []launchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]launchCall(nil), f.calls...)
}

type fakeGrant struct{ ok atomic.Bool }

func (f *fakeGrant) Entitlement() entitle.State {
	if !f.ok.Load() {
		return entitle.State{Linked: true, Plan: "free"}
	}
	return entitle.State{Linked: true, Plan: "pro_cloud", Features: []entitle.Feature{entitle.Requests}}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	svc    *Service
	st     *store.Store
	rs     *fakeRuns
	launch *fakeLauncher
	grant  *fakeGrant
	clock  *clock
	dbPath string
	dir    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		rs:     &fakeRuns{runs: map[string]*runs.Run{}},
		grant:  &fakeGrant{},
		clock:  &clock{t: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)},
		dbPath: filepath.Join(dir, "trebi.db"),
		dir:    dir,
	}
	h.grant.ok.Store(true)
	h.launch = &fakeLauncher{rs: h.rs}
	h.open(t)
	return h
}

func (h *harness) open(t *testing.T) {
	t.Helper()
	st, err := store.OpenPath(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.st = st
	h.svc = NewService(st, h.rs, h.grant, NewReportStore(filepath.Join(h.dir, "requests")), Options{}).WithClock(h.clock.now)
	h.rs.gate, h.rs.parker = h.svc, runs.NewParker(h.rs, h.launch, h.svc)
}

// run adds a live run with a session and a working dir.
func (h *harness) run(t *testing.T, id string) tools.Actor {
	t.Helper()
	wd := filepath.Join(h.dir, "wd-"+id)
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	h.rs.put(runs.Run{
		ID: id, Workspace: "ws1", JobID: "mail", JobName: "mail", Kind: "job", Status: runs.StatusRunning,
		SessionID: "sess-" + id, Harness: "claude", Runtime: "cli", WorkingDir: wd,
		ConversationKey: "ck", ConversationID: "cid",
	})
	return tools.Actor{RunID: id, Kind: "job", JobID: "mail", Job: "mail", WorkspaceID: "ws1"}
}

func (h *harness) create(t *testing.T, actor tools.Actor, o CreateOpts) Request {
	t.Helper()
	if len(o.Forms) == 0 {
		o.Forms = []FormOpts{confirm("Delete the old branch")}
	}
	r, err := h.svc.Create(context.Background(), actor, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (h *harness) approve(t *testing.T, id string) Request {
	t.Helper()
	r, err := h.svc.Answer(context.Background(), id, approveAll(h.get(t, id)), 0)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (h *harness) get(t *testing.T, id string) Request {
	t.Helper()
	r, err := h.svc.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (h *harness) topics(t *testing.T, topic string) int {
	t.Helper()
	var n int
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic=?`, topic).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateNeedsRunActor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, a := range []tools.Actor{{}, {Kind: "assistant", Conversation: "c1"}} {
		if _, err := h.svc.Create(context.Background(), a, CreateOpts{Forms: []FormOpts{confirm("x")}}); !errors.Is(err, ErrNotRun) {
			t.Errorf("actor %+v: err %v", a, err)
		}
	}
}

func TestCreateNeedsPlan(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	actor := h.run(t, "RUN1")
	h.grant.ok.Store(false)
	if _, err := h.svc.Create(context.Background(), actor, CreateOpts{Forms: []FormOpts{confirm("x")}}); !errors.Is(err, ErrPlan) {
		t.Fatalf("err %v", err)
	}
	h.grant.ok.Store(true)
	r := h.create(t, actor, CreateOpts{})
	if h.topics(t, "request.created") != 1 || r.Status != StatusPending {
		t.Fatalf("created %+v", r)
	}
}

func TestCreateCopiesRunFields(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	actor := h.run(t, "RUN1")
	r := h.get(t, h.create(t, actor, CreateOpts{}).ID)
	run, _ := h.rs.Get(context.Background(), "RUN1")
	if r.SessionID != run.SessionID || r.WorkingDir != run.WorkingDir || r.Harness != "claude" || r.Runtime != "cli" ||
		r.ConversationKey != "ck" || r.ConversationID != "cid" || r.JobID != "mail" || r.WorkspaceID != "ws1" {
		t.Fatalf("row %+v", r)
	}
	if r.ExpiresAt == nil || !r.ExpiresAt.Equal(h.clock.now().Add(defaultExpireAfter)) {
		t.Errorf("expires %v", r.ExpiresAt)
	}
}

func TestCreateSnapshotsReport(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	actor := h.run(t, "RUN1")
	run, _ := h.rs.Get(context.Background(), "RUN1")
	path := filepath.Join(run.WorkingDir, "report.md")
	if err := os.WriteFile(path, []byte("# v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.create(t, actor, CreateOpts{Forms: []FormOpts{{Type: TypeConfirm, Title: "x", Report: "report.md"}}})
	if err := os.WriteFile(path, []byte("# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := h.svc.Reports().Open(r.ID, 0, r.Forms[0].ReportKind)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, _ := f.Read(buf)
	f.Close()
	if string(buf[:n]) != "# v1\n" || r.Forms[0].ReportKind != "markdown" || r.Forms[0].ReportSHA256 == "" {
		t.Fatalf("snapshot %q kind %s", buf[:n], r.Forms[0].ReportKind)
	}
	big := filepath.Join(run.WorkingDir, "big.html")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", 6<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../escape.md", "big.html", "report.txt", "missing.md"} {
		var fe *FieldError
		_, err := h.svc.Create(context.Background(), actor, CreateOpts{Forms: []FormOpts{{Type: TypeConfirm, Title: "x", Report: rel}}})
		if !errors.As(err, &fe) || fe.Field != "report" {
			t.Errorf("%s: err %v", rel, err)
		}
	}
}

func TestAnswerOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.create(t, h.run(t, "RUN1"), CreateOpts{})
	h.approve(t, r.ID)
	reject := AnswerIn{Forms: []FormAnswerIn{{FormAnswer: FormAnswer{Decision: DecisionReject}}}, By: ByUser, Via: ViaUI}
	got, err := h.svc.Answer(context.Background(), r.ID, reject, 0)
	if !errors.Is(err, ErrClosed) || got.Forms[0].Answer == nil || got.Forms[0].Answer.Decision != DecisionApprove {
		t.Fatalf("second answer: %v %+v", err, got.Forms[0].Answer)
	}
	if got.Answered == nil || got.Answered.By != ByUser || got.Answered.Via != ViaUI {
		t.Fatalf("answered %+v", got.Answered)
	}
	if _, err := h.svc.Dismiss(context.Background(), r.ID, ByUser); !errors.Is(err, ErrClosed) {
		t.Fatalf("dismiss closed: %v", err)
	}
}

func TestAnswerIfMatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.create(t, h.run(t, "RUN1"), CreateOpts{})
	if _, err := h.svc.Answer(context.Background(), r.ID, approveAll(r), r.Revision+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale: %v", err)
	}
	got, err := h.svc.Answer(context.Background(), r.ID, approveAll(r), r.Revision)
	if err != nil || got.Revision != r.Revision+1 {
		t.Fatalf("fresh: %v rev %d", err, got.Revision)
	}
}

func TestListFilterAndCursor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a, b := h.run(t, "RUN1"), h.run(t, "RUN2")
	for range 3 {
		h.approve(t, h.create(t, a, CreateOpts{}).ID)
	}
	h.create(t, b, CreateOpts{})
	ctx := context.Background()
	page, next, err := h.svc.List(ctx, ListOpts{RunID: "RUN1", Limit: 2})
	if err != nil || len(page) != 2 || next == "" || page[0].ID < page[1].ID {
		t.Fatalf("page 1: %v %d %q", err, len(page), next)
	}
	rest, next, err := h.svc.List(ctx, ListOpts{RunID: "RUN1", Limit: 2, After: next})
	if err != nil || len(rest) != 1 || next != "" {
		t.Fatalf("page 2: %v %d %q", err, len(rest), next)
	}
	if all, _, _ := h.svc.List(ctx, ListOpts{Status: StatusPending}); len(all) != 1 || len(all[0].Forms) != 1 {
		t.Fatalf("pending %+v", all)
	}
}

// TestCreateJoinsOpenRequest pins the batch rule: a create call adds a form
// to the open request of the run. A chain request is a separate source.
func TestCreateJoinsOpenRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	actor := h.run(t, "RUN1")
	first := h.create(t, actor, CreateOpts{Title: "Ship the release", Forms: []FormOpts{confirm("Tag it")}})
	second := h.create(t, actor, CreateOpts{Title: "ignored", Forms: []FormOpts{confirm("Mail the team"), confirm("Close the issue")}})
	if second.ID != first.ID || second.Title != "Ship the release" || second.Revision != first.Revision+1 {
		t.Fatalf("second %+v", second)
	}
	got := h.get(t, first.ID)
	if len(got.Forms) != 3 || got.Forms[2].Idx != 2 || got.Forms[2].Title != "Close the issue" {
		t.Fatalf("forms %+v", got.Forms)
	}
	if h.topics(t, "request.created") != 1 || h.topics(t, "request.updated") != 1 {
		t.Fatal("want one created and one updated event")
	}
	chain, err := h.svc.CreateSystem(context.Background(), "RUN1", CreateOpts{Source: SourceChain, Forms: []FormOpts{confirm("Raise the limit")}})
	if err != nil || chain.ID == first.ID {
		t.Fatalf("chain request %v %+v", err, chain)
	}
	h.approve(t, first.ID)
	third := h.create(t, actor, CreateOpts{})
	if third.ID == first.ID || len(third.Forms) != 1 {
		t.Fatalf("create after the answer joined a closed request: %+v", third)
	}
}

// TestCreateFormLimit refuses the form that passes the limit.
func TestCreateFormLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	actor := h.run(t, "RUN1")
	forms := make([]FormOpts, maxForms)
	for i := range forms {
		forms[i] = confirm(fmt.Sprintf("Step %d", i))
	}
	r := h.create(t, actor, CreateOpts{Forms: forms})
	var fe *FieldError
	if _, err := h.svc.Create(context.Background(), actor, CreateOpts{Forms: []FormOpts{confirm("One more")}}); !errors.As(err, &fe) || fe.Field != "forms" {
		t.Fatalf("err %v", err)
	}
	if got := h.get(t, r.ID); len(got.Forms) != maxForms {
		t.Fatalf("forms %d", len(got.Forms))
	}
}

// TestAnswerAfterFormAdded refuses an answer built on an older revision.
func TestAnswerAfterFormAdded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	actor := h.run(t, "RUN1")
	r := h.create(t, actor, CreateOpts{})
	h.create(t, actor, CreateOpts{Forms: []FormOpts{confirm("Second")}})
	if _, err := h.svc.Answer(context.Background(), r.ID, approveAll(r), r.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale: %v", err)
	}
	var fe *FieldError
	if _, err := h.svc.Answer(context.Background(), r.ID, approveAll(r), 0); !errors.As(err, &fe) || !strings.Contains(err.Error(), "form 1 has no answer") {
		t.Fatalf("partial: %v", err)
	}
}

// TestPromptHintFollowsPlan pins the launch text: it names the create command
// with a grant and is empty without one.
func TestPromptHintFollowsPlan(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if hint := h.svc.PromptHint(runs.LaunchOpts{}); !strings.Contains(hint, "trebi request create") || !strings.Contains(hint, "--forms-file") {
		t.Fatalf("hint = %q", hint)
	}
	h.grant.ok.Store(false)
	if hint := h.svc.PromptHint(runs.LaunchOpts{}); hint != "" {
		t.Fatalf("hint without a grant = %q", hint)
	}
}

// confirm is one confirm form.
func confirm(title string) FormOpts { return FormOpts{Type: TypeConfirm, Title: title} }

// approveAll answers each form of r with approve.
func approveAll(r Request) AnswerIn {
	in := AnswerIn{By: ByUser, Via: ViaUI}
	for _, f := range r.Forms {
		in.Forms = append(in.Forms, FormAnswerIn{Idx: f.Idx, FormAnswer: FormAnswer{Decision: DecisionApprove}})
	}
	return in
}

// TestRequestRunEvents pins the session feed: create, a joined form, and the
// answer each record the request state on the run.
func TestRequestRunEvents(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	actor := h.run(t, "RUN1")
	h.create(t, actor, CreateOpts{})
	r := h.create(t, actor, CreateOpts{Forms: []FormOpts{confirm("Second")}})
	if _, err := h.svc.Answer(context.Background(), r.ID, approveAll(r), 0); err != nil {
		t.Fatal(err)
	}
	h.rs.mu.Lock()
	defer h.rs.mu.Unlock()
	var got []string
	for _, ev := range h.rs.events {
		tr, _ := ev.detail.(agentcli.TurnRequest)
		if ev.runID != "RUN1" || ev.kind != runs.EventRequest || tr.ID != r.ID {
			t.Fatalf("event %+v", ev)
		}
		got = append(got, fmt.Sprintf("%s/%d", tr.Status, tr.Forms))
	}
	if want := []string{"pending/1", "pending/2", "answered/2"}; !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}
