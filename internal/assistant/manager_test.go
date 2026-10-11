package assistant_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// fakeMemory satisfies assistant.memoryReader.
type fakeMemory struct{ text string }

func (f fakeMemory) Render(context.Context, string) (string, error) { return f.text, nil }

// fakeInbox records headless permission timeouts.
type fakeInbox struct {
	mu   sync.Mutex
	open []string
}

func (f *fakeInbox) OpenPermissionTimeout(_ context.Context, _, convID, tool string) error {
	f.mu.Lock()
	f.open = append(f.open, convID+"|"+tool)
	f.mu.Unlock()
	return nil
}

func (f *fakeInbox) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.open...)
}

// countingRT counts Start and Resume calls over the real runtime.
type countingRT struct {
	inner  agentRuntime
	starts int
	resume int
	mu     sync.Mutex
}

// agentRuntime mirrors the manager's consumer interface for tests.
type agentRuntime interface {
	Detect(ctx context.Context, h agentwire.Harness) (agentwire.Detection, error)
	Start(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error)
	Resume(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error)
}

func (c *countingRT) Detect(ctx context.Context, h agentwire.Harness) (agentwire.Detection, error) {
	return c.inner.Detect(ctx, h)
}

func (c *countingRT) Start(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	c.mu.Lock()
	c.starts++
	c.mu.Unlock()
	return c.inner.Start(ctx, req)
}

func (c *countingRT) Resume(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	c.mu.Lock()
	c.resume++
	c.mu.Unlock()
	return c.inner.Resume(ctx, req)
}

type testEnv struct {
	mgr      *assistant.Manager
	st       *store.Store
	conv     *conversations.Service
	ws       *workspaces.Service
	rt       *countingRT
	inbox    *fakeInbox
	wsDir    string
	runtime  *agentwire.Runtime
	resolver *assistant.Resolver
}

