package job_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

const discoverJobMD = "---\nharness: claude\n---\n\nDo the task.\n"

func TestDiscoverSkipsOtherWorkspaceRoots(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeJob(t, filepath.Join(root, "a"))
	writeJob(t, filepath.Join(root, "sub", "b"))

	// Without other roots, both jobs appear.
	disc, err := job.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(disc.Jobs) != 2 {
		t.Fatalf("jobs=%v want 2", jobNames(disc.Jobs))
	}

	// With sub registered as a workspace root, only "a" appears.
	disc, err = job.Discover(root, filepath.Join(root, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if len(disc.Jobs) != 1 || disc.Jobs[0].Name != "a" {
		t.Fatalf("jobs=%v want [a]", jobNames(disc.Jobs))
	}
}

func TestDiscoverSkipsNestedTrebiYAML(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeJob(t, filepath.Join(root, "a"))
	writeJob(t, filepath.Join(root, "sub", "b"))
	if err := os.WriteFile(filepath.Join(root, "sub", "trebi.yaml"), []byte("id: ws_x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	disc, err := job.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(disc.Jobs) != 1 || disc.Jobs[0].Name != "a" {
		t.Fatalf("jobs=%v want [a]", jobNames(disc.Jobs))
	}
}

func TestDiscoverValidAgent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeJob(t, filepath.Join(root, "a"))
	writeFile(t, filepath.Join(root, "agents", "cmo", "AGENT.md"),
		"---\nname: cmo\nharness: codex\n---\n\nLead growth.\n")
	writeJob(t, filepath.Join(root, "agents", "cmo", "duty"))

	disc, err := job.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(disc.Agents) != 1 {
		t.Fatalf("agents=%d want 1", len(disc.Agents))
	}
	ag := disc.Agents[0]
	if ag.Name != "agents/cmo" {
		t.Errorf("name=%q want agents/cmo", ag.Name)
	}
	if ag.Kind != "agent" {
		t.Errorf("kind=%q want agent", ag.Kind)
	}
	if ag.ParseError != "" {
		t.Errorf("parse_error=%q want empty", ag.ParseError)
	}
	if !ag.Memory {
		t.Error("memory=false want true (C.3 default)")
	}
	// Child job under the agent folder carries the owner.
	if len(disc.Jobs) != 2 {
		t.Fatalf("jobs=%v want [a, agents/cmo/duty]", jobNames(disc.Jobs))
	}
	var duty *job.Job
	for _, j := range disc.Jobs {
		if j.Name == "agents/cmo/duty" {
			duty = j
		}
	}
	if duty == nil {
		t.Fatalf("jobs=%v missing agents/cmo/duty", jobNames(disc.Jobs))
	}
	if duty.Owner != "agents/cmo" {
		t.Errorf("owner=%q want agents/cmo", duty.Owner)
	}
}

func TestDiscoverAgentLayoutRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rel  string // path of AGENT.md under the workspace root
		want string // substring of ParseError
	}{
		{"misplaced root", "AGENT.md", "misplaced AGENT.md"},
		{"misplaced deep", "team/AGENT.md", "misplaced AGENT.md"},
		{"nested root", "agents/cmo/bold/AGENT.md", "nested agent root"},
		{"bad folder name", "agents/Cmo/AGENT.md", "must match [a-z0-9][a-z0-9-]*"},
		{"soul beside", "agents/cmo/SOUL.md", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFile(t, filepath.Join(root, filepath.FromSlash(tc.rel)), "---\n---\n\nBody.\n")
			if tc.name == "soul beside" {
				// rel is the SOUL.md path; place AGENT.md beside it.
				dir := filepath.Dir(filepath.Join(root, filepath.FromSlash(tc.rel)))
				writeFile(t, filepath.Join(dir, "AGENT.md"), "---\n---\n\nBody.\n")
			}
			disc, err := job.Discover(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(disc.Agents) != 1 {
				t.Fatalf("agents=%d want 1", len(disc.Agents))
			}
			ag := disc.Agents[0]
			if ag.ParseError == "" {
				t.Fatal("parse_error empty want a message")
			}
			if !strings.Contains(ag.ParseError, "SOUL.md") && tc.name == "soul beside" {
				t.Fatalf("parse_error=%q want SOUL.md rule", ag.ParseError)
			}
			if tc.want != "" && !strings.Contains(ag.ParseError, tc.want) {
				t.Fatalf("parse_error=%q want %q", ag.ParseError, tc.want)
			}
		})
	}
}

func TestDiscoverAgentFrontmatterNameMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents", "cmo", "AGENT.md"),
		"---\nname: ceo\n---\n\nLead growth.\n")

	disc, err := job.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(disc.Agents) != 1 {
		t.Fatalf("agents=%d want 1", len(disc.Agents))
	}
	ag := disc.Agents[0]
	if ag.Name != "agents/cmo" {
		t.Errorf("name=%q want agents/cmo (folder wins)", ag.Name)
	}
	if !strings.Contains(ag.ParseError, "must equal folder name") {
		t.Errorf("parse_error=%q want name mismatch", ag.ParseError)
	}
}

func TestDiscoverAgentAndJobSameDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "agents", "cmo")
	writeFile(t, filepath.Join(dir, "AGENT.md"), "---\n---\n\nLead growth.\n")
	writeFile(t, filepath.Join(dir, "JOB.md"), discoverJobMD)

	disc, err := job.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly one row: the agent row with the parse error.
	if len(disc.Agents) != 1 || len(disc.Jobs) != 0 {
		t.Fatalf("agents=%d jobs=%v want 1 agent, no jobs", len(disc.Agents), jobNames(disc.Jobs))
	}
	if !strings.Contains(disc.Agents[0].ParseError, "same directory") {
		t.Errorf("parse_error=%q want same-directory rule", disc.Agents[0].ParseError)
	}
}

func TestDiscoverDuplicateAgentNames(t *testing.T) {
	t.Parallel()
	// Case-folded duplicates (agents/cmo and agents/Cmo) need a
	// case-sensitive disk. APFS here is case-insensitive, so this covers
	// the clean single-agent path only.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents", "cmo", "AGENT.md"), "---\n---\n\nA.\n")
	disc, err := job.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(disc.Agents) != 1 || disc.Agents[0].ParseError != "" {
		t.Fatalf("agents=%v want one clean agent", disc.Agents)
	}
}

func TestDiscoverSubstitutesJobVars(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	jobDir := filepath.Join(root, "digest")
	writeFile(t, filepath.Join(jobDir, "JOB.md"),
		"---\nharness: claude\n---\n\nRun {{job_dir}}/scripts/x.sh in {{workspace_dir}}\n")

	disc, err := job.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(disc.Jobs) != 1 {
		t.Fatalf("jobs=%d want 1", len(disc.Jobs))
	}
	got := disc.Jobs[0].Prompt
	want := "Run " + jobDir + "/scripts/x.sh in " + root
	if got != want {
		t.Errorf("Prompt = %q, want %q", got, want)
	}
}

func TestExpandPromptOverridesJobDir(t *testing.T) {
	t.Parallel()
	got := job.ExpandPrompt("at {{job_dir}} for {{who}}", "/jobs/a", "/ws", map[string]string{
		"job_dir": "/pinned",
		"who":     "fritz",
	})
	want := "at /pinned for fritz"
	if got != want {
		t.Errorf("ExpandPrompt = %q, want %q", got, want)
	}
}

func TestDiscoverSubstitutesAgentVars(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents", "cmo", "AGENT.md"),
		"---\n---\n\nBase: {{agent_dir}}\nHome: {{workspace_dir}}\nJobs: {{agent_jobs}}\n")

	disc, err := job.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(disc.Agents) != 1 {
		t.Fatalf("agents=%d want 1", len(disc.Agents))
	}
	body := disc.Agents[0].Prompt
	if !strings.Contains(body, filepath.Join(root, "agents", "cmo")) {
		t.Errorf("body=%q want agent_dir expanded", body)
	}
	if !strings.Contains(body, root) {
		t.Errorf("body=%q want workspace_dir expanded", body)
	}
	if !strings.Contains(body, "{{agent_jobs}}") {
		t.Errorf("body=%q want agent_jobs literal (wake-time)", body)
	}
}

func TestJobUnderAgentFolderWithoutAGENT(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// A bare JOB.md directly in agents/<name> has no owner: the rule needs a
	// path below agents/<seg> (pure path rule, Part D).
	writeFile(t, filepath.Join(root, "agents", "ghost", "JOB.md"), discoverJobMD)

	disc, err := job.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(disc.Jobs) != 1 || disc.Jobs[0].Owner != "" {
		t.Fatalf("jobs=%v want one unowned job", disc.Jobs)
	}
	if disc.Jobs[0].Name != "agents/ghost" {
		t.Errorf("name=%q want agents/ghost", disc.Jobs[0].Name)
	}
}

func writeJob(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "JOB.md"), []byte(discoverJobMD), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jobNames(jobs []*job.Job) []string {
	var out []string
	for _, j := range jobs {
		out = append(out, j.Name)
	}
	return out
}

