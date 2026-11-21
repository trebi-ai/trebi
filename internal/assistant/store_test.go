package assistant_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestStoreRecordAndEventsRoundTrip(t *testing.T) {
	base := t.TempDir()
	s, err := store.OpenPath(filepath.Join(base, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if _, err := s.DB().Exec(`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at)
		VALUES ('c1', 'ws1', 'k', '', '2026-09-25T00:00:00Z', '2026-09-25T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at)
		VALUES ('c2', 'ws1', 'k2', '', '2026-09-25T00:00:00Z', '2026-09-25T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	ast := storeAssistant(s)
	if err := ast.Create(ctx, assistant.Record{
		ConversationID: "c1", WorkspaceID: "ws1", Harness: "fake", Mode: "auto_edit", Status: assistant.StatusStarting,
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := ast.Get(ctx, "c1")
	if err != nil || rec.Status != assistant.StatusStarting {
		t.Fatalf("get %+v %v", rec, err)
	}
	rec, err = ast.Update(ctx, "c1", func(r *assistant.Record) { r.Status = assistant.StatusIdle; r.TokensIn = 5 })
	if err != nil || rec.Status != assistant.StatusIdle || rec.TokensIn != 5 {
		t.Fatalf("update %+v %v", rec, err)
	}
	if _, err := ast.Update(ctx, "nope", func(r *assistant.Record) {}); err == nil {
		t.Fatal("update on missing row must fail")
	}

	seq, err := ast.AppendEvent(ctx, "c1", assistant.Event{Type: "assistant", Text: "hello"})
	if err != nil || seq != 1 {
		t.Fatalf("append %d %v", seq, err)
	}
	seq, err = ast.AppendEvent(ctx, "c1", assistant.Event{Type: "result"})
	if err != nil || seq != 2 {
		t.Fatalf("append2 %d %v", seq, err)
	}
	events, err := ast.Events(ctx, "c1", 0, 10)
	if err != nil || len(events) != 2 || events[0].Text != "hello" {
		t.Fatalf("events %+v %v", events, err)
	}
	tail, err := ast.Events(ctx, "c1", 1, 10)
	if err != nil || len(tail) != 1 || tail[0].Type != "result" {
		t.Fatalf("after-seq events %+v %v", tail, err)
	}

	if err := ast.Create(ctx, assistant.Record{
		ConversationID: "c2", WorkspaceID: "ws1", Harness: "native", Mode: "auto_edit",
		Status: assistant.StatusRunning, TokensIn: 10, TokensOut: 3,
	}); err != nil {
		t.Fatal(err)
	}
	today, err := ast.TokensToday(ctx, "native")
	if err != nil || today != 13 {
		t.Fatalf("tokens today %d %v", today, err)
	}
	// A crash leaves c2 (no vendor session) and c3 (a vendor session) in a turn.
	if _, err := s.DB().Exec(`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at)
		VALUES ('c3', 'ws1', 'k3', '', '2026-09-25T00:00:00Z', '2026-09-25T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := ast.Create(ctx, assistant.Record{
		ConversationID: "c3", WorkspaceID: "ws1", Harness: "claude", Mode: "auto_edit",
		Status: assistant.StatusWaiting, VendorID: "v3", ActorTokenHash: "h3",
	}); err != nil {
		t.Fatal(err)
	}
	paused, closed, err := ast.PauseStale(ctx)
	if err != nil || paused != 1 || closed != 1 {
		t.Fatalf("pause stale %d/%d %v", paused, closed, err)
	}
	if rec, _ := ast.Get(ctx, "c2"); rec.Status != assistant.StatusClosed {
		t.Fatalf("stale status %s", rec.Status)
	}
	if rec, _ := ast.Get(ctx, "c3"); rec.Status != assistant.StatusPaused || rec.ActorTokenHash != "" {
		t.Fatalf("paused row %+v", rec)
	}
	if list, err := ast.Paused(ctx); err != nil || len(list) != 1 || list[0].ConversationID != "c3" {
		t.Fatalf("paused list %+v %v", list, err)
	}
	if err := ast.DeleteSession(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ast.Get(ctx, "c1"); err == nil {
		t.Fatal("session must be gone")
	}
}

func storeAssistant(s *store.Store) *assistant.Store { return assistant.NewStore(s) }

func TestStoreUpdateWakesStream(t *testing.T) {
	s, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if _, err := s.DB().Exec(`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at)
		VALUES ('c1', 'ws1', 'k', '', '2026-09-25T00:00:00Z', '2026-09-25T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	ast := storeAssistant(s)
	if err := ast.Create(ctx, assistant.Record{ConversationID: "c1", WorkspaceID: "ws1", Harness: "fake", Mode: "auto_edit", Status: assistant.StatusStarting}); err != nil {
		t.Fatal(err)
	}
	wake := s.BusWake()
	if _, err := ast.Update(ctx, "c1", func(r *assistant.Record) { r.Status = assistant.StatusIdle }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("Update did not wake the bus")
	}
}
