package workspaces_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/state"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// memoryMD is a valid MEMORY.md fixture: marker, H1, three sections.
const memoryMD = `<!-- trebi memory v2. Keep these three sections in this order. Do not add other "## " headings. -->
# Memory

## Context
Durable facts about the workspace.

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
				"agents/cmo/AGENT.md":    "---\ndescription: Test agent.\nharness: codex\n---\n\nLead growth from {{agent_dir}}.\n",
				"agents/cmo/duty/JOB.md": jobMD,
				"agents/cmo/MEMORY.md":   memoryMD,
			},
			opts: runs.WakeOpts{Message: "check ads", Recap: "leak", Memory: wakeMemoryLaunch("", "agents/cmo/MEMORY.md")},
			want: []string{
				"# Instructions", "Lead growth from {{agent_dir}}.",
				"# Your jobs", "- cmo/duty kind=job harness=claude active",
				"# Memory", "## Context", "Durable facts about the workspace.",
				"- [2026-09-02 08:00] Ran the weekly digest.",
				"# Conversation recap", "leak",
				"# Current message", "check ads",
				runs.ChatStyleLine,
			},
			notWant: []string{"# Agents", "Workspace jobs", "- alpha kind=job"},
		},
		{
			name: "body override still expands vars",
			files: map[string]string{
				"agents/cmo/AGENT.md":    "---\ndescription: Test agent.\n---\n\nFile body.\n",
				"agents/cmo/duty/JOB.md": jobMD,
			},
			opts:    runs.WakeOpts{Body: "Raw {{agent_dir}} body."},
			want:    []string{"# Instructions", "Raw {{agent_dir}} body."},
			notWant: []string{"File body.", "# Memory"},
		},
		{
			name: "inline agent_jobs",
			files: map[string]string{
				"agents/cmo/AGENT.md":    "---\ndescription: Test agent.\n---\n\nDuties:\n{{agent_jobs}}\nEnd.\n",
				"agents/cmo/duty/JOB.md": jobMD,
			},
			want:    []string{"Duties:", "- cmo/duty kind=job", "End."},
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
				WorkspaceID: wsID, Kind: "agent", Name: "cmo",
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
	files := map[string]string{"agents/cmo/AGENT.md": "---\ndescription: Test agent.\n---\n\nLead.\n"}
	for i := 0; i < 250; i++ {
		files[fmt.Sprintf("agents/cmo/duty%03d/JOB.md", i)] = jobMD
	}
	svc, wsID, _ := wakeFixture(t, files)
	got, err := svc.ComposeWakePrompt(context.Background(),
		runs.WakeScope{WorkspaceID: wsID, Kind: runs.KindAgent, Name: "cmo"}, runs.WakeOpts{})
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

// fakeBriefer returns a fixed State block and keeps the owner it got.
type fakeBriefer struct{ got state.Owner }

func (f *fakeBriefer) Brief(_ context.Context, o state.Owner) (string, error) {
	f.got = o
	return "## State\n| reported | 3 |\n", nil
}

// TestComposeAgentWakeState proves an agent wake has the State section after
// Memory, for the agent ID and its declared collections.
func TestComposeAgentWakeState(t *testing.T) {
	t.Parallel()
	svc, wsID, _ := wakeFixture(t, map[string]string{
		"agents/cmo/AGENT.md": "---\ndescription: Test agent.\nstate:\n  reported:\n    description: Sent items.\n---\n\nLead.\n",
	})
	b := &fakeBriefer{}
	svc.SetStateBriefer(b)
	got, err := svc.ComposeWakePrompt(context.Background(), runs.WakeScope{
		WorkspaceID: wsID, Kind: "agent", Name: "cmo",
	}, runs.WakeOpts{Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	assertWake(t, got, []string{"# State", "| reported | 3 |", "# Current message"}, []string{"## State"})
	if !strings.Contains(got[:strings.Index(got, "# Current message")], "# State") {
		t.Fatalf("State is not before the message:\n%s", got)
	}
	if !strings.HasPrefix(b.got.ID, "agt_") || b.got.WorkspaceID != wsID || b.got.Collections["reported"].Description != "Sent items." {
		t.Fatalf("owner = %+v", b.got)
	}
}

var updateGolden = flag.Bool("update", false, "rewrite the golden files")

func TestComposeWakeTeam(t *testing.T) {
	t.Parallel()
	team := map[string]string{
		"agents/cmo/AGENT.md":      "---\ndescription: Growth plans.\n---\n\nLead growth.\n",
		"agents/cfo/AGENT.md":      "---\ndescription: Invoices, vendor payments, and monthly spend.\n---\n\nMoney.\n",
		"agents/legal/AGENT.md":    "---\ndescription: Contracts, terms, and compliance questions.\n---\n\nLaw.\n",
		"agents/off/AGENT.md":      "---\ndescription: Disabled.\ndisabled: true\n---\n\nOff.\n",
		"agents/broken/AGENT.md":   "---\n---\n\nNo description.\n",
		"agents/private/AGENT.md":  "---\ndescription: Keeps to itself.\nteam:\n  chains: false\n---\n\nAlone.\n",
		"agents/cmo/duty/JOB.md":   jobMD,
		"agents/legal/MEMORY.md":   memoryMD,
		"agents/cfo/report/JOB.md": jobMD,
	}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range team {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	cases := []struct {
		name    string
		files   map[string]string
		opts    runs.WakeOpts
		chains  *workspaces.ChainPatch
		section string // "" compares the whole prompt is free of a team section
	}{
		{name: "team", files: team, section: "# Your team"},
		{name: "team_can_ask", files: with(map[string]string{"agents/cmo/AGENT.md": "---\ndescription: Growth plans.\nteam:\n  can_ask: [legal]\n---\n\nLead growth.\n"}), section: "# Your team"},
		{name: "team_chain", files: team, section: "# Your team", opts: runs.WakeOpts{Chain: &runs.WakeChain{Depth: 2, MaxHops: 5, Turns: 7, MaxTurns: 20, Waiting: []string{"ceo", "cfo"}}}},
		{name: "team_inline", files: with(map[string]string{"agents/cmo/AGENT.md": "---\ndescription: Growth plans.\n---\n\nAsk these agents:\n{{agent_team}}\nEnd.\n"}), section: "# Instructions"},
		{name: "team_off", files: team, chains: &workspaces.ChainPatch{Enabled: new(false)}},
		{name: "team_self_off", files: with(map[string]string{"agents/cmo/AGENT.md": "---\ndescription: Growth plans.\nteam:\n  chains: false\n---\n\nLead growth.\n"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, wsID, _ := wakeFixture(t, tc.files)
			if tc.chains != nil {
				if _, err := svc.Patch(context.Background(), wsID, workspaces.Patch{Chains: tc.chains}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := svc.ComposeWakePrompt(context.Background(), runs.WakeScope{WorkspaceID: wsID, Kind: "agent", Name: "cmo"}, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.section == "" {
				assertWake(t, got, nil, []string{"# Your team", "trebi agent ask"})
				return
			}
			assertWake(t, got, nil, []string{"broken", "off:", "private", "{{agent_team}}", "- cmo:"})
			body := wakeSection(got, tc.section)
			golden := filepath.Join("testdata", "wake", tc.name+".golden")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if body != string(want) {
				t.Fatalf("%s section:\n%s\nwant:\n%s", tc.section, body, want)
			}
		})
	}
}

// wakeSection returns one "# Title" section of a wake prompt, up to the next.
func wakeSection(prompt, title string) string {
	i := strings.Index(prompt, title+"\n")
	if i < 0 {
		return ""
	}
	rest := prompt[i:]
	if j := strings.Index(rest[len(title):], "\n# "); j >= 0 {
		rest = rest[:len(title)+j]
	}
	return strings.TrimSpace(rest) + "\n"
}

func TestComposeWakeSkipsArchived(t *testing.T) {
	t.Parallel()
	svc, wsID, _ := wakeFixture(t, map[string]string{
		"agents/cmo/AGENT.md":    "---\ndescription: Growth plans.\n---\n\nLead growth.\n",
		"agents/cfo/AGENT.md":    "---\ndescription: Invoices.\narchived_at: 2026-11-18T10:00:00Z\n---\n\nMoney.\n",
		"agents/legal/AGENT.md":  "---\ndescription: Contracts.\n---\n\nLaw.\n",
		"agents/cmo/duty/JOB.md": jobMD,
		"agents/cmo/old/JOB.md":  "---\nharness: claude\narchived_at: 2026-11-18T10:00:00Z\n---\n\nOld.\n",
	})
	got, err := svc.ComposeWakePrompt(context.Background(), runs.WakeScope{WorkspaceID: wsID, Kind: "agent", Name: "cmo"}, runs.WakeOpts{Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	assertWake(t, got, []string{"- legal:", "cmo/duty"}, []string{"- cfo:", "cmo/old"})
}
