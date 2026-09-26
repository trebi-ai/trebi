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
		assistant.DefaultsFromConfig(cfg), wsSvc, rt, cfg.Assistant.Native, timing)

	router := assistant.NewHarnessRouter(rt, assistant.NewNativeDriver(cfg.Assistant.Native, assistant.NewStore(st), nil, 0, nil))
	crt := &countingRT{inner: router}

	convSvc := conversations.New(st)
	inbox := &fakeInbox{}
	mgr := assistant.NewManager(crt, assistant.NewStore(st), convSvc, resolver, wsSvc,
		fakeMemory{text: "remember the release cadence"}, inbox, assistant.Options{
			MCPURL:    "http://127.0.0.1:1/mcp",
			TrebiExe:  "/bin/echo",
			SkillsDir: home.SkillsDir(),
			Log:       nil,
		})
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
		Activate: &conversations.Activate{ChannelType: conversations.ChannelUI, ChannelRef: "ui-1"},
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
		Text: "hello", ChannelType: conversations.ChannelUI, ChannelRef: "ui-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicate || res.Status != assistant.StatusRunning {
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
		Workspace: env.workspaceID(t), Operator: "agents/nightly",
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

func assistantResolverTimingZero() assistant.ResolverTiming { return assistant.ResolverTiming{} }

func promptInput(text string) assistant.PromptInput {
	return assistant.PromptInput{Text: text, ChannelType: conversations.ChannelUI, ChannelRef: "ui-1"}
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
