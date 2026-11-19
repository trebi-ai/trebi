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
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/waittest"
)

// TestAgentChannelItemQueuesReply proves the inbound side of a connector
// channel for an agent lane (plan G.2, G.3): the dispatched item writes a
// channel-typed user turn into the conversation the channel points at, and
// the turn that follows it becomes one pending outbox row. The extract sets
// a thread key; the reply still goes to the event's channel ref (P1-29).
// The item is the shape a trigger queues: the message is only the text var,
// never the AGENT.md body.
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
		JobID:       indexID(t, st, ws.ID, "ops"), JobName: "ops",
		Kind:            "agent",
		ConversationKey: "thread-7",
		EventID:         "evt-1",
		EventRaw:        `{"chat_id":"u1","text":"status?"}`,
		Vars:            map[string]string{"text": "status?"},
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

	// Each block of a turn arrives as its own pending outbox row, and a
	// second call never writes a row twice.
	if _, err := cp.Conversations.AppendAssistant(ctx, convID, 1, "_checking_"); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.Conversations.AppendAssistant(ctx, convID, 1, "all good"); err != nil {
		t.Fatal(err)
	}
	if err := cp.Channels.SendReplies(ctx, convID, ""); err != nil {
		t.Fatal(err)
	}
	if err := cp.Channels.SendReplies(ctx, convID, ""); err != nil {
		t.Fatal(err)
	}
	var rows int
	var status, ref string
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*), MIN(status), MIN(room) FROM outbox WHERE kind='channel'`).
		Scan(&rows, &status, &ref); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || status != "pending" || ref != "u1" {
		t.Fatalf("outbox rows=%d status=%q ref=%q; want 2 pending u1", rows, status, ref)
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
	// The failed first run wakes agent ops too; keep only the job prompt.
	t.Setenv("TREBI_FAKE_HARNESS_CMD", `case "$TREBI_FAKE_PROMPT" in Investigate*) printf %s "$TREBI_FAKE_PROMPT" > `+out+`;; esac; echo "$TREBI_RUN_COMPLETE"`)
	t.Cleanup(func() { home.SetBase("") })

	jobDir := opsJobDir(t, filepath.Join(dir, "ws"), "deploy")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\n---\n\nInvestigate {{text}}.\n"
	routingtest.Write(t, filepath.Join(dir, "ws"), "chat", routing.File{Routes: []routing.Route{{
		ID: "rt_00000000b1", Job: "ops/deploy", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Replies: "continue"},
	}}})
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
		WorkspaceID: ws.ID, JobID: indexID(t, st, ws.ID, "ops/deploy"), JobName: "ops/deploy", Harness: "fake", ConversationKey: "r1/m1",
		Trigger: &runs.TriggerCapture{Vars: map[string]string{"text": "the deploy"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Runs.Terminate(ctx, first.ID, runs.StatusFailed, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	item := &dispatch.QueueItem{
		Connector: chat.ID, WorkspaceID: ws.ID, JobID: indexID(t, st, ws.ID, "ops/deploy"), JobName: "ops/deploy", Kind: "job",
		ConversationKey: "r1/m1", FollowUp: true, EventID: "evt-3", RouteID: "rt_00000000b1", EventRaw: "{}",
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
	var b []byte
	waittest.Until(quietT{t}, "the prompt file", func() bool { b, _ = os.ReadFile(out); return len(b) > 0 })
	want := "Investigate the deploy.\n\n" +
		"Messages in this conversation so far:\nann: the deploy\nyou: on it\n\n" +
		"Earlier messages: connection chat, room r1. Read more with the history command of the connector.\n\n" +
		"New message in this conversation from bob:\n\nthanks"
	if string(b) != want {
		t.Fatalf("prompt =\n%s\nwant\n%s", b, want)
	}
}

// TestReplyJobBindsOwnerConversation proves the chat side of a job trigger
// (TREBI-650): a chat: reply job writes the inbound message into the chat of
// its owner agent and runs in that conversation. A listen job (no chat)
// writes nothing to a chat. An app message into the claimed chat queues a
// run of the reply job, not a wake of the agent.
func TestReplyJobBindsOwnerConversation(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	files := map[string]string{
		"ws/agents/ellis/AGENT.md":      "---\ntitle: Ellis\nharness: fake\n---\n\nEllis.\n",
		"ws/agents/ellis/italy/JOB.md":  "---\nharness: fake\n---\n\nAnswer about Italy.\n",
		"ws/agents/ellis/listen/JOB.md": "---\nharness: fake\n---\n\nTake notes.\n",
	}
	routingtest.Write(t, filepath.Join(dir, "ws"), "chat", routing.File{Routes: []routing.Route{
		{ID: "rt_00000000c1", Job: "ellis/italy", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Rooms: []string{"r1"}, Chat: "reply"}},
		{ID: "rt_00000000c2", Job: "ellis/listen", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Rooms: []string{"r2"}}},
	}})
	for rel, md := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
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
	ellisID := indexID(t, st, ws.ID, "ellis")
	italyID := indexID(t, st, ws.ID, "ellis/italy")

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	q := dispatch.NewQueue(st)
	enqueue := func(evID, room, jobName, route string) *dispatch.QueueItem {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO events
			(id, connector, instance, received_at, summary, raw_json, trace_id, room) VALUES (?,?,?,?,?,?,?,?)`,
			evID, "chat", chat.ID, time.Now().UTC().Format(time.RFC3339Nano), "", "{}", "tr-"+evID, room); err != nil {
			t.Fatal(err)
		}
		item := &dispatch.QueueItem{
			Connector: chat.ID, WorkspaceID: ws.ID,
			JobID: indexID(t, st, ws.ID, jobName), JobName: jobName,
			Kind: runs.KindJob, ConversationKey: room, EventID: evID, RouteID: route,
			EventRaw: `{"chat_id":"` + room + `","text":"hello"}`,
			Vars:     map[string]string{"text": "hello", "room": room},
			Status:   dispatch.QueuePending,
		}
		if err := q.Enqueue(ctx, item); err != nil {
			t.Fatal(err)
		}
		if status, _, _ := waitQueue(t, st, item.ID, func(s, _, _ string) bool { return s == "done" }); status != "done" {
			t.Fatalf("%s item status=%s want done", jobName, status)
		}
		return item
	}

	enqueue("evt-r1", "r1", "ellis/italy", "rt_00000000c1")
	var convID, operator, text string
	if err := st.DB().QueryRowContext(ctx, `SELECT m.conversation_id, c.operator_id, m.text
		FROM conversation_messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.role='user' AND m.room='r1'`).Scan(&convID, &operator, &text); err != nil {
		t.Fatalf("user turn: %v", err)
	}
	if operator != ellisID || text != "hello" {
		t.Fatalf("operator=%s text=%q; want %s hello", operator, text, ellisID)
	}
	var runConv, runKind string
	if err := st.DB().QueryRowContext(ctx, `SELECT COALESCE(conversation_id,''), kind FROM runs WHERE job_id=?`, italyID).
		Scan(&runConv, &runKind); err != nil {
		t.Fatal(err)
	}
	if runConv != convID || runKind != runs.KindJob {
		t.Fatalf("run conversation=%q kind=%s; want %s job", runConv, runKind, convID)
	}

	enqueue("evt-r2", "r2", "ellis/listen", "rt_00000000c2")
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_messages WHERE room='r2'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("listen job wrote %d chat rows; want 0", n)
	}

	conv, err := cp.Conversations.Get(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	if conv.JobID != italyID || conv.JobName != "ellis/italy" {
		t.Fatalf("conversation job=%q %q; want %s ellis/italy", conv.JobID, conv.JobName, italyID)
	}
	jobID, jobName, ok := cp.HandlerJob(ctx, conv)
	if !ok || jobID != italyID || jobName != "ellis/italy" {
		t.Fatalf("handler=%s %s %v; want %s ellis/italy", jobID, jobName, ok, italyID)
	}
	if _, err := cp.Conversations.Send(ctx, convID, conversations.SendOpts{Text: "from the app"}); err != nil {
		t.Fatal(err)
	}
	var kind, itemJob string
	if err := st.DB().QueryRowContext(ctx, `SELECT kind, job_id FROM event_queue WHERE connector=? ORDER BY id DESC LIMIT 1`,
		conversations.ConnectorConversation).Scan(&kind, &itemJob); err != nil {
		t.Fatal(err)
	}
	if kind != runs.KindJob || itemJob != italyID {
		t.Fatalf("app item kind=%s job=%s; want job %s", kind, itemJob, italyID)
	}
}
