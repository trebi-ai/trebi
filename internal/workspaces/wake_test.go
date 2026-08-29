package workspaces_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func TestComposeWakePromptOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		trebi   string
		memory  string
		jobs    bool
		recap   string
		extra   string
		want    []string
		notWant []string
	}{
		{
			name:  "empty memory no jobs",
			trebi: "---\nharness: claude\n---\n\nYou are the assistant.\n",
			want:  []string{"# Instructions", "You are the assistant.", "# Workspace jobs", "kind=assistant", "# Memory", "(empty)"},
		},
		{
			name:   "recap present",
			trebi:  "---\nharness: claude\n---\n\nBody.\n",
			memory: "# ledger\nnote",
			recap:  "user: hi\nassistant: hello",
			extra:  "focus on CI",
			want:   []string{"# Instructions", "Body.", "# Memory", "note", "# Conversation recap", "user: hi", "# Current message", "focus on CI"},
		},
		{
			name:    "huge index truncated",
			trebi:   "---\nharness: claude\n---\n\nX.\n",
			jobs:    true,
			want:    []string{"# Workspace jobs", "…[truncated]"},
			notWant: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, wsID := wakeFixture(t, tc.trebi, tc.memory, tc.jobs)
			got, err := svc.ComposeWakePrompt(context.Background(), wsID, workspaces.WakeOpts{
				Recap: tc.recap, ExtraPrompt: tc.extra, Message: tc.extra,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("missing %q in:\n%s", w, got)
				}
			}
			assertSectionOrder(t, got)
		})
	}
}

func assertSectionOrder(t *testing.T, got string) {
	t.Helper()
	keys := []string{"# Instructions", "# Workspace jobs", "# Memory"}
	pos := 0
	for _, k := range keys {
		i := strings.Index(got, k)
		if i < pos {
			t.Fatalf("section order: %q at %d before %d\n%s", k, i, pos, got)
		}
		pos = i
	}
}

func wakeFixture(t *testing.T, trebi, memory string, hugeIndex bool) (*workspaces.Service, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenPath(filepath.Join(dir, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := workspaces.New(st)
	wsPath := filepath.Join(dir, "ws")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "TREBI.md"), []byte(trebi), 0o644); err != nil {
		t.Fatal(err)
	}
	if memory != "" {
		if err := os.WriteFile(filepath.Join(wsPath, "MEMORY.md"), []byte(memory), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := svc.Add(context.Background(), wsPath, "wake-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RebuildIndex(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if hugeIndex {
		pad := strings.Repeat("jobxxxxxxxx ", 2000)
		_, _ = st.DB().Exec(`UPDATE job_index SET name=? WHERE workspace_id=?`, pad, ws.ID)
	}
	return svc, ws.ID
}
