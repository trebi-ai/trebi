package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
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
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	cp := daemon.New(st, daemon.Options{QueueInterval: 30 * time.Millisecond})
	chat, err := cp.Connectors.Create(ctx, connectors.Connection{Type: "chat", Slug: "chat", From: connectors.FromInline, Manifest: &manifest.Manifest{
		Schema: manifest.SchemaID, Name: "chat", Title: "Chat", Description: "Test chat", Version: "1.0.0",
		Events: &manifest.Events{
			Protocol: manifest.ProtocolConnector, Command: "sleep 60",
			Types: []manifest.EventType{{Type: "message"}},
		},
		Channel: &manifest.Channel{Features: []string{"typing"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := cp.Workspaces.Add(ctx, filepath.Join(dir, "ws"), "ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Workspaces.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}

	if _, err := st.DB().ExecContext(ctx, `INSERT INTO events
		(id, connector, instance, received_at, summary, raw_json, trace_id, room) VALUES (?,?,?,?,?,?,?,?)`,
		"evt-1", "chat", chat.ID, time.Now().UTC().Format(time.RFC3339Nano), "", "{}", "tr", "u1"); err != nil {
		t.Fatal(err)
	}
	item := &dispatch.QueueItem{
		Connector:   chat.ID,
		WorkspaceID: ws.ID,
		JobID:       "agents/ops", JobName: "agents/ops",
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

	var convID, channelType, instance, channelRef, text string
	if err := st.DB().QueryRowContext(ctx, `SELECT conversation_id, connector, instance, room, text
		FROM conversation_messages WHERE role='user' ORDER BY id LIMIT 1`).
		Scan(&convID, &channelType, &instance, &channelRef, &text); err != nil {
		t.Fatalf("user turn: %v", err)
	}
	if channelType != "chat" || instance != chat.ID || channelRef != "u1" || text != "status?" {
		t.Fatalf("user turn %q %q %q %q; want chat %s u1 status?", channelType, instance, channelRef, text, chat.ID)
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

// TestJobFollowUpPrompt proves the follow-up texts (plan A.11): a fresh
// session gets the job prompt with the first vars, the recap, the lookup
// line, and the new message.
func TestJobFollowUpPrompt(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	out := filepath.Join(dir, "prompt")
	t.Setenv("TREBI_FAKE_HARNESS_CMD", `printf %s "$TREBI_FAKE_PROMPT" > `+out+`; echo "$TREBI_RUN_COMPLETE"`)
	t.Cleanup(func() { home.SetBase("") })

	jobDir := filepath.Join(dir, "ws", "deploy")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\ntriggers:\n  - event: chat\n    replies: continue\n---\n\nInvestigate {{text}}.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	cp := daemon.New(st, daemon.Options{QueueInterval: 30 * time.Millisecond})
	chat, err := cp.Connectors.Create(ctx, connectors.Connection{Type: "chat", Slug: "chat", From: connectors.FromInline, Manifest: &manifest.Manifest{
		Schema: manifest.SchemaID, Name: "chat", Title: "Chat", Description: "Test chat", Version: "1.0.0",
		Events: &manifest.Events{
			Protocol: manifest.ProtocolConnector, Command: "sleep 60",
			Types: []manifest.EventType{{Type: "message"}},
		},
		Channel: &manifest.Channel{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := cp.Workspaces.Add(ctx, filepath.Join(dir, "ws"), "ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Workspaces.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, e := range []struct{ id, sender, text, raw string }{
		{"evt-1", "ann", "the deploy", `{"sender":{"id":"u1"}}`},
		{"evt-2", "me", "on it", `{"sender":{"id":"u0","self":true}}`},
		{"evt-3", "bob", "thanks", `{"sender":{"id":"u2"}}`},
	} {
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO events
			(id, connector, instance, received_at, summary, raw_json, trace_id, room, sender, text, root, source_id) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.id, "chat", chat.ID, now, "", e.raw, "tr", "r1", e.sender, e.text, "m1", e.id); err != nil {
			t.Fatal(err)
		}
	}
	first, err := cp.Runs.Create(ctx, runs.CreateOpts{
		WorkspaceID: ws.ID, JobID: "deploy", JobName: "deploy", Harness: "fake", ConversationKey: "r1/m1",
		Trigger: &runs.TriggerCapture{Vars: map[string]string{"text": "the deploy"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Runs.Terminate(ctx, first.ID, runs.StatusFailed, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	item := &dispatch.QueueItem{
		Connector: chat.ID, WorkspaceID: ws.ID, JobID: "deploy", JobName: "deploy", Kind: "job",
		ConversationKey: "r1/m1", FollowUp: true, EventID: "evt-3", EventRaw: "{}",
		Vars:   map[string]string{"text": "thanks", "sender": "bob", "room": "r1", "connection": "chat", "thread": ""},
		Status: dispatch.QueuePending,
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
	deadline := time.Now().Add(5 * time.Second)
	b, _ := os.ReadFile(out)
	for ; len(b) == 0 && time.Now().Before(deadline); b, _ = os.ReadFile(out) {
		time.Sleep(20 * time.Millisecond)
	}
	want := "Investigate the deploy.\n\n" +
		"Messages in this conversation so far:\nann: the deploy\nyou: on it\n\n" +
		"Earlier messages: connection chat, room r1. Read more with the history command of the connector.\n\n" +
		"New message in this conversation from bob:\n\nthanks"
	if string(b) != want {
		t.Fatalf("prompt =\n%s\nwant\n%s", b, want)
	}
}
