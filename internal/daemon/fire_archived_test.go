package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/schedule"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestFireOneSkipsArchivedQuietly: a schedule fire of an archived job starts
// no run and raises no alert.
func TestFireOneSkipsArchivedQuietly(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })
	wsPath := filepath.Join(dir, "ws")
	files := map[string]string{
		"agents/a/AGENT.md": "---\ndescription: A.\n---\n\nA.\n",
		"agents/a/x/JOB.md": "---\nharness: fake\narchived_at: 2026-11-01T10:00:00Z\n---\n\nX.\n",
	}
	for rel, body := range files {
		p := filepath.Join(wsPath, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cp := New(st, Options{Shadow: true})
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "fire-archived")
	if err != nil {
		t.Fatal(err)
	}
	row, err := cp.Workspaces.JobByName(ctx, ws.ID, "agents/a/x")
	if err != nil {
		t.Fatal(err)
	}
	cp.fireOne(ctx, schedule.Row{WorkspaceID: ws.ID, JobID: row.ID, JobName: "agents/a/x", Idx: 0, Spec: "@every 1h"})

	for table, q := range map[string]string{
		"runs":  `SELECT COUNT(*) FROM runs`,
		"inbox": `SELECT COUNT(*) FROM inbox_items`,
	} {
		var n int
		if err := st.DB().QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s rows=%d, want 0", table, n)
		}
	}
}
