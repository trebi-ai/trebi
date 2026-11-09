package assistant_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/store"
)

// scriptedSession is a hand-rolled agentwire.Session that emits a queued
// script, so the pump tests control every event.
type scriptedSession struct {
	mu      sync.Mutex
	events  chan agentwire.Event
	script  []agentwire.Event
	prompts []string
	answers []scriptedAnswer
	vendor  string
	err     error
	closed  bool
	ints    int
}

type scriptedAnswer struct {
	permissionID string
	allow        bool
	by           string // "user" or "allow_list"; blank for the raw session view
}

func newScriptedSession(vendor string) *scriptedSession {
	return &scriptedSession{events: make(chan agentwire.Event, 256), vendor: vendor}
}

func (s *scriptedSession) setScript(evts ...agentwire.Event) {
	s.mu.Lock()
	s.script = evts
	s.mu.Unlock()
}

func (s *scriptedSession) Provider() string { return "scripted" }
func (s *scriptedSession) ID() string       { return s.vendor }
func (s *scriptedSession) PID() int         { return 0 }
func (s *scriptedSession) Done() <-chan struct{} {
	d := make(chan struct{})
	return d
}
func (s *scriptedSession) Events() <-chan agentwire.Event { return s.events }

func (s *scriptedSession) Prompt(_ context.Context, p agentwire.Prompt) error {
	s.mu.Lock()
	s.prompts = append(s.prompts, p.Text)
	script := append([]agentwire.Event(nil), s.script...)
	s.mu.Unlock()
	for _, e := range script {
		e.Turn = len(s.prompts)
		if e.At.IsZero() {
			e.At = time.Now().UTC()
		}
		s.events <- e
	}
	return nil
}

func (s *scriptedSession) Interrupt(context.Context) error {
	s.mu.Lock()
	s.ints++
	s.mu.Unlock()
	return nil
}

func (s *scriptedSession) AnswerPermission(_ context.Context, permissionID string, d agentwire.Decision) error {
	s.mu.Lock()
	s.answers = append(s.answers, scriptedAnswer{permissionID: permissionID, allow: d.Allow})
	s.mu.Unlock()
	return nil
}

func (s *scriptedSession) InterruptTurn(ctx context.Context) error { return s.Interrupt(ctx) }
func (s *scriptedSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *scriptedSession) Close(context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func (s *scriptedSession) promptTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

func (s *scriptedSession) answersList() []scriptedAnswer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]scriptedAnswer(nil), s.answers...)
}

// workspacesStub satisfies assistant.OverrideStore in memory.
type workspacesStub struct {
	ov    assistant.Override
	hash  string
	name  string
	id    string
	stale string
	mu    sync.Mutex
	dir   string
}

func (w *workspacesStub) AssistantOverride(_ context.Context, _ string) (assistant.Override, string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ov, w.hash, nil
}

func (w *workspacesStub) SetAssistantOverride(_ context.Context, _ string, o assistant.Override, ifMatch string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stale != "" && ifMatch != "" && ifMatch != w.stale {
		return w.stale, errStaleHash
	}
	w.ov = o
	w.stale = "h2"
	return "h2", nil
}

func (w *workspacesStub) WorkspaceInfo(_ context.Context, _ string) (assistant.WorkspaceInfo, error) {
	return assistant.WorkspaceInfo{ID: w.id, Name: w.name, Path: w.dir}, nil
}

var errStaleHash = errors.New("precondition failed")

// pumpHarness builds a manager whose agent sessions come from a scripted
// factory, over one shared store and conversations service.
type pumpHarness struct {
	mu       sync.Mutex
	mgr      *assistant.Manager
	st       *store.Store
	conv     *conversations.Service
	ws       *workspacesStub
	inbox    *fakeInbox
	sessions []*scriptedSession
	factory  func() *scriptedSession
}

func newPumpHarness(t *testing.T, factory func() *scriptedSession) *pumpHarness {
	return newPumpHarnessTiming(t, assistant.ResolverTiming{}, factory)
}

func newPumpHarnessTiming(t *testing.T, timing assistant.ResolverTiming, factory func() *scriptedSession) *pumpHarness {
	t.Helper()
	env := newEnv(t, timing)
	conv := conversations.New(env.st)
	h := &pumpHarness{
		st:    env.st,
		conv:  conv,
		ws:    &workspacesStub{id: env.workspaceID(t), name: "sling", hash: "h1", dir: env.wsDir},
		inbox: &fakeInbox{},
	}
	h.factory = factory
	rt := &factoryRT{h: h}
	h.mgr = assistant.NewManager(rt, assistant.NewStore(env.st), conv, env.resolver, h.ws,
		fakeMemory{}, h.inbox, assistant.Options{MCPURL: "http://127.0.0.1:1/mcp", TrebiExe: "/bin/echo"})
	return h
}

