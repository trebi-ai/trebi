package conversations_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestConversationCRUDAndMessages(t *testing.T) {
	t.Parallel()
	svc := testSvc(t)
	ctx := context.Background()

	c, err := svc.Create(ctx, conversations.CreateOpts{
		Workspace: "ws1", OriginChannel: "cli", Title: "",
		Activate: &conversations.Address{Connector: "cli", Room: "ws1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Revision != 1 || c.Key == "" || c.OriginChannel != "cli" {
		t.Fatalf("%+v", c)
	}
	if len(c.Channels) != 1 || c.Channels[0].Connector != "cli" {
		t.Fatalf("channels %+v", c.Channels)
	}

	ptr, err := svc.GetPointer(ctx, "ws1", conversations.Address{Connector: "cli", Room: "ws1"})
	if err != nil || ptr.ConversationID != c.ID {
		t.Fatalf("pointer %+v %v", ptr, err)
	}

	list, err := svc.List(ctx, conversations.ListOpts{Workspace: "ws1", Channel: "cli", Limit: 10})
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}

	msgs, err := svc.ListMessages(ctx, c.ID, conversations.MessageListOpts{Limit: 10})
	if err != nil || len(msgs) != 0 {
		t.Fatalf("empty messages %v %v", msgs, err)
	}

	title := "hello"
	patched, err := svc.Patch(ctx, c.ID, conversations.PatchOpts{Title: &title, Revision: c.Revision})
	if err != nil || patched.Title != "hello" || patched.Revision != 2 {
		t.Fatalf("patch %+v %v", patched, err)
	}
	_, err = svc.Patch(ctx, c.ID, conversations.PatchOpts{Title: &title, Revision: 1})
	if !errors.Is(err, conversations.ErrConflict) {
		t.Fatalf("stale revision %v", err)
	}
}

func TestMessagePaginationAndIdempotency(t *testing.T) {
	t.Parallel()
	svc := testSvc(t)
	ctx := context.Background()
	c, err := svc.Create(ctx, conversations.CreateOpts{Workspace: "ws", OriginChannel: "ui"})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		res, err := svc.Send(ctx, c.ID, conversations.SendOpts{
			Text: "m", Address: conversations.Address{Connector: "ui", Room: "ws"},
			IdempotencyKey: ulid.Make().String(),
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.Message.ID)
	}
	page, err := svc.ListMessages(ctx, c.ID, conversations.MessageListOpts{Limit: 2})
	if err != nil || len(page) != 2 {
		t.Fatalf("page %v %v", page, err)
	}
	if page[0].ID != ids[0] || page[1].ID != ids[1] {
		t.Fatalf("ascending got %s %s want %s %s", page[0].ID, page[1].ID, ids[0], ids[1])
	}
	page2, err := svc.ListMessages(ctx, c.ID, conversations.MessageListOpts{Limit: 2, Cursor: page[1].ID})
	if err != nil || len(page2) != 1 || page2[0].ID != ids[2] {
		t.Fatalf("page2 %v %v", page2, err)
	}

	dup, err := svc.Send(ctx, c.ID, conversations.SendOpts{
		Text: "ignored", Address: conversations.Address{Connector: "ui", Room: "ws"}, IdempotencyKey: "same",
	})
	if err != nil {
		t.Fatal(err)
	}
	dup2, err := svc.Send(ctx, c.ID, conversations.SendOpts{
		Text: "ignored", Address: conversations.Address{Connector: "ui", Room: "ws"}, IdempotencyKey: "same",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dup.Message.ID != dup2.Message.ID {
		t.Fatalf("idempotency %s vs %s", dup.Message.ID, dup2.Message.ID)
	}
	all, _ := svc.ListMessages(ctx, c.ID, conversations.MessageListOpts{Limit: 50})
	if len(all) != 4 { // 3 + 1 unique idempotent
		t.Fatalf("count %d", len(all))
	}
}

func TestWatcherIdempotencyAndAutoTitle(t *testing.T) {
	t.Parallel()
	svc := testSvc(t)
	ctx := context.Background()
	c, err := svc.Create(ctx, conversations.CreateOpts{Workspace: "ws", OriginChannel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Send(ctx, c.ID, conversations.SendOpts{Text: "Please summarize the backlog", Address: conversations.Address{Connector: "cli", Room: "ws"}})
	if err != nil {
		t.Fatal(err)
	}
	turns := []conversations.IncomingTurn{
		{Role: "user", Text: "skip"},
		{Role: "assistant", Text: "Sure, here is a summary."},
	}
	n, err := svc.IngestAssistantTurns(ctx, "run1", c.ID, turns)
	if err != nil || n != 1 {
		t.Fatalf("ingest %d %v", n, err)
	}
	n, err = svc.IngestAssistantTurns(ctx, "run1", c.ID, turns)
	if err != nil || n != 0 {
		t.Fatalf("idempotent ingest %d %v", n, err)
	}
	got, err := svc.Get(ctx, c.ID)
	if err != nil || got.Title != "Please summarize the backlog" {
		t.Fatalf("auto-title %+v %v", got, err)
	}
}

func TestPointerSwitchAnd404(t *testing.T) {
	t.Parallel()
	svc := testSvc(t)
	ctx := context.Background()
	a, err := svc.Create(ctx, conversations.CreateOpts{Workspace: "ws", OriginChannel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Create(ctx, conversations.CreateOpts{Workspace: "ws", OriginChannel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutPointer(ctx, "ws", conversations.Address{Connector: "cli", Room: "ws"}, a.ID); err != nil {
		t.Fatal(err)
	}
	msgsA, _ := svc.ListMessages(ctx, a.ID, conversations.MessageListOpts{Limit: 10})
	if _, err := svc.PutPointer(ctx, "ws", conversations.Address{Connector: "cli", Room: "ws"}, a.ID); err != nil {
		t.Fatal(err)
	}
	msgsA2, _ := svc.ListMessages(ctx, a.ID, conversations.MessageListOpts{Limit: 10})
	if len(msgsA2) != len(msgsA) {
		t.Fatalf("repeat PutPointer added recap rows: %d -> %d", len(msgsA), len(msgsA2))
	}
	if _, err := svc.PutPointer(ctx, "ws", conversations.Address{Connector: "cli", Room: "ws"}, b.ID); err != nil {
		t.Fatal(err)
	}
	ptr, err := svc.GetPointer(ctx, "ws", conversations.Address{Connector: "cli", Room: "ws"})
	if err != nil || ptr.ConversationID != b.ID {
		t.Fatalf("%+v %v", ptr, err)
	}
	msgs, _ := svc.ListMessages(ctx, b.ID, conversations.MessageListOpts{Limit: 10})
	if len(msgs) == 0 || msgs[0].Role != conversations.RoleSystem {
		t.Fatalf("recap row %v", msgs)
	}
	_, err = svc.GetPointer(ctx, "ws", conversations.Address{Connector: "telegram", Room: "missing"})
	if !errors.Is(err, conversations.ErrNotFound) {
		t.Fatalf("missing pointer %v", err)
	}
	_, err = svc.Get(ctx, ulid.Make().String())
	if !errors.Is(err, conversations.ErrNotFound) {
		t.Fatalf("missing conv %v", err)
	}
}

func TestAskCreatesAndReusesTheChannelConversation(t *testing.T) {
	t.Parallel()
	svc := testSvc(t)
	ctx := context.Background()

	first, err := svc.Ask(ctx, conversations.AskOpts{Workspace: "ws1", Text: "status?"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ConversationID == "" || first.Message.Text != "status?" {
		t.Fatalf("%+v", first)
	}
	ptr, err := svc.GetPointer(ctx, "ws1", conversations.Address{Connector: "cli", Room: "ws1"})
	if err != nil || ptr.ConversationID != first.ConversationID {
		t.Fatalf("pointer %+v %v", ptr, err)
	}

	second, err := svc.Ask(ctx, conversations.AskOpts{Workspace: "ws1", Text: "again"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ConversationID != first.ConversationID {
		t.Fatalf("ask made a second conversation: %s then %s", first.ConversationID, second.ConversationID)
	}

	other, err := svc.Ask(ctx, conversations.AskOpts{
		Workspace: "ws1", Text: "hi", Address: conversations.Address{Connector: "ui", Room: "panel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if other.ConversationID == first.ConversationID {
		t.Fatal("a different channel must get its own conversation")
	}

	if _, err := svc.Ask(ctx, conversations.AskOpts{Text: "no workspace"}); !errors.Is(err, conversations.ErrValidation) {
		t.Fatalf("missing workspace: %v", err)
	}
}

func testSvc(t *testing.T) *conversations.Service {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := runs.New(st, nil, nil)
	q := dispatch.NewQueue(st)
	svc := conversations.New(st).WithRuns(r).WithQueue(q)
	// Sends with no operator route to the assistant. The stub stores the
	// user row and reports it, which is all the send path needs here.
	svc.WithAssistant(stubPrompter{svc: svc})
	return svc
}

// stubPrompter satisfies the assistant seam: it stores the user row and
// reports it, so send-path tests exercise routing without a session.
type stubPrompter struct {
	svc *conversations.Service
}

func (p stubPrompter) Prompt(ctx context.Context, convID string, in wire.PromptInput) (wire.PromptResult, error) {
	msg, _, err := p.svc.AppendUser(ctx, convID, conversations.UserTurn{
		Text: in.Text, Address: conversations.Address{Connector: in.Connector, Room: in.Room},
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return wire.PromptResult{}, err
	}
	return wire.PromptResult{Message: wireFromConv(msg)}, nil
}

func (p stubPrompter) Ask(ctx context.Context, convID string, in wire.PromptInput, _ time.Duration) (wire.AskResult, error) {
	return p.ask(ctx, convID, in)
}

func (p stubPrompter) ask(ctx context.Context, convID string, in wire.PromptInput) (wire.AskResult, error) {
	msg, _, err := p.svc.AppendUser(ctx, convID, conversations.UserTurn{
		Text: in.Text, Address: conversations.Address{Connector: in.Connector, Room: in.Room},
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return wire.AskResult{}, err
	}
	return wire.AskResult{Message: wireFromConv(msg)}, nil
}

func wireFromConv(m conversations.Message) wire.Message {
	return wire.Message{ID: m.ID, ConversationID: m.ConversationID, Role: m.Role,
		Connector: m.Connector, Room: m.Room, Text: m.Text, CreatedAt: m.CreatedAt}
}

// TestAutoTitleOnFirstUserMessage pins plan 2026-09-26 E: the title comes
// from the first user row as soon as it exists, before any assistant row.
func TestAutoTitleOnFirstUserMessage(t *testing.T) {
	svc := testSvc(t)
	ctx := context.Background()
	c, err := svc.Create(ctx, conversations.CreateOpts{Workspace: "ws", OriginChannel: "cli"})
	if err != nil {
		t.Fatal(err)
	}

	// AppendUser titles the conversation with no assistant row present.
	msg, _, err := svc.AppendUser(ctx, c.ID, conversations.UserTurn{Text: "How do I reset the daemon cache?"})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != conversations.RoleUser {
		t.Fatalf("role = %v", msg.Role)
	}
	got, err := svc.Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "How do I reset the daemon cache?" {
		t.Fatalf("title after AppendUser = %q", got.Title)
	}

	// A second user message does not change the title.
	if _, _, err := svc.AppendUser(ctx, c.ID, conversations.UserTurn{Text: "Second question"}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "How do I reset the daemon cache?" {
		t.Fatalf("title moved on second message = %q", got.Title)
	}

	// A title the user set is never overwritten.
	c2, err := svc.Create(ctx, conversations.CreateOpts{Workspace: "ws", OriginChannel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	named := "Ops handover"
	if _, err := svc.Patch(ctx, c2.ID, conversations.PatchOpts{Title: &named}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AppendUser(ctx, c2.ID, conversations.UserTurn{Text: "Different question"}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.Get(ctx, c2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != named {
		t.Fatalf("user title overwritten = %q", got.Title)
	}

	// A 70-rune first line truncates to 60 runes plus an ellipsis.
	c3, err := svc.Create(ctx, conversations.CreateOpts{Workspace: "ws", OriginChannel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 70)
	if _, _, err := svc.AppendUser(ctx, c3.ID, conversations.UserTurn{Text: long}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.Get(ctx, c3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != long[:60]+"\u2026" {
		t.Fatalf("long title = %q (%d runes)", got.Title, len([]rune(got.Title)))
	}

	// A multi-line message titles from the first line only.
	c4, err := svc.Create(ctx, conversations.CreateOpts{Workspace: "ws", OriginChannel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AppendUser(ctx, c4.ID, conversations.UserTurn{Text: "First line\n\nbody"}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.Get(ctx, c4.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "First line" {
		t.Fatalf("multiline title = %q", got.Title)
	}
}

// TestReplyTextJoinsTurnBlocks: the session text blocks after the user row
// join in id order; the last row is the key. No text after the user row is
// not ok, so an older answer never goes out again.
func TestReplyTextJoinsTurnBlocks(t *testing.T) {
	t.Parallel()
	svc := testSvc(t)
	ctx := context.Background()
	c, err := svc.Create(ctx, conversations.CreateOpts{Workspace: "ws", OriginChannel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := svc.AppendUser(ctx, c.ID, conversations.UserTurn{Text: "q1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AppendAssistant(ctx, c.ID, 1, "Let me check."); err != nil {
		t.Fatal(err)
	}
	last, err := svc.AppendAssistant(ctx, c.ID, 1, "Final answer.")
	if err != nil {
		t.Fatal(err)
	}
	msg, text, ok, err := svc.ReplyText(ctx, c.ID, "", first.ID)
	if err != nil || !ok {
		t.Fatalf("ReplyText ok=%v err=%v", ok, err)
	}
	if text != "Let me check.\n\nFinal answer." || msg.ID != last.ID {
		t.Fatalf("text %q msg %s want %s", text, msg.ID, last.ID)
	}
	second, _, err := svc.AppendUser(ctx, c.ID, conversations.UserTurn{Text: "q2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := svc.ReplyText(ctx, c.ID, "", second.ID); err != nil || ok {
		t.Fatalf("tool-only turn ok=%v err=%v", ok, err)
	}
}

// TestBindRoom proves a bind keeps a pointer of the same operator and moves
// the room to a new conversation for another operator.
func TestBindRoom(t *testing.T) {
	t.Parallel()
	svc := testSvc(t)
	ctx := context.Background()
	room := conversations.Address{Connector: "whatsapp", Room: "r1"}
	if _, err := svc.BindRoom(ctx, "ws1", "", conversations.Address{Connector: "whatsapp"}); !errors.Is(err, conversations.ErrValidation) {
		t.Fatalf("no room: %v", err)
	}
	p1, err := svc.BindRoom(ctx, "ws1", "", room)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := svc.BindRoom(ctx, "ws1", "", room)
	if err != nil || p2.ConversationID != p1.ConversationID {
		t.Fatalf("second bind %+v %v", p2, err)
	}
	p3, err := svc.BindRoom(ctx, "ws1", "agt_0000000001", room)
	if err != nil || p3.ConversationID == p1.ConversationID {
		t.Fatalf("agent bind %+v %v", p3, err)
	}
	conv, err := svc.Get(ctx, p3.ConversationID)
	if err != nil || conv.OperatorID != "agt_0000000001" || conv.OriginChannel != "whatsapp" {
		t.Fatalf("conversation %+v %v", conv, err)
	}
}

// TestRoomBindings proves the bindings of one instance carry the operator
// of the conversation, with "assistant" for the empty operator.
func TestRoomBindings(t *testing.T) {
	t.Parallel()
	svc := testSvc(t)
	ctx := context.Background()
	if _, err := svc.BindRoom(ctx, "ws1", "", conversations.Address{Connector: "whatsapp", Instance: "con_wa", Room: "r1"}); err != nil {
		t.Fatal(err)
	}
	p2, err := svc.BindRoom(ctx, "ws1", "agt_0000000002", conversations.Address{Connector: "whatsapp", Instance: "con_wa", Room: "r2", Thread: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BindRoom(ctx, "ws1", "", conversations.Address{Connector: "whatsapp", Instance: "con_other", Room: "r1"}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.RoomBindings(ctx, "con_wa")
	if err != nil || len(got) != 2 {
		t.Fatalf("bindings %+v %v", got, err)
	}
	if got[0].OperatorName != "assistant" || got[0].Room != "r1" || got[0].Workspace != "ws1" {
		t.Fatalf("first %+v", got[0])
	}
	want := conversations.RoomBinding{Workspace: "ws1", OperatorID: "agt_0000000002", Room: "r2", Thread: "t1", ConversationID: p2.ConversationID}
	if got[1] != want {
		t.Fatalf("second %+v", got[1])
	}
	if none, err := svc.RoomBindings(ctx, "con_none"); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("none %#v %v", none, err)
	}
}
