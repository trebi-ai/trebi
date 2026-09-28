package channels_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/channels"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/outbox"
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
	ob    *outbox.Service
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
	ob := outbox.New(st)
	svc := channels.New(st, ob, defs, convs)
	ob.Register(outbox.KindChannel, svc, outbox.Policy{Base: 5 * time.Second, Max: 5 * time.Minute, Attempts: 5})
	return &fixture{t: t, dir: dir, st: st, convs: convs, defs: defs, ob: ob, svc: svc}
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
		(id, conversation_id, role, connector, room, text, run_id, turn_index, created_at)
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
	rows, err := f.list(ctx, runConv)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("run outbox rows = %d, want 1", len(rows))
	}
	if rows[0].Body != "the answer" || rows[0].Room != "chat1" || rows[0].Connector() != "telegram" {
		t.Fatalf("run row %+v", rows[0])
	}
	if rows[0].WorkspaceID != "ws1" || rows[0].RunID != "r1" {
		t.Fatalf("run row workspace/run %+v", rows[0])
	}

	// The assistant session has run_id NULL.
	sessionConv := f.conv("ws1")
	f.userMsg(sessionConv, "telegram", "chat2", "ping")
	f.assistantMsg(sessionConv, "", "pong")
	if err := f.svc.ReplyOnTurn(ctx, sessionConv, ""); err != nil {
		t.Fatal(err)
	}
	rows, err = f.list(ctx, sessionConv)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Body != "pong" || rows[0].RunID != "" {
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
	if rows, _ = f.list(ctx, plain); len(rows) != 0 {
		t.Fatalf("plain conversation got %+v", rows)
	}
}

// TestReplyOnTurnJoinsTextAfterUser: every text block of the turn after the
// last user message goes out in order. A turn with no text sends nothing and
// never repeats an older answer.
func TestReplyOnTurnJoinsTextAfterUser(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addChannel("telegram", "true")
	ctx := context.Background()

	convID := f.conv("ws1")
	f.userMsg(convID, "telegram", "chat1", "first question")
	f.assistantMsg(convID, "", "old answer")
	f.userMsg(convID, "telegram", "chat1", "second question")
	f.sessionMsg(convID, 2, "Let me check.")
	f.sessionMsg(convID, 2, "Final answer.")
	if err := f.svc.ReplyOnTurn(ctx, convID, ""); err != nil {
		t.Fatal(err)
	}
	row := f.onlyOutbox(ctx, convID)
	if row.Body != "Let me check.\n\nFinal answer." {
		t.Fatalf("reply text = %q", row.Body)
	}

	// A tool-only turn writes no text: no row, and no old answer.
	toolOnly := f.conv("ws1")
	f.userMsg(toolOnly, "telegram", "chat2", "first")
	f.assistantMsg(toolOnly, "", "answer one")
	f.userMsg(toolOnly, "telegram", "chat2", "run the tool")
	if err := f.svc.ReplyOnTurn(ctx, toolOnly, ""); err != nil {
		t.Fatal(err)
	}
	if rows, _ := f.list(ctx, toolOnly); len(rows) != 0 {
		t.Fatalf("tool-only turn got %+v", rows)
	}
}

// TestReplyOnTurnSkipsNoAddress: a user row with no channel ref, or with a
// trigger ref, gets no reply row.
func TestReplyOnTurnSkipsNoAddress(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addChannel("telegram", "true")
	ctx := context.Background()
	for _, ref := range []string{"", "trigger:telegram"} {
		convID := f.conv("ws1")
		f.userMsg(convID, "telegram", ref, "event")
		f.assistantMsg(convID, "", "reply")
		if err := f.svc.ReplyOnTurn(ctx, convID, ""); err != nil {
			t.Fatal(err)
		}
		if rows, _ := f.list(ctx, convID); len(rows) != 0 {
			t.Fatalf("ref %q got %+v", ref, rows)
		}
	}
}