func TestLayoutIssues(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const ws = "ws1"
	// Real files only where the sibling stat must find them.
	agentMD := "---\nharness: claude\n---\n\nRun the area.\n"

	cases := []struct {
		name  string
		rows  []job.LayoutRow
		extra []string // sibling files the stat must find (not index rows)
		want  []string // codes, in row order
	}{
		{
			name: "canonical agent and owned job are clean",
			rows: []job.LayoutRow{
				{WorkspaceID: ws, Name: "agents/cmo", Path: filepath.Join(root, "agents", "cmo", "AGENT.md"), Kind: "agent"},
				{WorkspaceID: ws, Name: "agents/cmo/weekly", Path: filepath.Join(root, "agents", "cmo", "weekly", "JOB.md"), Kind: "job", Owner: "agents/cmo"},
				{WorkspaceID: ws, Name: "trebi", Path: filepath.Join(root, "TREBI.md"), Kind: "assistant"},
			},
			want: nil,
		},
		{
			name: "misplaced agent",
			rows: []job.LayoutRow{
				{WorkspaceID: ws, Name: "notes", Path: filepath.Join(root, "notes", "AGENT.md"), Kind: "agent"},
			},
			want: []string{job.CodeAgentMisplaced},
		},
		{
			name: "nested agent",
			rows: []job.LayoutRow{
				{WorkspaceID: ws, Name: "agents/cmo/scout", Path: filepath.Join(root, "agents", "cmo", "scout", "AGENT.md"), Kind: "agent"},
			},
			want: []string{job.CodeAgentNested},
		},
		{
			name: "JOB.md beside AGENT.md",
			rows: []job.LayoutRow{
				{WorkspaceID: ws, Name: "agents/cmo", Path: filepath.Join(root, "rj", "AGENT.md"), Kind: "agent"},
			},
			extra: []string{filepath.Join(root, "rj", "JOB.md")},
			want:  []string{job.CodeAgentRootJob},
		},
		{
			name: "owned job whose agent row is gone",
			rows: []job.LayoutRow{
				{WorkspaceID: ws, Name: "agents/gone/weekly", Path: filepath.Join(root, "agents", "gone", "weekly", "JOB.md"), Kind: "job", Owner: "agents/gone"},
			},
			want: []string{job.CodeAgentOrphanedJob},
		},
		{
			name: "job at an agent root with no AGENT.md",
			rows: []job.LayoutRow{
				{WorkspaceID: ws, Name: "agents/ghost", Path: filepath.Join(root, "agents", "ghost", "JOB.md"), Kind: "job"},
			},
			want: []string{job.CodeAgentOrphanedJob},
		},
		{
			name: "owner in another workspace does not adopt",
			rows: []job.LayoutRow{
				{WorkspaceID: "ws2", Name: "agents/cmo", Path: filepath.Join(root, "agents", "cmo", "AGENT.md"), Kind: "agent"},
				{WorkspaceID: ws, Name: "agents/cmo/weekly", Path: filepath.Join(root, "agents", "cmo", "weekly", "JOB.md"), Kind: "job", Owner: "agents/cmo"},
			},
			want: []string{job.CodeAgentOrphanedJob},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, r := range tc.rows {
				content := discoverJobMD
				if r.Kind == "agent" {
					content = agentMD
				}
				writeFile(t, r.Path, content)
			}
			for _, p := range tc.extra {
				writeFile(t, p, discoverJobMD)
			}
			got := job.LayoutIssues(tc.rows)
			var codes []string
			for _, iss := range got {
				codes = append(codes, iss.Code)
				if iss.Severity() == "" || iss.Message() == "" || !strings.Contains(iss.Message(), iss.Path) {
					t.Errorf("issue %+v needs severity and a message naming the path", iss)
				}
			}
			if strings.Join(codes, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("codes=%v want %v", codes, tc.want)
			}
		})
	}
}

func TestLayoutIssueSeverity(t *testing.T) {
	t.Parallel()
	errorCodes := map[string]bool{job.CodeAgentNested: true, job.CodeAgentRootJob: true}
	for _, code := range []string{job.CodeAgentMisplaced, job.CodeAgentNested, job.CodeAgentRootJob, job.CodeAgentOrphanedJob} {
		iss := job.LayoutIssue{Code: code, Path: "/x"}
		want := "warn"
		if errorCodes[code] {
			want = "error"
		}
		if iss.Severity() != want {
			t.Errorf("%s severity=%q want %q", code, iss.Severity(), want)
		}
	}
}