// newEnv builds a manager over the fake harness with one workspace whose
// trebi.yaml pins harness: fake.
func newEnv(t *testing.T, timing assistant.ResolverTiming) *testEnv {
	t.Helper()
	base := t.TempDir()
	home.SetBase(base)
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(base, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	root := filepath.Join(base, "workspaces")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	wsSvc := workspaces.New(st).WithRoot(root)
	wsDir := filepath.Join(root, "sling")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := wsSvc.Add(context.Background(), wsDir, "sling"); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Assistant.Native.Model = ""
	rt := agentwire.New(agentwire.Options{ClientName: "trebi-test"})
	// The stock fake script answers one turn and exits. The daemon expects a
	// process that stays live across turns, so the fixture loops per line.
	rt.SetDriver(agentwire.Fake, agentwire.FakeDriver{Script: fakeLoopScript})
	resolver := assistant.NewResolver(
		assistant.DefaultsFromConfig(cfg), wsSvc, rt,
		func() config.NativeConfig { return cfg.Assistant.Native }, timing)

	assistant.RegisterNativeDriver(rt, func() config.NativeConfig { return cfg.Assistant.Native }, nil, assistant.NewStore(st), 0, nil)
	crt := &countingRT{inner: rt}

	convSvc := conversations.New(st)
	inbox := &fakeInbox{}
	mgr := assistant.NewManager(crt, assistant.NewStore(st), convSvc, resolver, wsSvc,
		fakeMemory{text: "remember the release cadence"}, inbox, assistant.Options{
			MCPURL:    "http://127.0.0.1:1/mcp",
			TrebiExe:  "/bin/echo",
			SkillsDir: home.SkillsDir(),
			Log:       nil,
		})
	// Stop live sessions before the store closes and TempDir is removed.
	t.Cleanup(func() { _ = mgr.Shutdown(context.Background()) })
	convSvc.WithAssistant(mgr)

	env := &testEnv{
		mgr: mgr, st: st, conv: convSvc, ws: wsSvc, rt: crt, inbox: inbox,
		wsDir: wsDir, runtime: rt, resolver: resolver,
	}

	// Pin the fake harness in trebi.yaml so resolve skips machine probes.
	if _, err := wsSvc.SetAssistantOverride(context.Background(), env.workspaceID(t),
		wireOverride("fake"), ""); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *testEnv) workspaceID(t *testing.T) string {
	t.Helper()
	list, err := e.ws.List(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("workspaces: %v %v", list, err)
	}
	return list[0].ID
}

func (e *testEnv) conversation(t *testing.T) string {
	t.Helper()
	c, err := e.conv.Create(context.Background(), conversations.CreateOpts{
		Workspace: e.workspaceID(t), OriginChannel: conversations.ChannelUI,
		Activate: &conversations.Address{Connector: conversations.ChannelUI, Room: "ui-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestFirstPromptStartsAndSecondReuses(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{})
	ctx := context.Background()
	convID := env.conversation(t)

	res, err := env.mgr.Prompt(ctx, convID, assistant.PromptInput{
		Text: "hello", Connector: conversations.ChannelUI, Room: "ui-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The fake turn can end before Prompt reads the status back.
	if res.Duplicate || (res.Status != assistant.StatusRunning && res.Status != assistant.StatusIdle) {
		t.Fatalf("res %+v", res)
	}
	waitFor(t, 2*time.Second, "first turn end", func() bool {
		rec, err := env.mgr.Session(ctx, convID)
		return err == nil && rec.Status == assistant.StatusIdle && rec.Turn >= 1
	})
	if !env.mgr.Live(convID) {
		t.Fatal("session should still be live after one turn")
	}
	if _, err := env.mgr.Prompt(ctx, convID, assistant.PromptInput{Text: "again"}); err != nil {
		t.Fatal(err)
	}
	env.rt.mu.Lock()
	starts := env.rt.starts
	env.rt.mu.Unlock()
	if starts != 1 {
		t.Fatalf("second prompt must reuse the process, starts=%d", starts)
	}
	rec, _ := env.mgr.Session(ctx, convID)
	if rec.Prompts != 2 {
		t.Fatalf("prompts=%d", rec.Prompts)
	}
}

func TestIdleCloseThenResumeByVendorID(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{IdleClose: 80 * time.Millisecond})
	ctx := context.Background()
	convID := env.conversation(t)

	if _, err := env.mgr.Prompt(ctx, convID, assistant.PromptInput{Text: "one"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "idle close", func() bool {
		rec, err := env.mgr.Session(ctx, convID)
		return err == nil && rec.Status == assistant.StatusClosed
	})
	if env.mgr.Live(convID) {
		t.Fatal("idle session must be closed")
	}
	if _, err := env.mgr.Prompt(ctx, convID, assistant.PromptInput{Text: "two"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "resume", func() bool {
		return env.rt.resumeCount() >= 1
	})
	rec, _ := env.mgr.Session(ctx, convID)
	if rec.Status != assistant.StatusIdle && rec.Status != assistant.StatusRunning {
		t.Fatalf("resumed status %s", rec.Status)
	}
}

func (c *countingRT) resumeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resume
}

func TestMaxLiveEvictsIdleSession(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{MaxLive: 1})
	ctx := context.Background()
	conv1 := env.conversation(t)
	if _, err := env.mgr.Prompt(ctx, conv1, assistant.PromptInput{Text: "one"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "first idle", func() bool {
		rec, err := env.mgr.Session(ctx, conv1)
		return err == nil && rec.Status == assistant.StatusIdle
	})
	conv2 := env.conversation(t)
	if _, err := env.mgr.Prompt(ctx, conv2, assistant.PromptInput{Text: "two"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "eviction of the first", func() bool {
		rec, err := env.mgr.Session(ctx, conv1)
		return err == nil && rec.Status == assistant.StatusClosed && !env.mgr.Live(conv1)
	})
}

func TestDuplicateIdempotencyKeySendsNothing(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{})
	ctx := context.Background()
	convID := env.conversation(t)

	first, err := env.mgr.Prompt(ctx, convID, assistant.PromptInput{Text: "hello", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "turn end", func() bool {
		rec, err := env.mgr.Session(ctx, convID)
		return err == nil && rec.Status == assistant.StatusIdle
	})
	second, err := env.mgr.Prompt(ctx, convID, assistant.PromptInput{Text: "hello", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate || second.Message.ID != first.Message.ID {
		t.Fatalf("second %+v", second)
	}
	rec, _ := env.mgr.Session(ctx, convID)
	if rec.Prompts != 1 {
		t.Fatalf("prompts=%d, want 1", rec.Prompts)
	}
}

func TestDisabledAssistantRefused(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{})
	ctx := context.Background()
	wsID := env.workspaceID(t)
	disabled := true
	ov := wireOverride("fake")
	ov.Disabled = &disabled
	if _, err := env.ws.SetAssistantOverride(ctx, wsID, ov, ""); err != nil {
		t.Fatal(err)
	}
	convID := env.conversation(t)
	_, err := env.mgr.Prompt(ctx, convID, assistant.PromptInput{Text: "hello"})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("want disabled error, got %v", err)
	}
}

func TestAgentConversationRefused(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{})
	ctx := context.Background()
	c, err := env.conv.Create(ctx, conversations.CreateOpts{
		Workspace: env.workspaceID(t), OperatorID: "agt_0000000003",
		OriginChannel: conversations.ChannelAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.mgr.Prompt(ctx, c.ID, assistant.PromptInput{Text: "hello"})
	if err == nil || !strings.Contains(err.Error(), "agent conversations") {
		t.Fatalf("want agent refusal, got %v", err)
	}
}

func TestDeleteRemovesConversationAndSession(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{})
	ctx := context.Background()
	convID := env.conversation(t)
	if _, err := env.mgr.Prompt(ctx, convID, assistant.PromptInput{Text: "one"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "turn end", func() bool {
		rec, err := env.mgr.Session(ctx, convID)
		return err == nil && rec.Status == assistant.StatusIdle
	})
	if err := env.mgr.Delete(ctx, convID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.mgr.Session(ctx, convID); !errors.Is(err, assistant.ErrNoSession) {
		t.Fatalf("session row should be gone: %v", err)
	}
	if _, err := env.conv.Get(ctx, convID); !errors.Is(err, conversations.ErrNotFound) {
		t.Fatalf("conversation should be gone: %v", err)
	}
	events, err := env.mgr.Events(ctx, convID, 0, 100)
	if err != nil || len(events) != 0 {
		t.Fatalf("events should be gone: %v %v", events, err)
	}
}

// wireOverride builds a trebi.yaml assistant override pinning one harness.
func wireOverride(harness string) assistant.Override {
	h := harness
	return assistant.Override{Harness: &h}
}

func promptInput(text string) assistant.PromptInput {
	return assistant.PromptInput{Text: text, Connector: conversations.ChannelUI, Room: "ui-1"}
}

func listOpts50() conversations.MessageListOpts { return conversations.MessageListOpts{Limit: 50} }

// fakeLoopScript answers every prompt line with one assistant text and one
// result, and stays alive for the next turn (the stock fake exits after one
// turn, which would close the session after the first prompt).
const fakeLoopScript = `first=1
while IFS= read -r line; do
if [ "$first" = 1 ]; then first=0; echo '{"type":"init","session_id":"fake-vendor-1"}'; fi
echo '{"type":"assistant","text":"Working"}'
echo '{"type":"result","subtype":"success"}'
done`

// --- plan 2026-09-26 A: the prompt becomes a `user` event ---

// countUserEvents returns the stored `user` events of one conversation.
func countUserEvents(t *testing.T, env *testEnv, convID string) []assistant.Event {
	t.Helper()
	events, err := env.mgr.Events(context.Background(), convID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	var out []assistant.Event
	for _, e := range events {
		if e.Type == "user" {
			out = append(out, e)
		}
	}
	return out
}

func TestPromptWritesUserEvent(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{})
	ctx := context.Background()
	convID := env.conversation(t)

	if _, err := env.mgr.Prompt(ctx, convID, assistant.PromptInput{
		Text: "how many jobs active?", Connector: conversations.ChannelUI,
		Room: "ui-1", Context: []string{"jobs:active"},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "first turn end", func() bool {
		rec, err := env.mgr.Session(ctx, convID)
		return err == nil && rec.Status == assistant.StatusIdle && rec.Turn >= 1
	})

	events, err := env.mgr.Events(ctx, convID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	user := -1
	for i, e := range events {
		if e.Type == "user" {
			user = i
			break
		}
	}
	if user < 0 {
		t.Fatalf("no user event among %d events", len(events))
	}
	e := events[user]
	if e.Text != "how many jobs active?" || e.Prompt != "how many jobs active?" {
		t.Fatalf("user event text=%q prompt=%q", e.Text, e.Prompt)
	}
	if len(e.Context) != 1 || e.Context[0] != "jobs:active" {
		t.Fatalf("user event context=%v", e.Context)
	}
	if e.Channel != conversations.ChannelUI {
		t.Fatalf("user event channel=%q", e.Channel)
	}
	if e.Turn != 1 {
		t.Fatalf("user event turn=%d, want 1", e.Turn)
	}
	for _, h := range events[user+1:] {
		if h.Turn == 1 && h.Seq <= e.Seq {
			t.Fatalf("harness event of turn 1 has seq %d before the user event seq %d", h.Seq, e.Seq)
		}
	}
}

func TestDuplicatePromptWritesOneUserEvent(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{})
	ctx := context.Background()
	convID := env.conversation(t)

	in := assistant.PromptInput{Text: "once", Connector: conversations.ChannelUI,
		Room: "ui-1", IdempotencyKey: "k1"}
	if _, err := env.mgr.Prompt(ctx, convID, in); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "first turn end", func() bool {
		rec, err := env.mgr.Session(ctx, convID)
		return err == nil && rec.Status == assistant.StatusIdle
	})
	res, err := env.mgr.Prompt(ctx, convID, in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Duplicate {
		t.Fatalf("resend must be a duplicate, got %+v", res)
	}
	if users := countUserEvents(t, env, convID); len(users) != 1 {
		t.Fatalf("user events=%d, want 1", len(users))
	}
}

// failDriver starts sessions whose Prompt always fails (plan 2026-09-26 A).
type failDriver struct{}

func (failDriver) Name() string { return string(agentwire.Fake) }

func (failDriver) Start(context.Context, agentwire.StartRequest) (agentwire.Session, error) {
	return &failSession{events: make(chan agentwire.Event)}, nil
}

func (failDriver) Resume(context.Context, agentwire.StartRequest) (agentwire.Session, error) {
	return &failSession{events: make(chan agentwire.Event)}, nil
}

// failSession closes its event stream on Close, so the pump ends.
type failSession struct {
	events chan agentwire.Event
	once   sync.Once
}

func (*failSession) Provider() string { return string(agentwire.Fake) }
func (*failSession) ID() string       { return "fail-1" }
func (*failSession) PID() int         { return 0 }
func (*failSession) Prompt(context.Context, agentwire.Prompt) error {
	return errors.New("pipe broken")
}
func (*failSession) Interrupt(context.Context) error                                    { return nil }
func (*failSession) AnswerPermission(context.Context, string, agentwire.Decision) error { return nil }
func (*failSession) InterruptTurn(context.Context) error                                { return nil }
func (*failSession) Done() <-chan struct{}                                              { return make(chan struct{}) }
func (*failSession) Err() error                                                         { return nil }
func (f *failSession) Close(context.Context) error {
	f.once.Do(func() { close(f.events) })
	return nil
}
func (f *failSession) Events() <-chan agentwire.Event { return f.events }

func TestSendFailureKeepsUserEventAndAddsError(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{})
	env.runtime.SetDriver(agentwire.Fake, failDriver{})
	ctx := context.Background()
	convID := env.conversation(t)

	if _, err := env.mgr.Prompt(ctx, convID, promptInput("hi")); err == nil {
		t.Fatal("expected the send to fail")
	}
	events, err := env.mgr.Events(ctx, convID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	var users, errs []assistant.Event
	for _, e := range events {
		switch e.Type {
		case "user":
			users = append(users, e)
		case "error":
			errs = append(errs, e)
		}
	}
	if len(users) != 1 {
		t.Fatalf("user events=%d, want 1", len(users))
	}
	if len(errs) != 1 {
		t.Fatalf("error events=%d, want 1", len(errs))
	}
	if errs[0].Error == nil || errs[0].Error.Class != "send" ||
		!strings.Contains(errs[0].Error.Message, "could not send the prompt") {
		t.Fatalf("error event=%+v", errs[0])
	}
}

// The API create writes a mode-only pin row with an empty harness. The first
// prompt must inherit the resolved harness and write it back, so the session
// row and the drawer picker show what actually runs (plan 2026-09-26 F.2).
func TestEmptyHarnessPinInheritsResolved(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{})
	ctx := context.Background()
	convID := env.conversation(t)
	asstStore := assistant.NewStore(env.st)
	if err := asstStore.Create(ctx, assistant.Record{
		ConversationID: convID, WorkspaceID: env.workspaceID(t),
		Mode:   assistant.ModeAutoEdit,
		Status: assistant.StatusClosed,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.mgr.Prompt(ctx, convID, promptInput("hello")); err != nil {
		t.Fatal(err)
	}
	rec, err := asstStore.Get(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Harness != "fake" {
		t.Fatalf("session harness = %q, want the resolved fake", rec.Harness)
	}
}

// scriptDriver hands out one scriptSession that the test drives: the test
// emits the events, and Send reports a delivery like agent-wire does.
type scriptDriver struct{ sess *scriptSession }

func (scriptDriver) Name() string { return string(agentwire.Fake) }

func (d scriptDriver) Start(context.Context, agentwire.StartRequest) (agentwire.Session, error) {
	return d.sess, nil
}

func (d scriptDriver) Resume(context.Context, agentwire.StartRequest) (agentwire.Session, error) {
	return d.sess, nil
}

type scriptSession struct {
	events chan agentwire.Event
	once   sync.Once
	steer  bool // false: a prompt during a turn is held

	mu      sync.Mutex
	turn    int
	active  bool
	prompts []agentwire.Prompt
	steered []agentwire.Prompt
	held    []agentwire.Prompt
}

func (*scriptSession) Provider() string { return string(agentwire.Fake) }
func (*scriptSession) ID() string       { return "script-1" }
func (*scriptSession) PID() int         { return 0 }

func (s *scriptSession) Prompt(_ context.Context, p agentwire.Prompt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn++
	s.active = true
	s.prompts = append(s.prompts, p)
	return nil
}

func (s *scriptSession) Send(ctx context.Context, p agentwire.Prompt) (agentwire.Delivery, error) {
	s.mu.Lock()
	switch {
	case s.active && s.steer:
		s.steered = append(s.steered, p)
		s.mu.Unlock()
		return agentwire.DeliverySteered, nil
	case s.active:
		s.held = append(s.held, p)
		s.mu.Unlock()
		return agentwire.DeliveryHeld, nil
	}
	s.mu.Unlock()
	return agentwire.DeliveryPrompted, s.Prompt(ctx, p)
}

func (*scriptSession) Interrupt(context.Context) error                                    { return nil }
func (*scriptSession) AnswerPermission(context.Context, string, agentwire.Decision) error { return nil }
func (*scriptSession) InterruptTurn(context.Context) error                                { return nil }
func (*scriptSession) Done() <-chan struct{}                                              { return make(chan struct{}) }
func (*scriptSession) Err() error                                                         { return nil }
func (s *scriptSession) Close(context.Context) error {
	s.once.Do(func() { close(s.events) })
	return nil
}
func (s *scriptSession) Events() <-chan agentwire.Event { return s.events }

func TestSendDuringTurn(t *testing.T) {
	cases := []struct {
		name      string
		steer     bool
		want      agentwire.Delivery
		wantTurn  int // turn of the second user event
		prompts   int // records after the second send
		sentTurns int // Prompt calls on the harness after the turn ends
	}{
		{name: "steered", steer: true, want: agentwire.DeliverySteered, wantTurn: 1, prompts: 1, sentTurns: 1},
		{name: "held", steer: false, want: agentwire.DeliveryHeld, wantTurn: 2, prompts: 2, sentTurns: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t, assistant.ResolverTiming{})
			sess := newScriptSession(tc.steer)
			env.runtime.SetDriver(agentwire.Fake, scriptDriver{sess: sess})
			ctx := context.Background()
			convID := env.conversation(t)

			first, err := env.mgr.Prompt(ctx, convID, promptInput("start a long task"))
			if err != nil {
				t.Fatal(err)
			}
			if first.Delivery != string(agentwire.DeliveryPrompted) {
				t.Fatalf("first delivery=%q", first.Delivery)
			}
			sess.emit(agentwire.Event{Type: agentwire.EventInit, SessionID: "script-1"})
			waitFor(t, 2*time.Second, "turn 1 recorded", func() bool {
				rec, err := env.mgr.Session(ctx, convID)
				return err == nil && rec.Turn == 1
			})

			second, err := env.mgr.Prompt(ctx, convID, promptInput("also check the logs"))
			if err != nil {
				t.Fatal(err)
			}
			if second.Delivery != string(tc.want) || second.Status != assistant.StatusRunning {
				t.Fatalf("second=%+v", second)
			}
			rec, _ := env.mgr.Session(ctx, convID)
			if rec.Prompts != tc.prompts || rec.Turn != 1 {
				t.Fatalf("prompts=%d turn=%d", rec.Prompts, rec.Turn)
			}
			users := countUserEvents(t, env, convID)
			if len(users) != 2 {
				t.Fatalf("user events=%d", len(users))
			}
			if users[0].Delivery != string(agentwire.DeliveryPrompted) || users[0].Turn != 1 {
				t.Fatalf("first user event=%+v", users[0])
			}
			if users[1].Delivery != string(tc.want) || users[1].Turn != tc.wantTurn {
				t.Fatalf("second user event=%+v", users[1])
			}

			sess.endTurn()
			if tc.want == agentwire.DeliveryHeld {
				sess.emit(agentwire.Event{Type: agentwire.EventAssistant, Text: "Checked the logs"})
				waitFor(t, 2*time.Second, "held turn running", func() bool {
					rec, err := env.mgr.Session(ctx, convID)
					return err == nil && rec.Turn == 2 && rec.Status == assistant.StatusRunning
				})
				sess.endTurn()
			}
			waitFor(t, 2*time.Second, "idle", func() bool {
				rec, err := env.mgr.Session(ctx, convID)
				return err == nil && rec.Status == assistant.StatusIdle
			})
			if n, _ := sess.counts(); n != tc.sentTurns {
				t.Fatalf("harness turns=%d, want %d", n, tc.sentTurns)
			}
		})
	}
}

func TestHeldPromptDroppedIsVisible(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{})
	sess := newScriptSession(false)
	env.runtime.SetDriver(agentwire.Fake, scriptDriver{sess: sess})
	ctx := context.Background()
	convID := env.conversation(t)

	if _, err := env.mgr.Prompt(ctx, convID, promptInput("start")); err != nil {
		t.Fatal(err)
	}
	sess.emit(agentwire.Event{Type: agentwire.EventError, Code: agentwire.CodeHeldPromptDropped,
		Error: "agentwire: the session ended before a held prompt was sent", Text: "later"})
	waitFor(t, 2*time.Second, "error event", func() bool {
		events, _ := env.mgr.Events(ctx, convID, 0, 200)
		for _, e := range events {
			if e.Type == "error" && e.Error != nil && e.Error.Code == agentwire.CodeHeldPromptDropped {
				return strings.Contains(e.Error.Message, "queued message")
			}
		}
		return false
	})
}

func TestStartTimeoutFailsSilentTurn(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{StartTimeout: 100 * time.Millisecond})
	sess := newScriptSession(true)
	env.runtime.SetDriver(agentwire.Fake, scriptDriver{sess: sess})
	ctx := context.Background()
	convID := env.conversation(t)

	if _, err := env.mgr.Prompt(ctx, convID, promptInput("hello")); err != nil {
		t.Fatal(err)
	}
	// Limits can arrive while the prompt is unread; they do not count.
	sess.emit(agentwire.Event{Type: agentwire.EventLimits})
	waitFor(t, 2*time.Second, "failed start", func() bool {
		rec, err := env.mgr.Session(ctx, convID)
		return err == nil && rec.Status == assistant.StatusError && !env.mgr.Live(convID)
	})
	rec, _ := env.mgr.Session(ctx, convID)
	if rec.Error != "The assistant did not start. Try again." {
		t.Fatalf("error=%q", rec.Error)
	}
	events, _ := env.mgr.Events(ctx, convID, 0, 200)
	var found bool
	for _, e := range events {
		if e.Type == "error" && e.Error != nil && e.Error.Code == "start_timeout" {
			found = true
		}
	}
	if !found {
		t.Fatal("no start_timeout error event")
	}
}

func TestStartTimeoutDisarmsOnFirstEvent(t *testing.T) {
	env := newEnv(t, assistant.ResolverTiming{StartTimeout: 100 * time.Millisecond})
	sess := newScriptSession(true)
	env.runtime.SetDriver(agentwire.Fake, scriptDriver{sess: sess})
	ctx := context.Background()
	convID := env.conversation(t)

	if _, err := env.mgr.Prompt(ctx, convID, promptInput("hello")); err != nil {
		t.Fatal(err)
	}
	sess.emit(agentwire.Event{Type: agentwire.EventInit, SessionID: "script-1"})
	time.Sleep(300 * time.Millisecond)
	rec, _ := env.mgr.Session(ctx, convID)
	if !env.mgr.Live(convID) || rec.Status != assistant.StatusRunning {
		t.Fatalf("live=%v status=%s", env.mgr.Live(convID), rec.Status)
	}
}

func newScriptSession(steer bool) *scriptSession {
	return &scriptSession{events: make(chan agentwire.Event, 64), steer: steer}
}

// emit sends one event in the current turn.
func (s *scriptSession) emit(e agentwire.Event) {
	s.mu.Lock()
	if e.Turn == 0 {
		e.Turn = s.turn
	}
	s.mu.Unlock()
	s.events <- e
}

// endTurn emits the result of the current turn. Held prompts then start the
// next turn, as agent-wire does.
func (s *scriptSession) endTurn() {
	s.mu.Lock()
	turn := s.turn
	s.active = false
	held := s.held
	s.held = nil
	if len(held) > 0 {
		s.turn++
		s.active = true
		s.prompts = append(s.prompts, held...)
	}
	s.mu.Unlock()
	s.events <- agentwire.Event{Type: agentwire.EventResult, Turn: turn}
}

func (s *scriptSession) counts() (prompts, steered int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prompts), len(s.steered)
}
