package conversations_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/oklog/ulid/v2"
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
		Activate: &conversations.Activate{ChannelType: "cli", ChannelRef: "ws1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Revision != 1 || c.Key == "" || c.OriginChannel != "cli" {
		t.Fatalf("%+v", c)
	}
	if len(c.Channels) != 1 || c.Channels[0].ChannelType != "cli" {
		t.Fatalf("channels %+v", c.Channels)
	}

	ptr, err := svc.GetPointer(ctx, "cli", "ws1")
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
			Text: "m", ChannelType: "ui", ChannelRef: "ws",
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
		Text: "ignored", ChannelType: "ui", ChannelRef: "ws", IdempotencyKey: "same",
	})
	if err != nil {
		t.Fatal(err)
	}
	dup2, err := svc.Send(ctx, c.ID, conversations.SendOpts{
		Text: "ignored", ChannelType: "ui", ChannelRef: "ws", IdempotencyKey: "same",
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
	_, err = svc.Send(ctx, c.ID, conversations.SendOpts{Text: "Please summarize the backlog", ChannelType: "cli", ChannelRef: "ws"})
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
	if _, err := svc.PutPointer(ctx, "cli", "ws", a.ID); err != nil {
		t.Fatal(err)
	}
	msgsA, _ := svc.ListMessages(ctx, a.ID, conversations.MessageListOpts{Limit: 10})
	if _, err := svc.PutPointer(ctx, "cli", "ws", a.ID); err != nil {
		t.Fatal(err)
	}
	msgsA2, _ := svc.ListMessages(ctx, a.ID, conversations.MessageListOpts{Limit: 10})
	if len(msgsA2) != len(msgsA) {
		t.Fatalf("repeat PutPointer added recap rows: %d -> %d", len(msgsA), len(msgsA2))
	}
	if _, err := svc.PutPointer(ctx, "cli", "ws", b.ID); err != nil {
		t.Fatal(err)
	}
	ptr, err := svc.GetPointer(ctx, "cli", "ws")
	if err != nil || ptr.ConversationID != b.ID {
		t.Fatalf("%+v %v", ptr, err)
	}
	msgs, _ := svc.ListMessages(ctx, b.ID, conversations.MessageListOpts{Limit: 10})
	if len(msgs) == 0 || msgs[0].Role != conversations.RoleSystem {
		t.Fatalf("recap row %v", msgs)
	}
	_, err = svc.GetPointer(ctx, "telegram", "missing")
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
	ptr, err := svc.GetPointer(ctx, "cli", "ws1")
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
		Workspace: "ws1", Text: "hi", ChannelType: "ui", ChannelRef: "panel",
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
	r := runs.New(st, nil)
	q := dispatch.NewQueue(st)
	return conversations.New(st).WithRuns(r).WithQueue(q)
}
