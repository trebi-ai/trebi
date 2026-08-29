package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestTurnWatchIngestsFakeTranscript(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Setenv("TREBI_FAKE_HARNESS_CMD", `mkdir -p "$(dirname "$TREBI_TRANSCRIPT_PATH")" && printf '%s\n' '{"type":"assistant","timestamp":"2026-08-28T12:00:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Hello from fake"}]}}' >> "$TREBI_TRANSCRIPT_PATH" && echo "$TREBI_RUN_COMPLETE"`)
	t.Cleanup(func() { home.SetBase("") })

	wsPath := filepath.Join(dir, "ws")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "TREBI.md"), []byte("---\nharness: claude\n---\n\nYou are the assistant.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := daemon.New(st, daemon.Options{
		QueueInterval:     50 * time.Millisecond,
		TurnWatchInterval: 50 * time.Millisecond,
		AlertsInterval:    time.Hour,
	})
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "watch-ws")
	if err != nil {
		t.Fatal(err)
	}

	conv, err := cp.Conversations.Create(ctx, conversations.CreateOpts{
		Workspace: ws.ID, OriginChannel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cp.Conversations.Send(ctx, conv.ID, conversations.SendOpts{
		Text: "hi", ChannelType: "cli", ChannelRef: ws.ID,
	}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := cp.Conversations.ListMessages(ctx, conv.ID, conversations.MessageListOpts{Limit: 20})
		if err == nil {
			for _, m := range msgs {
				if m.Role == conversations.RoleAssistant && m.Text == "Hello from fake" {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	msgs, _ := cp.Conversations.ListMessages(ctx, conv.ID, conversations.MessageListOpts{Limit: 20})
	t.Fatalf("no assistant reply: %+v", msgs)
}