// TestOutboxFrames: a new row and each backoff retry emit outbox.queued
// with the row id; a duplicate write emits nothing.
func TestOutboxFrames(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addChannel("telegram", "exit 1")
	ctx := context.Background()

	convID := f.conv("ws1")
	f.userMsg(convID, "telegram", "chat1", "hello")
	f.assistantMsg(convID, "r1", "reply")
	for range 2 {
		if err := f.svc.ReplyOnTurn(ctx, convID, "r1"); err != nil {
			t.Fatal(err)
		}
	}
	row := f.onlyOutbox(ctx, convID)
	if n := f.frames("outbox.queued", row.ID); n != 1 {
		t.Fatalf("queued frames after insert = %d, want 1", n)
	}
	if err := f.ob.SendDue(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := f.frames("outbox.queued", row.ID); n != 2 {
		t.Fatalf("queued frames after retry = %d, want 2", n)
	}
}

// TestSendDueStopsOnCancel: a cancelled context sends no row.
func TestSendDueStopsOnCancel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addChannel("telegram", "true")
	convID := f.conv("ws1")
	f.userMsg(convID, "telegram", "chat1", "hello")
	f.assistantMsg(convID, "r1", "reply")
	if err := f.svc.ReplyOnTurn(context.Background(), convID, "r1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.ob.SendDue(ctx, time.Now()); err == nil {
		t.Fatal("SendDue on a cancelled context must return the error")
	}
	if row := f.onlyOutbox(context.Background(), convID); row.Status != outbox.StatusPending || row.Attempts != 0 {
		t.Fatalf("row changed after cancel: %+v", row)
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
	f.addChannel("telegram", `cat > body.txt && printf '%s|%s|%s' "$TREBI_CHANNEL_REF" "$TREBI_CONVERSATION_ID" "$TREBI_MESSAGE_ID" > env.txt`)
	f.defs.cwds["telegram"] = cwd
	f.defs.envs["telegram"] = map[string]string{"EXTRA": "x"}
	ctx := context.Background()

	convID := f.conv("ws1")
	f.userMsg(convID, "telegram", "chat1", "hello")
	f.assistantMsg(convID, "r1", "the reply text")
	if err := f.svc.ReplyOnTurn(ctx, convID, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := f.ob.SendDue(ctx, time.Now()); err != nil {
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
	rows, _ := f.list(ctx, convID)
	if len(rows) != 1 || rows[0].Status != outbox.StatusSent || rows[0].SentAt == nil {
		t.Fatalf("sent row %+v", rows)
	}
	if want := "chat1|" + convID + "|" + rows[0].ID; string(env) != want {
		t.Fatalf("env = %q, want %q", env, want)
	}
}

// TestSendDueRetryBackoff: a failed send backs off 5s << (attempts-1).
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
	if err := f.ob.SendDue(ctx, t0); err != nil {
		t.Fatal(err)
	}
	row := f.onlyOutbox(ctx, convID)
	if row.Attempts != 1 || row.Status != outbox.StatusPending {
		t.Fatalf("after first failure %+v", row)
	}
	if want := t0.Add(5 * time.Second); row.NextAttemptAt == nil || !row.NextAttemptAt.Equal(want.Truncate(time.Millisecond)) {
		t.Fatalf("next = %v, want %v", row.NextAttemptAt, want)
	}
	// Before the due time the row stays.
	if err := f.ob.SendDue(ctx, t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if row = f.onlyOutbox(ctx, convID); row.Attempts != 1 {
		t.Fatalf("not-due row advanced: %+v", row)
	}
	// At the due time the second attempt runs.
	if err := f.ob.SendDue(ctx, t0.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	row = f.onlyOutbox(ctx, convID)
	if row.Attempts != 2 {
		t.Fatalf("second attempt %+v", row)
	}
	if want := t0.Add(15 * time.Second); row.NextAttemptAt == nil || !row.NextAttemptAt.Equal(want.Truncate(time.Millisecond)) {
		t.Fatalf("second next = %v, want %v", row.NextAttemptAt, want)
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
		if err := f.ob.SendDue(ctx, time.Now().Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	row := f.onlyOutbox(ctx, convID)
	if row.Status != outbox.StatusFailed || row.Attempts != 5 {
		t.Fatalf("failed row %+v", row)
	}

	frames, err := f.st.ListBusWith(ctx, store.ListBusOpts{Topics: []string{"outbox.failed"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("outbox.failed frames = %d, want 1", len(frames))
	}
	var attrs map[string]string
	if err := json.Unmarshal(frames[0].Attrs, &attrs); err != nil {
		t.Fatal(err)
	}
	if attrs["connector"] != "telegram" || attrs["conversation_id"] != convID || attrs["row_id"] != row.ID {
		t.Fatalf("attrs %+v", attrs)
	}

	alertSvc := alerts.New(f.st, alerts.Options{})
	alertSvc.HandleEvent(ctx, frames[0])
	if n := f.openItems("outbox/" + row.ID); n != 1 {
		t.Fatalf("inbox items = %d, want 1", n)
	}

	// A manual retry that sends resolves the item.
	f.addChannel("telegram", "true")
	if _, err := f.ob.Retry(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.ob.SendDue(ctx, time.Now().Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	sent, err := f.st.ListBusWith(ctx, store.ListBusOpts{Topics: []string{"outbox.sent"}, Limit: 10})
	if err != nil || len(sent) != 1 {
		t.Fatalf("outbox.sent frames = %d, %v", len(sent), err)
	}
	alertSvc.HandleEvent(ctx, sent[0])
	if n := f.openItems("outbox/" + row.ID); n != 0 {
		t.Fatalf("open inbox items after send = %d, want 0", n)
	}
}

func (f *fixture) openItems(subject string) int {
	f.t.Helper()
	var n int
	if err := f.st.DB().QueryRow(`SELECT COUNT(*) FROM inbox_items
		WHERE alert_kind='send_failed' AND subject=? AND status!='resolved'`, subject).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
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
		if err := f.ob.SendDue(ctx, time.Now().Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	row := f.onlyOutbox(ctx, convID)
	retried, err := f.ob.Retry(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != outbox.StatusPending || retried.Attempts != 0 || retried.NextAttemptAt != nil {
		t.Fatalf("retried %+v", retried)
	}
	if _, err := f.ob.Retry(ctx, row.ID); !errors.Is(err, outbox.ErrConflict) {
		t.Fatal("retry of a pending row must fail")
	}
	if _, err := f.ob.Retry(ctx, "missing"); !errors.Is(err, outbox.ErrNotFound) {
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

	if _, err := f.convs.EnsureForChannel(ctx, "ws1", "", conversations.Address{Connector: "telegram", Room: "chat1"}); err != nil {
		t.Fatal(err)
	}
	// The key is a thread id; the ack goes to the event's channel ref.
	evID := f.event("telegram", "chat1")
	item := dispatch.QueueItem{Connector: "telegram", WorkspaceID: "ws1", ConversationKey: "thread9", EventID: evID}
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

// busyTurns is a LiveTurns fake.
type busyTurns struct {
	mu   sync.Mutex
	busy map[string]bool
}

func (b *busyTurns) Busy(_ context.Context, convID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.busy[convID]
}

func (b *busyTurns) set(convID string, v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.busy[convID] = v
}

// TestPulseTypingStopsAtMax: typing stops at typing_max and stays stopped
// while the turn is busy. A conversation that waits is not busy, and the
// next busy turn starts typing again.
func TestPulseTypingStopsAtMax(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	turns := &busyTurns{busy: map[string]bool{}}
	f.svc.WithLiveTurns(turns)
	f.defs.channels["telegram"] = connectors.Channel{Ref: "id", Send: "true",
		Typing:         `date +%s%N >> typing.txt`,
		TypingInterval: connectors.Duration(8 * time.Second),
		TypingMax:      connectors.Duration(20 * time.Second),
	}
	f.defs.cwds["telegram"] = f.dir
	ctx := context.Background()

	convID := f.conv("ws1")
	f.userMsg(convID, "telegram", "chat1", "hello")
	f.setSessionStatus(convID, "running")

	// A live session that is not busy gets no typing.
	t0 := time.Now()
	if err := f.svc.PulseTyping(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if n := f.lines("typing.txt"); n != 0 {
		t.Fatalf("typing lines while not busy = %d, want 0", n)
	}
	turns.set(convID, true)
	steps := []struct {
		at   time.Duration
		want int
	}{
		{0, 1}, {10 * time.Second, 2}, {20 * time.Second, 2}, {30 * time.Second, 2}, {60 * time.Second, 2},
	}
	for _, st := range steps {
		if err := f.svc.PulseTyping(ctx, t0.Add(st.at)); err != nil {
			t.Fatal(err)
		}
		if n := f.lines("typing.txt"); n != st.want {
			t.Fatalf("at %s typing lines = %d, want %d", st.at, n, st.want)
		}
	}
	// The turn waits, then a new turn starts: typing runs again.
	turns.set(convID, false)
	if err := f.svc.PulseTyping(ctx, t0.Add(61*time.Second)); err != nil {
		t.Fatal(err)
	}
	turns.set(convID, true)
	if err := f.svc.PulseTyping(ctx, t0.Add(62*time.Second)); err != nil {
		t.Fatal(err)
	}
	if n := f.lines("typing.txt"); n != 3 {
		t.Fatalf("typing lines after a new turn = %d, want 3", n)
	}
}

// TestPointersPerWorkspace: the same connector ref in two workspaces gives
// two conversations.
func TestPointersPerWorkspace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	a, err := f.convs.EnsureForChannel(ctx, "ws1", "", conversations.Address{Connector: "telegram", Room: "chat1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.convs.EnsureForChannel(ctx, "ws2", "", conversations.Address{Connector: "telegram", Room: "chat1"})
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("both workspaces share conversation %s", a)
	}
	pa, err := f.convs.GetPointer(ctx, "ws1", conversations.Address{Connector: "telegram", Room: "chat1"})
	if err != nil || pa.ConversationID != a {
		t.Fatalf("ws1 pointer %+v %v", pa, err)
	}
	pb, err := f.convs.GetPointer(ctx, "ws2", conversations.Address{Connector: "telegram", Room: "chat1"})
	if err != nil || pb.ConversationID != b {
		t.Fatalf("ws2 pointer %+v %v", pb, err)
	}
}

// event inserts one events row with a channel ref and returns its id.
func (f *fixture) event(connector, channelRef string) string {
	f.t.Helper()
	id := ulid.Make().String()
	if _, err := f.st.DB().Exec(`INSERT INTO events
		(id, connector, instance, received_at, summary, raw_json, trace_id, room)
		VALUES (?,?,?,?,?,?,?,?)`,
		id, connector, connector, time.Now().UTC().Format(time.RFC3339Nano), "", "{}", "tr", channelRef); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// sessionMsg inserts one assistant session row (run_id NULL) of one turn.
func (f *fixture) sessionMsg(convID string, turn int, text string) {
	f.t.Helper()
	if _, err := f.st.DB().Exec(`INSERT INTO conversation_messages
		(id, conversation_id, role, text, turn_index, created_at) VALUES (?,?,?,?,?,?)`,
		ulid.Make().String(), convID, "assistant", text, turn,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		f.t.Fatal(err)
	}
}

// frames counts the bus frames of one topic for one outbox row.
func (f *fixture) frames(topic, rowID string) int {
	f.t.Helper()
	got, err := f.st.ListBusWith(context.Background(), store.ListBusOpts{Topics: []string{topic}, Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	n := 0
	for _, fr := range got {
		var attrs map[string]string
		if err := json.Unmarshal(fr.Attrs, &attrs); err != nil {
			f.t.Fatal(err)
		}
		if attrs["row_id"] == rowID {
			n++
		}
	}
	return n
}

func (f *fixture) list(ctx context.Context, convID string) ([]outbox.Row, error) {
	rows, _, err := f.ob.List(ctx, outbox.Filter{ConversationID: convID})
	return rows, err
}

func (f *fixture) onlyOutbox(ctx context.Context, convID string) outbox.Row {
	f.t.Helper()
	rows, err := f.list(ctx, convID)
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
