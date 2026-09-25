package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/agents"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// ControlPlane.Start runs the assistant backfill beside the email sync
// (2026-09-24 plan K.3): a legacy row with assistant_seeded = 0 and no
// TREBI.md gets the template once.
func TestStartSeedsAssistantOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	os.MkdirAll(filepath.Join(dir, "legacy"), 0o755)
	svc := workspaces.New(st)
	ws, err := svc.Add(ctx, filepath.Join(dir, "legacy"), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	// The add already seeded and flagged. Reset both to simulate a
	// pre-v23 workspace whose assistant was deleted.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE workspaces SET assistant_seeded = 0 WHERE id = ?`, ws.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(ws.Path, "TREBI.md")); err != nil {
		t.Fatal(err)
	}

	cp := daemon.New(st, daemon.Options{Shadow: true})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	path := filepath.Join(ws.Path, "TREBI.md")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := os.ReadFile(path); err == nil {
			if string(got) != agents.AssistantTemplate {
				t.Fatalf("seeded wrong content: %q", string(got))
			}
			// Flag is set: a later manual delete stays deleted.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("deleted assistant rewritten (stat err %v)", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("backfill never wrote TREBI.md")
}
