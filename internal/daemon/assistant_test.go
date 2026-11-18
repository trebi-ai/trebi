package daemon_test

// Assistant session wiring in the daemon (plan 2026-09-26 K.1): a trigger
// item prompts the session through the queue lane, drain holds assistant
// items, and the turn end hook fires per turn.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/waittest"
)

func TestTriggerItemPromptsAssistantSession(t *testing.T) {
	dir := t.TempDir()
	stopHolders(t, dir)
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	// Stay alive after the turn like a real CLI, so Stop has a session to close.
	t.Setenv(agentwire.FakeScriptEnv, `while IFS= read -r line; do
echo '{"type":"assistant","text":"working"}'
echo '{"type":"result","subtype":"success","text":"done"}'
done`)
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := daemon.New(st, daemon.Options{QueueInterval: 20 * time.Millisecond})
	ctx := context.Background()

	wsPath := filepath.Join(dir, "ws")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := cp.Workspaces.Add(ctx, wsPath, "trig-ws")
	if err != nil {
		t.Fatal(err)
	}
	// Pin the fake harness: the driver below serves it.
	fake := "fake"
	if _, err := cp.Workspaces.SetAssistantOverride(ctx, ws.ID,
		assistant.Override{Harness: &fake}, ""); err != nil {
		t.Fatal(err)
	}

	turns := make(chan string, 4)
	cp.Assistant().OnTurnEnd(func(ctx context.Context, te assistant.TurnEnd) {
		turns <- te.Reply.Text
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	stopped := false
	defer func() {
		if !stopped {
			cp.Stop()
		}
	}()

	q := dispatch.NewQueue(st)
	item := &dispatch.QueueItem{
		Connector:   "test-conn",
		WorkspaceID: ws.ID,
		JobID:       ws.ID, JobName: "assistant",
		Kind:            dispatch.KindAssistant,
		EventID:         "evt-1",
		ConversationKey: "k1",
		Prompt:          "Summarize the trigger",
		Status:          dispatch.QueuePending,
	}
	if err := q.Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}

	// The lane creates the channel conversation, prompts the session, and
	// marks the item done.
	var convID string
	waittest.Until(t, "the assistant item to complete into a conversation", func() bool {
		rows, err := q.List(ctx, dispatch.ListOpts{Workspace: ws.ID})
		if err != nil {
			return false
		}
		for _, row := range rows {
			if row.ID == item.ID && row.Status == dispatch.QueueDone {
				p, err := cp.Conversations.GetPointer(ctx, ws.ID, conversations.Address{Connector: "test-conn", Room: "k1"})
				if err == nil {
					convID = p.ConversationID
				}
			}
		}
		return convID != ""
	})

	waitForStatus(t, cp, convID, assistant.StatusIdle, assistant.StatusClosed)
	msgs, _ := cp.Conversations.ListMessages(ctx, convID, conversations.MessageListOpts{Limit: 20})
	var replied bool
	for _, m := range msgs {
		if m.Role == conversations.RoleAssistant && m.Text == "working" {
			replied = true
		}
	}
	if !replied {
		t.Fatalf("no assistant reply: %+v", msgs)
	}

	// The turn end hook fired with the reply (the email reply rides it).
	select {
	case text := <-turns:
		if text != "working" {
			t.Fatalf("turn end reply %q", text)
		}
	case <-time.After(waittest.D(waittest.Default)):
		t.Fatal("turn end hook never fired")
	}

	// No assistant session outlives the daemon.
	if !cp.Assistant().Live(convID) {
		t.Fatal("session not live before stop")
	}
	stopped = true
	cp.Stop()
	if cp.Assistant().Live(convID) {
		t.Fatal("session live after stop")
	}
	if rec, err := cp.AssistantStore().Get(ctx, convID); err != nil || rec.Status != assistant.StatusClosed {
		t.Fatalf("status after stop = %q, %v; want closed", rec.Status, err)
	}
}

func waitForStatus(t *testing.T, cp *daemon.ControlPlane, convID string, statuses ...string) {
	t.Helper()
	want := map[string]bool{}
	for _, s := range statuses {
		want[s] = true
	}
	waittest.Until(t, fmt.Sprintf("the session to reach %v", statuses), func() bool {
		rec, err := cp.AssistantStore().Get(context.Background(), convID)
		return err == nil && want[rec.Status]
	})
}

// TestDrainHoldsAssistantItems: the drain flag holds queued assistant items
// like every other kind (plan E).
func TestDrainHoldsAssistantItems(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetFlag("drain", "1"); err != nil {
		t.Fatal(err)
	}

	cp := daemon.New(st, daemon.Options{QueueInterval: 20 * time.Millisecond})
	ctx := context.Background()
	wsPath := filepath.Join(dir, "ws")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := cp.Workspaces.Add(ctx, wsPath, "drain-ws")
	if err != nil {
		t.Fatal(err)
	}
	var fired atomic.Int64
	cp.Assistant().OnTurnEnd(func(context.Context, assistant.TurnEnd) { fired.Add(1) })

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	q := dispatch.NewQueue(st)
	item := &dispatch.QueueItem{
		Connector: "test-conn", WorkspaceID: ws.ID, JobID: ws.ID, JobName: "assistant",
		Kind: dispatch.KindAssistant, EventID: "evt-drain",
		Status: dispatch.QueuePending,
	}
	if err := q.Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	rows, _ := q.List(ctx, dispatch.ListOpts{Workspace: ws.ID})
	for _, row := range rows {
		if row.ID == item.ID && row.Status != dispatch.QueuePending {
			t.Fatalf("drained process picked up the item: %s", row.Status)
		}
	}
	if fired.Load() != 0 {
		t.Fatal("turn end fired under drain")
	}
}
