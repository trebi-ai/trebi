package channels_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/channels"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/store"
)

// fakeDefs is the channel view of a connectors service without a file.
type fakeDefs struct {
	channels map[string]connectors.Channel
	envs     map[string]map[string]string
	cwds     map[string]string
}

func (f *fakeDefs) Channel(name string) (connectors.Channel, map[string]string, bool) {
	ch, ok := f.channels[name]
	return ch, f.envs[name], ok
}

func (f *fakeDefs) Cwd(name string) string { return f.cwds[name] }

type fixture struct {
	t     *testing.T
	dir   string
	st    *store.Store
	convs *conversations.Service
	defs  *fakeDefs
	svc   *channels.Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	convs := conversations.New(st)
	defs := &fakeDefs{
		channels: map[string]connectors.Channel{},
		envs:     map[string]map[string]string{},
		cwds:     map[string]string{},
	}
	return &fixture{t: t, dir: dir, st: st, convs: convs, defs: defs,
		svc: channels.New(st, defs, convs)}
}

func (f *fixture) conv(workspace string) string {
	f.t.Helper()
	c, err := f.convs.Create(context.Background(), conversations.CreateOpts{
		Workspace: workspace, OriginChannel: "cli",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return c.ID
}

// addChannel registers a connector channel with a send command.
func (f *fixture) addChannel(name, send string) {
	f.defs.channels[name] = connectors.Channel{Ref: "id", Send: send}
}

func (f *fixture) userMsg(convID, channelType, ref, text string) {
	f.t.Helper()
	f.insertMsg(ulid.Make().String(), convID, "user", channelType, ref, text, "", false)
}

func (f *fixture) assistantMsg(convID, runID, text string) {
	f.t.Helper()
	f.insertMsg(ulid.Make().String(), convID, "assistant", "", "", text, runID, true)
}

func (f *fixture) insertMsg(id, convID, role, channelType, ref, text, runID string, turn bool) {
	f.t.Helper()
	var run any
	if runID != "" {
		run = runID
	}
	var turnIdx any
	if turn {
		turnIdx = 1
	}
	_, err := f.st.DB().Exec(`INSERT INTO conversation_messages
		(id, conversation_id, role, channel_type, channel_ref, text, run_id, turn_index, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		id, convID, role, channelType, ref, text, run, turnIdx,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		f.t.Fatal(err)
	}
}

// TestReplyOnTurnRunAndSession: one outbox row per assistant turn, for a run
// and for the assistant session, and the message id keeps it unique.
func TestReplyOnTurnRunAndSession(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addChannel("telegram", "true")
	ctx := context.Background()

	runConv := f.conv("ws1")
	f.userMsg(runConv, "telegram", "chat1", "hello")
	f.assistantMsg(runConv, "r1", "the answer")
	if err := f.svc.ReplyOnTurn(ctx, runConv, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ReplyOnTurn(ctx, runConv, "r1"); err != nil {
		t.Fatal(err) // the message id dedupes a second write
	}
	rows, err := f.svc.ListOutbox(ctx, runConv)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("run outbox rows = %d, want 1", len(rows))
	}
	if rows[0].Text != "the answer" || rows[0].ChannelRef != "chat1" || rows[0].Connector != "telegram" {
		t.Fatalf("run row %+v", rows[0])
	}
	if rows[0].Workspace != "ws1" || rows[0].RunID != "r1" {
		t.Fatalf("run row workspace/run %+v", rows[0])
	}

	// The assistant session has run_id NULL.
	sessionConv := f.conv("ws1")
	f.userMsg(sessionConv, "telegram", "chat2", "ping")
	f.assistantMsg(sessionConv, "", "pong")
	if err := f.svc.ReplyOnTurn(ctx, sessionConv, ""); err != nil {
		t.Fatal(err)
	}
	rows, err = f.svc.ListOutbox(ctx, sessionConv)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Text != "pong" || rows[0].RunID != "" {
		t.Fatalf("session row %+v", rows)
	}

	// A conversation whose last user message is not a channel connector is
	// left alone.
	plain := f.conv("ws1")
	f.userMsg(plain, "", "", "hi")
	f.assistantMsg(plain, "", "reply")
	if err := f.svc.ReplyOnTurn(ctx, plain, ""); err != nil {
		t.Fatal(err)
	}
	if rows, _ = f.svc.ListOutbox(ctx, plain); len(rows) != 0 {
		t.Fatalf("plain conversation got %+v", rows)
	}
}

// TestSendDueStdinEnvAndSuccess: the reply text goes on stdin, the channel env
// is set, and the connector cwd applies.
func TestSendDueStdinEnvAndSuccess(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cwd := filepath.Join(f.dir, "cwd")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	f.addChannel("telegram", `cat > body.txt && printf '%s|%s' "$TREBI_CHANNEL_REF" "$TREBI_CONVERSATION_ID" > env.txt`)
	f.defs.cwds["telegram"] = cwd
	f.defs.envs["telegram"] = map[string]string{"EXTRA": "x"}
	ctx := context.Background()

	convID := f.conv("ws1")
	f.userMsg(convID, "telegram", "chat1", "hello")
	f.assistantMsg(convID, "r1", "the reply text")
	if err := f.svc.ReplyOnTurn(ctx, convID, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SendDue(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(cwd, "body.txt"))
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	if string(body) != "the reply text" {
		t.Fatalf("stdin body = %q", body)
	}
	env, err := os.ReadFile(filepath.Join(cwd, "env.txt"))
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if want := "chat1|" + convID; string(env) != want {
		t.Fatalf("env = %q, want %q", env, want)
	}
	rows, _ := f.svc.ListOutbox(ctx, convID)
	if len(rows) != 1 || rows[0].Status != channels.StatusSent || rows[0].SentAt == "" {
		t.Fatalf("sent row %+v", rows)
	}
}

// TestSendDueRetryBackoff: a failed send backs off 5s << attempts.
func TestSendDueRetryBackoff(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addChannel("telegram", "exit 1")
	ctx := context.Background()

	convID := f.conv("ws1")
	f.userMsg(convID, "telegram", "chat1", "hello")
	f.assistantMsg(convID, "r1", "reply")
	if err := f.svc.ReplyOnTurn(ctx, convID, "r1"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if err := f.svc.SendDue(ctx, t0); err != nil {
		t.Fatal(err)
	}
	row := f.onlyOutbox(ctx, convID)
	if row.Attempts != 1 || row.Status != channels.StatusPending {
		t.Fatalf("after first failure %+v", row)
	}
	wantNext := t0.Add(10 * time.Second).UTC().Format(time.RFC3339)
	if row.NextAttemptAt != wantNext {
		t.Fatalf("next = %q, want %q", row.NextAttemptAt, wantNext)
	}
	// Before the due time the row stays.
	if err := f.svc.SendDue(ctx, t0.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if row = f.onlyOutbox(ctx, convID); row.Attempts != 1 {
		t.Fatalf("not-due row advanced: %+v", row)
	}
	// At the due time the second attempt runs.
	if err := f.svc.SendDue(ctx, t0.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	row = f.onlyOutbox(ctx, convID)
	if row.Attempts != 2 {
		t.Fatalf("second attempt %+v", row)
	}
	wantNext = t0.Add(30 * time.Second).UTC().Format(time.RFC3339)
	if row.NextAttemptAt != wantNext {
		t.Fatalf("second next = %q, want %q", row.NextAttemptAt, wantNext)
	}
}

// TestSendDueFailsAfterFiveWithInbox: five failures fail the row, emit
// channel.send_failed, and open the inbox item.
func TestSendDueFailsAfterFiveWithInbox(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addChannel("telegram", "exit 1")
	ctx := context.Background()

	convID := f.conv("ws1")
	f.userMsg(convID, "telegram", "chat1", "hello")
	f.assistantMsg(convID, "r1", "reply")
	if err := f.svc.ReplyOnTurn(ctx, convID, "r1"); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if err := f.svc.SendDue(ctx, time.Now().Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	row := f.onlyOutbox(ctx, convID)
	if row.Status != channels.StatusFailed || row.Attempts != 5 {
		t.Fatalf("failed row %+v", row)
	}

	frames, err := f.st.ListBusWith(ctx, store.ListBusOpts{Topics: []string{"channel.send_failed"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("channel.send_failed frames = %d, want 1", len(frames))
	}
	var attrs map[string]string
	if err := json.Unmarshal(frames[0].Attrs, &attrs); err != nil {
		t.Fatal(err)
	}
	if attrs["connector"] != "telegram" || attrs["conversation_id"] != convID {
		t.Fatalf("attrs %+v", attrs)
	}

	alertSvc := alerts.New(f.st, alerts.Options{})
	alertSvc.HandleEvent(ctx, frames[0])
	var count int
	if err := f.st.DB().QueryRow(`SELECT COUNT(*) FROM inbox_items
		WHERE alert_kind='channel_send_failed' AND subject=?`, "conversation/"+convID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("inbox items = %d, want 1", count)
	}
}

// TestRetryOutbox: a failed row goes back to pending with attempts = 0.
func TestRetryOutbox(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addChannel("telegram", "exit 1")
	ctx := context.Background()

	convID := f.conv("ws1")
	f.userMsg(convID, "telegram", "chat1", "hello")
	f.assistantMsg(convID, "r1", "reply")
	if err := f.svc.ReplyOnTurn(ctx, convID, "r1"); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if err := f.svc.SendDue(ctx, time.Now().Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	row := f.onlyOutbox(ctx, convID)
	retried, err := f.svc.RetryOutbox(ctx, convID, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != channels.StatusPending || retried.Attempts != 0 || retried.NextAttemptAt != "" {
		t.Fatalf("retried %+v", retried)
	}
	if _, err := f.svc.RetryOutbox(ctx, convID, row.ID); err == nil {
		t.Fatal("retry of a pending row must fail")
	}
	if _, err := f.svc.RetryOutbox(ctx, convID, "missing"); err == nil {
		t.Fatal("retry of a missing row must fail")
	}
}

// TestAckRunsOnce: the ack command runs once per Ack call with the channel env.
func TestAckRunsOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cwd := f.dir
	f.defs.channels["telegram"] = connectors.Channel{Ref: "id", Send: "true",
		Ack: `printf '%s' "$TREBI_CHANNEL_REF" >> ack.txt`}
	f.defs.cwds["telegram"] = cwd
	ctx := context.Background()

	if _, err := f.convs.EnsureForChannel(ctx, "ws1", "", "telegram", "chat1"); err != nil {
		t.Fatal(err)
	}
	item := dispatch.QueueItem{Connector: "telegram", WorkspaceID: "ws1", ConversationKey: "chat1"}
	if err := f.svc.Ack(ctx, item); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "ack.txt"))
	if err != nil {
		t.Fatalf("ack file: %v", err)
	}
	if string(data) != "chat1" {
		t.Fatalf("ack = %q", data)
	}
	// A connector with no ack command is a no-op.
	if err := f.svc.Ack(ctx, dispatch.QueueItem{Connector: "missing"}); err != nil {
		t.Fatal(err)
	}
}

// TestPulseTypingStartsAndStops: typing runs at the interval while the turn
// is live and stops when it ends.
func TestPulseTypingStartsAndStops(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cwd := f.dir
	f.defs.channels["telegram"] = connectors.Channel{Ref: "id", Send: "true",
		Typing:         `date +%s%N >> typing.txt`,
		TypingInterval: connectors.Duration(8 * time.Second),
		TypingMax:      connectors.Duration(15 * time.Minute),
	}
	f.defs.cwds["telegram"] = cwd
	ctx := context.Background()

	convID := f.conv("ws1")
	f.userMsg(convID, "telegram", "chat1", "hello")
	f.setSessionStatus(convID, "running")

	t0 := time.Now()
	if err := f.svc.PulseTyping(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if n := f.lines("typing.txt"); n != 1 {
		t.Fatalf("typing lines after start = %d, want 1", n)
	}
	// Inside the interval nothing more is sent.
	if err := f.svc.PulseTyping(ctx, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if n := f.lines("typing.txt"); n != 1 {
		t.Fatalf("typing lines inside interval = %d, want 1", n)
	}
	// After the interval one more pulse goes.
	if err := f.svc.PulseTyping(ctx, t0.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	if n := f.lines("typing.txt"); n != 2 {
		t.Fatalf("typing lines after interval = %d, want 2", n)
	}
	// The turn ends: the pulse stops and the state clears.
	f.setSessionStatus(convID, "idle")
	if err := f.svc.PulseTyping(ctx, t0.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if n := f.lines("typing.txt"); n != 2 {
		t.Fatalf("typing lines after stop = %d, want 2", n)
	}
	// A new turn starts the pulse again.
	f.setSessionStatus(convID, "running")
	if err := f.svc.PulseTyping(ctx, t0.Add(21*time.Second)); err != nil {
		t.Fatal(err)
	}
	if n := f.lines("typing.txt"); n != 3 {
		t.Fatalf("typing lines after restart = %d, want 3", n)
	}
}

// TestPointersPerWorkspace: the same connector ref in two workspaces gives
// two conversations.
func TestPointersPerWorkspace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	a, err := f.convs.EnsureForChannel(ctx, "ws1", "", "telegram", "chat1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.convs.EnsureForChannel(ctx, "ws2", "", "telegram", "chat1")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("both workspaces share conversation %s", a)
	}
	pa, err := f.convs.GetPointer(ctx, "ws1", "telegram", "chat1")
	if err != nil || pa.ConversationID != a {
		t.Fatalf("ws1 pointer %+v %v", pa, err)
	}
	pb, err := f.convs.GetPointer(ctx, "ws2", "telegram", "chat1")
	if err != nil || pb.ConversationID != b {
		t.Fatalf("ws2 pointer %+v %v", pb, err)
	}
}

func (f *fixture) onlyOutbox(ctx context.Context, convID string) channels.OutboxRow {
	f.t.Helper()
	rows, err := f.svc.ListOutbox(ctx, convID)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(rows) != 1 {
		f.t.Fatalf("outbox rows = %d, want 1", len(rows))
	}
	return rows[0]
}

func (f *fixture) setSessionStatus(convID, status string) {
	f.t.Helper()
	_, err := f.st.DB().Exec(`INSERT INTO assistant_sessions
		(conversation_id, workspace_id, harness, mode, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (conversation_id) DO UPDATE SET status=excluded.status`,
		convID, "ws1", "fake", "ask", status,
		time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) lines(name string) int {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		f.t.Fatal(err)
	}
	return strings.Count(strings.TrimSpace(string(data)), "\n") + 1
}
