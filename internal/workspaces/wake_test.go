package workspaces_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// memoryMD is a valid v1 MEMORY.md fixture: marker, H1, four sections.
const memoryMD = `<!-- trebi memory v1. Keep these four sections in this order. Do not add other "## " headings. -->
# Memory

## Context
Durable facts about the workspace.

## State
Watching CI.

## Lessons
- [2026-09-01 09:00] Prune stale branches weekly.

## Log
- [2026-09-02 08:00] Ran the weekly digest.
`

func TestComposeAgentWake(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		files   map[string]string
		opts    runs.WakeOpts
		want    []string
		notWant []string
	}{
		{
			name: "folder scoped",
			files: map[string]string{
				"TREBI.md":               "---\nharness: claude\n---\n\nWorkspace.\n",
				"alpha/JOB.md":           jobMD,
				"agents/cmo/AGENT.md":    "---\nharness: codex\n---\n\nLead growth from {{agent_dir}}.\n",
				"agents/cmo/duty/JOB.md": jobMD,
				"agents/cmo/MEMORY.md":   memoryMD,
			},
			opts: runs.WakeOpts{Message: "check ads", Recap: "leak", Memory: wakeMemoryLaunch("", "agents/cmo/MEMORY.md")},
			want: []string{
				"# Instructions", "Lead growth from {{agent_dir}}.",
				"# Your jobs", "- agents/cmo/duty kind=job harness=claude active",
				"# Memory", "## Context", "Durable facts about the workspace.",
				"- [2026-09-02 08:00] Ran the weekly digest.",
				"# Conversation recap", "leak",
				"# Current message", "check ads",
			},
			notWant: []string{"# Agents", "Workspace jobs", "- alpha kind=job"},
		},
		{
			name: "body override still expands vars",
			files: map[string]string{
				"agents/cmo/AGENT.md":    "---\n---\n\nFile body.\n",
				"agents/cmo/duty/JOB.md": jobMD,
			},
			opts:    runs.WakeOpts{Body: "Raw {{agent_dir}} body."},
			want:    []string{"# Instructions", "Raw {{agent_dir}} body."},
			notWant: []string{"File body.", "# Memory"},
		},
		{
			name: "inline agent_jobs",
			files: map[string]string{
				"agents/cmo/AGENT.md":    "---\n---\n\nDuties:\n{{agent_jobs}}\nEnd.\n",
				"agents/cmo/duty/JOB.md": jobMD,
			},
			want:    []string{"Duties:", "- agents/cmo/duty kind=job", "End."},
			notWant: []string{"# Your jobs", "{{agent_jobs}}", "# Memory"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, wsID, wsPath := wakeFixture(t, tc.files)
			if tc.opts.Memory != nil {
				tc.opts.Memory.Root = wsPath
			}
			got, err := svc.ComposeWakePrompt(context.Background(), runs.WakeScope{
				WorkspaceID: wsID, Kind: "agent", Name: "agents/cmo",
			}, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			// {{agent_dir}} expands to the agent folder.
			got = strings.ReplaceAll(got, filepath.Join(wsPath, "agents", "cmo"), "{{agent_dir}}")
			assertWake(t, got, tc.want, tc.notWant)
		})
	}
}

func TestComposeWakeTruncatesHugeIndex(t *testing.T) {
	t.Parallel()
	files := map[string]string{"agents/cmo/AGENT.md": "---\n---\n\nLead.\n"}
	for i := 0; i < 250; i++ {
		files[fmt.Sprintf("agents/cmo/duty%03d/JOB.md", i)] = jobMD
	}
	svc, wsID, _ := wakeFixture(t, files)
	got, err := svc.ComposeWakePrompt(context.Background(),
		runs.WakeScope{WorkspaceID: wsID, Kind: runs.KindAgent, Name: "agents/cmo"}, runs.WakeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Your jobs") || !strings.Contains(got, "…[truncated]") {
		t.Fatalf("index not truncated:\n%.200s…", got)
	}
}
func assertWake(t *testing.T, got string, want, notWant []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
	}
	for _, w := range notWant {
		if strings.Contains(got, w) {
			t.Fatalf("unexpected %q in:\n%s", w, got)
		}
	}
}

func assertSectionOrder(t *testing.T, got string, keys ...string) {
	t.Helper()
	pos := 0
	for _, k := range keys {
		i := strings.Index(got, k)
		if i < 0 {
			continue
		}
		if i < pos {
			t.Fatalf("section order: %q at %d before %d\n%s", k, i, pos, got)
		}
		pos = i
	}
}

// wakeFixture writes files (workspace-relative) and indexes the workspace.
func wakeFixture(t *testing.T, files map[string]string) (*workspaces.Service, string, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenPath(filepath.Join(dir, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := workspaces.New(st)
	wsPath := filepath.Join(dir, "ws")
	for rel, content := range files {
		p := filepath.Join(wsPath, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
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
	return svc, ws.ID, wsPath
}

// wakeMemoryLaunch builds the launch seam for a fixture workspace file.
func wakeMemoryLaunch(wsPath, rel string) *runs.MemoryLaunch {
	return &runs.MemoryLaunch{
		Root: wsPath, Rel: rel,
		Opt: memory.Options{MaxLogs: 100, RequireUpdate: true, InjectLogs: 20},
	}
}
