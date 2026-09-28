package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestAgentChannelItemQueuesReply proves the inbound side of a connector
// channel for an agent lane (plan G.2, G.3): the dispatched item writes a
// channel-typed user turn into the conversation the channel points at, and
// the turn that follows it becomes one pending outbox row. The extract sets
// a thread key; the reply still goes to the event's channel ref (P1-29).
func TestAgentChannelItemQueuesReply(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	agentDir := filepath.Join(dir, "ws", "agents", "ops")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"),
		[]byte("---\ntitle: Ops\nharness: fake\n---\n\nAnswer the message.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "connectors.yaml"), []byte(`chat:
  events:
    command: 'cat'
    format: json
  channel:
    ref: chat_id
    send: 'cat >> /dev/null'
`), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	cp := daemon.New(st, daemon.Options{QueueInterval: 30 * time.Millisecond})
	ws, err := cp.Workspaces.Add(ctx, filepath.Join(dir, "ws"), "ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Workspaces.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}

	if _, err := st.DB().ExecContext(ctx, `INSERT INTO events
		(id, connector, instance, received_at, summary, raw_json, trace_id, room) VALUES (?,?,?,?,?,?,?,?)`,
		"evt-1", "chat", "chat", time.Now().UTC().Format(time.RFC3339Nano), "", "{}", "tr", "u1"); err != nil {
		t.Fatal(err)
	}
	item := &dispatch.QueueItem{
		Connector:       "chat",
		WorkspaceID:     ws.ID,
		Job:             "agents/ops",
		Kind:            "agent",
		ConversationKey: "thread-7",
		EventID:         "evt-1",
		EventRaw:        `{"chat_id":"u1","text":"status?"}`,
		Prompt:          "status?",
		Status:          dispatch.QueuePending,
	}
	if err := dispatch.NewQueue(st).Enqueue(ctx, item); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	if status, _, _ := waitQueue(t, st, item.ID, func(s, _, _ string) bool { return s == "done" }); status != "done" {
		t.Fatalf("item status=%s want done", status)
	}

	var convID, channelType, channelRef, text string
	if err := st.DB().QueryRowContext(ctx, `SELECT conversation_id, connector, room, text
		FROM conversation_messages WHERE role='user' ORDER BY id LIMIT 1`).
		Scan(&convID, &channelType, &channelRef, &text); err != nil {
		t.Fatalf("user turn: %v", err)
	}
	if channelType != "chat" || channelRef != "u1" || text != "status?" {
		t.Fatalf("user turn %q %q %q; want chat u1 status?", channelType, channelRef, text)
	}

	// The reply of a turn arrives as one pending outbox row, and a second
	// call never writes the row twice.
	if _, err := cp.Conversations.AppendAssistant(ctx, convID, 1, "all good"); err != nil {
		t.Fatal(err)
	}
	if err := cp.Channels.ReplyOnTurn(ctx, convID, ""); err != nil {
		t.Fatal(err)
	}
	if err := cp.Channels.ReplyOnTurn(ctx, convID, ""); err != nil {
		t.Fatal(err)
	}
	var rows int
	var status, ref string
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*), MIN(status), MIN(room) FROM outbox WHERE kind='channel'`).
		Scan(&rows, &status, &ref); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || status != "pending" || ref != "u1" {
		t.Fatalf("outbox rows=%d status=%q ref=%q; want 1 pending u1", rows, status, ref)
	}
}

// TestChannelPointerIsWorkspaceScoped proves EnsureForChannel keys the pointer
// by workspace (plan G.2): two workspaces hold two conversations for one ref.
func TestChannelPointerIsWorkspaceScoped(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	conv := conversations.New(st)
	first, err := conv.EnsureForChannel(ctx, "ws-a", "", conversations.Address{Connector: "chat", Room: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := conv.EnsureForChannel(ctx, "ws-b", "", conversations.Address{Connector: "chat", Room: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("both workspaces point at conversation %s; want one each", first)
	}
	again, err := conv.EnsureForChannel(ctx, "ws-a", "", conversations.Address{Connector: "chat", Room: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("second ensure=%s want %s", again, first)
	}
}
