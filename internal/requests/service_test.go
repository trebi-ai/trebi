package requests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/entitle"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
)

type fakeRuns struct {
	mu     sync.Mutex
	runs   map[string]*runs.Run
	turn   int
	gate   runs.ParkGate
	parker *runs.Parker
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

func (f *fakeRuns) WakeParked(ctx context.Context, id string) error {
	return f.parker.Wake(ctx, id)
}

func (f *fakeRuns) TurnAfter(context.Context, string, runs.RunReason, time.Time) (int, error) {
	return f.turn, nil
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
	if o.Type == "" {
		o.Type, o.Title = TypeConfirm, "Delete the old branch"
	}
	r, err := h.svc.Create(context.Background(), actor, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (h *harness) approve(t *testing.T, id string) Request {
	t.Helper()
	r, err := h.svc.Answer(context.Background(), id, Answer{Decision: DecisionApprove, By: ByUser, Via: ViaUI}, 0)
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
		if _, err := h.svc.Create(context.Background(), a, CreateOpts{Type: TypeConfirm, Title: "x"}); !errors.Is(err, ErrNotRun) {
			t.Errorf("actor %+v: err %v", a, err)
		}
	}
}

func TestCreateNeedsPlan(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	actor := h.run(t, "RUN1")
	h.grant.ok.Store(false)
	if _, err := h.svc.Create(context.Background(), actor, CreateOpts{Type: TypeConfirm, Title: "x"}); !errors.Is(err, ErrPlan) {
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
	r := h.create(t, actor, CreateOpts{Type: TypeConfirm, Title: "x", Report: "report.md"})
	if err := os.WriteFile(path, []byte("# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := h.svc.Reports().Open(r.ID, r.ReportKind)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, _ := f.Read(buf)
	f.Close()
	if string(buf[:n]) != "# v1\n" || r.ReportKind != "markdown" || r.ReportSHA256 == "" {
		t.Fatalf("snapshot %q kind %s", buf[:n], r.ReportKind)
	}
	big := filepath.Join(run.WorkingDir, "big.html")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", 6<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../escape.md", "big.html", "report.txt", "missing.md"} {
		var fe *FieldError
		_, err := h.svc.Create(context.Background(), actor, CreateOpts{Type: TypeConfirm, Title: "x", Report: rel})
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
	got, err := h.svc.Answer(context.Background(), r.ID, Answer{Decision: DecisionReject, By: ByUser, Via: ViaUI}, 0)
	if !errors.Is(err, ErrClosed) || got.Answer == nil || got.Answer.Decision != DecisionApprove {
		t.Fatalf("second answer: %v %+v", err, got.Answer)
	}
	if _, err := h.svc.Dismiss(context.Background(), r.ID, ByUser); !errors.Is(err, ErrClosed) {
		t.Fatalf("dismiss closed: %v", err)
	}
}

func TestAnswerIfMatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.create(t, h.run(t, "RUN1"), CreateOpts{})
	if _, err := h.svc.Answer(context.Background(), r.ID, Answer{Decision: DecisionApprove}, r.Revision+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale: %v", err)
	}
	got, err := h.svc.Answer(context.Background(), r.ID, Answer{Decision: DecisionApprove}, r.Revision)
	if err != nil || got.Revision != r.Revision+1 {
		t.Fatalf("fresh: %v rev %d", err, got.Revision)
	}
}

func TestListFilterAndCursor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a, b := h.run(t, "RUN1"), h.run(t, "RUN2")
	for range 3 {
		h.create(t, a, CreateOpts{})
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
	if all, _, _ := h.svc.List(ctx, ListOpts{Status: StatusPending}); len(all) != 4 {
		t.Fatalf("pending %d", len(all))
	}
}

func TestPollCancelledOnRunEnd(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.create(t, h.run(t, "RUN1"), CreateOpts{Type: TypeConfirm, Title: "x", Delivery: DeliveryPoll})
	if h.pending(t, "RUN1") {
		t.Fatal("a poll request parks the run")
	}
	h.rs.setStatus("RUN1", runs.StatusComplete)
	h.svc.RunEnded(context.Background(), "RUN1", runs.StatusComplete)
	got, err := h.svc.Wait(context.Background(), r.ID, time.Second)
	if err != nil || got.Status != StatusCancelled || len(h.launch.Calls()) != 0 {
		t.Fatalf("wait: %v %s calls %d", err, got.Status, len(h.launch.Calls()))
	}
}

func TestWaitReturnsOnAnswer(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.create(t, h.run(t, "RUN1"), CreateOpts{Type: TypeConfirm, Title: "x", Delivery: DeliveryPoll})
	if got, _ := h.svc.Wait(context.Background(), r.ID, 300*time.Millisecond); got.Status != StatusPending {
		t.Fatalf("timeout status %s", got.Status)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = h.svc.Answer(context.Background(), r.ID, Answer{Decision: DecisionApprove}, 0) // Wait checks the result
	}()
	got, err := h.svc.Wait(context.Background(), r.ID, 5*time.Second)
	if err != nil || got.Status != StatusAnswered {
		t.Fatalf("wait: %v %s", err, got.Status)
	}
}

// TestPromptHintFollowsPlan pins the launch text: it names the create command
// with a grant and is empty without one.
func TestPromptHintFollowsPlan(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if hint := h.svc.PromptHint(runs.LaunchOpts{}); !strings.Contains(hint, "trebi request create") || !strings.Contains(hint, "[trebi] The person answered") {
		t.Fatalf("hint = %q", hint)
	}
	h.grant.ok.Store(false)
	if hint := h.svc.PromptHint(runs.LaunchOpts{}); hint != "" {
		t.Fatalf("hint without a grant = %q", hint)
	}
}