// newConversation creates one assistant conversation.
func (h *pumpHarness) newConversation(t *testing.T) string {
	t.Helper()
	c, err := h.conv.Create(context.Background(), conversations.CreateOpts{
		Workspace: h.ws.id, OriginChannel: conversations.ChannelUI,
		Activate: &conversations.Address{Connector: conversations.ChannelUI, Room: "ui-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// factoryRT hands out scripted sessions.
type factoryRT struct {
	h *pumpHarness
}

func (f *factoryRT) Detect(_ context.Context, h agentwire.Harness) (agentwire.Detection, error) {
	return agentwire.Detection{Harness: h, Supported: true, Auth: agentwire.AuthOK}, nil
}

func (f *factoryRT) Start(context.Context, agentwire.StartRequest) (agentwire.Session, error) {
	s := f.h.factory()
	f.h.mu.Lock()
	f.h.sessions = append(f.h.sessions, s)
	f.h.mu.Unlock()
	return s, nil
}

func (f *factoryRT) Resume(_ context.Context, _ agentwire.StartRequest) (agentwire.Session, error) {
	return nil, agentwire.ErrResumeUnsupported
}

func (h *pumpHarness) lastSession() *scriptedSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sessions) == 0 {
		return nil
	}
	return h.sessions[len(h.sessions)-1]
}

func TestDeltasStreamedButStoredOnce(t *testing.T) {
	h := newPumpHarness(t, func() *scriptedSession {
		s := newScriptedSession("v1")
		s.setScript(
			evt(assistantDelta("Hel")),
			evt(assistantDelta("lo")),
			evt(assistantText("Hello")),
			evt(resultOK()),
		)
		return s
	})
	ctx := context.Background()
	convID := h.newConversation(t)

	ch, cancel := h.mgr.Subscribe(convID)
	defer cancel()

	if _, err := h.mgr.Prompt(ctx, convID, promptInput("hi")); err != nil {
		t.Fatal(err)
	}

	var deltas, stored int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case e := <-ch:
			if e.Type == "assistant" && e.Delta {
				deltas++
			}
			if e.Type == "assistant" && !e.Delta {
				stored++
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
		if stored >= 1 {
			break
		}
	}
	if deltas < 2 {
		t.Fatalf("live deltas = %d", deltas)
	}
	time.Sleep(100 * time.Millisecond)
	events, err := h.mgr.Events(ctx, convID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	storedEvents := 0
	text := ""
	for _, e := range events {
		if e.Type == "assistant" && !e.Delta {
			storedEvents++
			text = e.Text
		}
	}
	if storedEvents != 1 || text != "Hello" {
		t.Fatalf("stored assistant events=%d text=%q", storedEvents, text)
	}
	msgs, _ := h.conv.ListMessages(ctx, convID, conversations.MessageListOpts{Limit: 50})
	assistantRows := 0
	for _, m := range msgs {
		if m.Role == conversations.RoleAssistant {
			assistantRows++
		}
	}
	if assistantRows != 1 {
		t.Fatalf("assistant rows=%d", assistantRows)
	}
}

// TestBlockHookFiresPerBlock: each stored text block fires one BlockClosed
// with its message id, in order. An empty block fires none.
func TestBlockHookFiresPerBlock(t *testing.T) {
	h := newPumpHarness(t, func() *scriptedSession {
		s := newScriptedSession("v1")
		s.setScript(
			evt(assistantText("_Checking…_")),
			evt(assistantText("   ")),
			evt(assistantText("Done.")),
			evt(resultOK()),
		)
		return s
	})
	ctx := context.Background()
	convID := h.newConversation(t)
	var mu sync.Mutex
	var got []assistant.BlockClosed
	h.mgr.OnBlock(func(_ context.Context, b assistant.BlockClosed) {
		mu.Lock()
		got = append(got, b)
		mu.Unlock()
	})
	if _, err := h.mgr.Prompt(ctx, convID, promptInput("hi")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].Message.Text != "_Checking…_" || got[1].Message.Text != "Done." {
		t.Fatalf("blocks = %+v", got)
	}
	msgs, _ := h.conv.ListMessages(ctx, convID, conversations.MessageListOpts{Limit: 50})
	var ids []string
	for _, m := range msgs {
		if m.Role == conversations.RoleAssistant {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) != 2 || ids[0] != got[0].Message.ID || ids[1] != got[1].Message.ID || got[0].ConversationID != convID {
		t.Fatalf("ids %v, blocks %+v", ids, got)
	}
}

func TestFinalEchoOfBufferDropped(t *testing.T) {
	h := newPumpHarness(t, func() *scriptedSession {
		s := newScriptedSession("v1")
		s.setScript(
			evt(assistantDelta("Hel")),
			evt(assistantDelta("lo")),
			evt(assistantText("Hello")),
		)
		return s
	})
	ctx := context.Background()
	convID := h.newConversation(t)
	if _, err := h.mgr.Prompt(ctx, convID, promptInput("hi")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "stored echo-free assistant", func() bool {
		events, _ := h.mgr.Events(ctx, convID, 0, 100)
		n := 0
		for _, e := range events {
			if e.Type == "assistant" && !e.Delta {
				n++
			}
		}
		return n == 1
	})
}

func TestAllowListAutoAnswers(t *testing.T) {
	h := newPumpHarness(t, func() *scriptedSession {
		s := newScriptedSession("v1")
		s.setScript(
			evt(permissionAsk("p1", "mcp__trebi__job")),
			evt(resultOK()),
		)
		return s
	})
	ctx := context.Background()
	convID := h.newConversation(t)
	if _, err := h.mgr.Prompt(ctx, convID, promptInput("hi")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "permission", func() bool {
		_, ok := h.mgr.PendingPermissions(convID)["p1"]
		return ok
	})
	if err := h.mgr.Answer(ctx, convID, "p1", assistant.Decision{Choice: "allow_session"}); err != nil {
		t.Fatal(err)
	}
	// A second turn re-asks the same tool. The session answers it itself.
	if _, err := h.mgr.Prompt(ctx, convID, promptInput("again")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "auto answer", func() bool {
		return len(h.lastSession().answersList()) == 2
	})
	ans := h.lastSession().answersList()[1]
	if ans.permissionID != "p1" || !ans.allow {
		t.Fatalf("answer %+v", ans)
	}
	// The event lands after the harness sees the answer.
	waitFor(t, 2*time.Second, "allow_list answer event", func() bool {
		events, _ := h.mgr.Events(ctx, convID, 0, 100)
		for _, e := range events {
			if e.Type == "answer" && e.Answer != nil && e.Answer.By == "allow_list" {
				return true
			}
		}
		return false
	})
}

func TestPermissionSetsWaitingAndAnswerStores(t *testing.T) {
	h := newPumpHarness(t, func() *scriptedSession {
		s := newScriptedSession("v1")
		s.setScript(evt(permissionAsk("p1", "bash")))
		return s
	})
	ctx := context.Background()
	convID := h.newConversation(t)
	if _, err := h.mgr.Prompt(ctx, convID, promptInput("hi")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "waiting status", func() bool {
		rec, err := h.mgr.Session(ctx, convID)
		return err == nil && rec.Status == assistant.StatusWaiting
	})
	if err := h.mgr.Answer(ctx, convID, "p1", assistant.Decision{Choice: "allow_session"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "running after answer", func() bool {
		rec, err := h.mgr.Session(ctx, convID)
		return err == nil && rec.Status == assistant.StatusRunning
	})
	events, _ := h.mgr.Events(ctx, convID, 0, 100)
	found := false
	for _, e := range events {
		if e.Type == "answer" && e.Answer != nil && e.Answer.Decision == "allow_session" && e.Answer.By == "user" {
			found = true
		}
	}
	if !found {
		t.Fatal("no user answer event stored")
	}
	// The tool is now on the session allow list.
	waitFor(t, time.Second, "session allow entry", func() bool {
		return len(h.lastSession().answersList()) == 1
	})
}

func TestAnsweredPermissionIsUnknown(t *testing.T) {
	h := newPumpHarness(t, func() *scriptedSession {
		s := newScriptedSession("v1")
		s.setScript(evt(permissionAsk("p1", "bash")))
		return s
	})
	ctx := context.Background()
	convID := h.newConversation(t)
	if _, err := h.mgr.Prompt(ctx, convID, promptInput("hi")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "waiting", func() bool {
		rec, err := h.mgr.Session(ctx, convID)
		return err == nil && rec.Status == assistant.StatusWaiting
	})
	if err := h.mgr.Answer(ctx, convID, "p1", assistant.Decision{Choice: "deny"}); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.Answer(ctx, convID, "p1", assistant.Decision{Choice: "deny"}); err == nil {
		t.Fatal("second answer must fail")
	}
}

func TestHeadlessTimeoutDeniesAndOpensInbox(t *testing.T) {
	h := newPumpHarnessTiming(t, assistant.ResolverTiming{PermissionTimeout: 60 * time.Millisecond}, func() *scriptedSession {
		s := newScriptedSession("v1")
		s.setScript(evt(permissionAsk("p1", "bash")))
		return s
	})
	ctx := context.Background()
	convID := h.newConversation(t)
	if _, err := h.mgr.Ask(ctx, convID, assistant.PromptInput{Text: "hi", Headless: true}, time.Second); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "timeout answer", func() bool {
		events, _ := h.mgr.Events(ctx, convID, 0, 100)
		for _, e := range events {
			if e.Type == "answer" && e.Answer != nil && e.Answer.By == "timeout" {
				return true
			}
		}
		return false
	})
	waitFor(t, time.Second, "inbox item", func() bool {
		return len(h.inbox.calls()) == 1
	})
	ans := h.lastSession().answersList()[0]
	if ans.allow {
		t.Fatal("timeout must deny")
	}
}

func TestUsageTotals(t *testing.T) {
	h := newPumpHarness(t, func() *scriptedSession {
		s := newScriptedSession("v1")
		s.setScript(
			evt(usage(100, 20)),
			evt(resultOK()),
		)
		return s
	})
	ctx := context.Background()
	convID := h.newConversation(t)
	if _, err := h.mgr.Prompt(ctx, convID, promptInput("hi")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "usage totals", func() bool {
		rec, err := h.mgr.Session(ctx, convID)
		return err == nil && rec.TokensIn == 100 && rec.TokensOut == 20
	})
}

func TestRevertRestoresSnapshot(t *testing.T) {
	h := newPumpHarness(t, func() *scriptedSession {
		s := newScriptedSession("v1")
		s.setScript(
			evt(toolEdit("jobs/job.md")),
			evt(resultOK()),
		)
		return s
	})
	dir := h.ws.dir
	file := filepath.Join(dir, "jobs", "job.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	convID := h.newConversation(t)
	if _, err := h.mgr.Prompt(ctx, convID, promptInput("hi")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "turn end", func() bool {
		rec, err := h.mgr.Session(ctx, convID)
		return err == nil && (rec.Status == assistant.StatusIdle || rec.Status == assistant.StatusClosed)
	})
	// The session captured the file before the edit event.
	if err := os.WriteFile(file, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.Revert(ctx, convID, 1, file); err != nil {
		t.Fatalf("revert: %v", err)
	}
	data, _ := os.ReadFile(file)
	if string(data) != "before" {
		t.Fatalf("reverted content %q", data)
	}
}

func TestRevertWithoutSnapshotIsNoSnapshot(t *testing.T) {
	h := newPumpHarness(t, func() *scriptedSession {
		s := newScriptedSession("v1")
		s.setScript(evt(resultOK()))
		return s
	})
	ctx := context.Background()
	convID := h.newConversation(t)
	if _, err := h.mgr.Prompt(ctx, convID, promptInput("hi")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "turn end", func() bool {
		rec, err := h.mgr.Session(ctx, convID)
		return err == nil && (rec.Status == assistant.StatusIdle || rec.Status == assistant.StatusClosed)
	})
	if _, err := h.mgr.Revert(ctx, convID, 9, "/nope"); err == nil {
		t.Fatal("want no-snapshot error")
	}
}

// --- event builders ---

func evt(e agentwire.Event) agentwire.Event {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	return e
}

func assistantDelta(text string) agentwire.Event {
	return agentwire.Event{Type: agentwire.EventAssistant, Text: text, Delta: true}
}

func assistantText(text string) agentwire.Event {
	return agentwire.Event{Type: agentwire.EventAssistant, Text: text}
}

func resultOK() agentwire.Event {
	return agentwire.Event{Type: agentwire.EventResult, Result: &agentwire.Result{Subtype: "success"}}
}

func permissionAsk(id, tool string) agentwire.Event {
	return agentwire.Event{Type: agentwire.EventPermission,
		Permission: &agentwire.Permission{ID: id, Tool: tool, Question: "allow?"}}
}

func usage(in, out int64) agentwire.Event {
	return agentwire.Event{Type: agentwire.EventUsage, Usage: &agentwire.Usage{Input: in, Output: out}}
}

func toolEdit(path string) agentwire.Event {
	return agentwire.Event{Type: agentwire.EventTool,
		Tool: &agentwire.ToolEvent{ID: "t1", Name: "edit", Status: "started", Paths: []string{path}}}
}
