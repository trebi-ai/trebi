package runs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// TestActionEnvMergesConnectorEnv proves only CLI action env reaches the run
// shell when connectors mix MCP and CLI actions.
func TestActionEnvMergesConnectorEnv(t *testing.T) {
	t.Parallel()
	actions := []ConnectorAction{
		{Name: "files", Env: map[string]string{"ROOT": "/srv"}},
		{Name: "search", MCP: &agentwire.MCPServer{Name: "search", Command: "npx"}, Env: nil},
		{Name: "db", Env: map[string]string{"ROOT": "/data", "MODE": "ro"}},
	}
	env := actionEnv(actions)
	if env["ROOT"] != "/data" || env["MODE"] != "ro" || len(env) != 2 {
		t.Fatalf("env = %v", env)
	}
	if actionEnv(nil) != nil {
		t.Fatal("no actions must give no env")
	}
}

// TestActionEnvIsNilForMCPOnly proves an MCP-only connector leaks no secret
// into the run shell.
func TestActionEnvIsNilForMCPOnly(t *testing.T) {
	t.Parallel()
	actions := []ConnectorAction{{
		Name: "gh",
		MCP:  &agentwire.MCPServer{Name: "gh", Command: "npx", Env: map[string]string{"TOKEN": "secret:x"}},
	}}
	if env := actionEnv(actions); len(env) != 0 {
		t.Fatalf("env = %v; MCP secrets stay in the MCP config", env)
	}
}

// TestMCPServersListsOnlyMCPActions proves the CLI-only connector is absent.
func TestMCPServersListsOnlyMCPActions(t *testing.T) {
	t.Parallel()
	actions := []ConnectorAction{
		{Name: "files", Commands: []string{"ls"}},
		{Name: "gh", MCP: &agentwire.MCPServer{Name: "gh", Command: "npx", Args: []string{"-y", "gh"}}},
	}
	servers := mcpServers(actions)
	if len(servers) != 1 || servers[0].Name != "gh" || servers[0].Command != "npx" {
		t.Fatalf("servers = %+v", servers)
	}
	if hasMCP(actions) != true || hasMCP(actions[:1]) != false {
		t.Fatal("hasMCP must follow the MCP action")
	}
}

// TestActionSkillsNeedADir proves a skill without a directory is not
// projected: the harness would otherwise load an empty path.
func TestActionSkillsNeedADir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	actions := []ConnectorAction{
		{Name: "psql", Skill: "postgres", SkillDir: dir},
		{Name: "named-only", Skill: "postgres"},
	}
	skills := actionSkills(actions)
	if len(skills) != 1 || skills[0].Name != "postgres" || skills[0].Dir != dir {
		t.Fatalf("skills = %+v", skills)
	}
}

// TestConnectorHintLines proves the prompt hint names commands and skills and
// skips an MCP-only connector.
func TestConnectorHintLines(t *testing.T) {
	t.Parallel()
	actions := []ConnectorAction{
		{Name: "psql", Commands: []string{"psql"}, Skill: "postgres"},
		{Name: "gh", MCP: &agentwire.MCPServer{Name: "gh", Command: "npx"}},
		{Name: "jq", Commands: []string{"jq", "yq"}},
	}
	hint := connectorHint(actions)
	if !strings.Contains(hint, "Connector psql: use psql (skill postgres).") {
		t.Fatalf("hint = %q", hint)
	}
	if !strings.Contains(hint, "Connector jq: use jq, yq.") {
		t.Fatalf("hint = %q", hint)
	}
	if strings.Contains(hint, "gh") {
		t.Fatalf("hint names an MCP-only connector: %q", hint)
	}
}

// TestSupportsActionsMatrix proves decision 8: every CLI harness takes CLI
// actions, and only an MCPInjector tool takes an MCP action in the pane.
func TestSupportsActionsMatrix(t *testing.T) {
	t.Parallel()
	// The gate only sees a launch that carries an MCP action (the caller
	// returns early for CLI-only actions). These six harnesses take one.
	for _, name := range []string{"claude", "codex", "opencode", "copilot", "cursor", "gemini"} {
		if !supportsActions(tmux.Detect(name), true) {
			t.Errorf("%s: cli runtime takes MCP actions", name)
		}
	}
	// pi has no MCP seam and grok has no driver (decision 8).
	for _, name := range []string{"pi", "grok"} {
		if supportsActions(tmux.Detect(name), true) || supportsActions(tmux.Detect(name), false) {
			t.Errorf("%s: no runtime takes MCP actions", name)
		}
	}
	for _, name := range []string{"pi", "cursor", "grok", "gemini", "copilot"} {
		if supportsActions(tmux.Detect(name), false) {
			t.Errorf("%s: pane runtime takes no MCP action", name)
		}
	}
	for _, name := range []string{"claude", "codex", "opencode"} {
		if !supportsActions(tmux.Detect(name), false) {
			t.Errorf("%s: pane runtime takes MCP actions", name)
		}
	}
}

// TestClaudeInjectMCPWritesTightConfig proves the pane claude launch writes
// a private mcp-config file mode 0600, names it on the command line, and
// never overwrites a file.
func TestClaudeInjectMCPWritesTightConfig(t *testing.T) {
	t.Parallel()
	files := tmux.RunFiles{Dir: t.TempDir(), RunID: "01JABCDEFGHJKMNPQRSTVWXYZ0"}
	plan := &tmux.LaunchPlan{Command: "claude"}
	servers := []agentwire.MCPServer{{Name: "gh", Command: "npx", Args: []string{"-y", "gh"}}}
	inj := tmux.Detect("claude").(tmux.MCPInjector)
	if err := inj.InjectMCP(plan, servers, files); err != nil {
		t.Fatal(err)
	}
	path := files.Path("mcp.json")
	if !strings.Contains(plan.Command, "--mcp-config") || !strings.Contains(plan.Command, path) {
		t.Fatalf("command = %q", plan.Command)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v; want 0600", st.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"gh"`) || !strings.Contains(string(body), "npx") {
		t.Fatalf("config = %s", body)
	}
	if err := inj.InjectMCP(&tmux.LaunchPlan{Command: "claude"}, servers, files); err == nil {
		t.Fatal("second inject overwrote the file")
	}
}

// TestPaneClaudeMCPConfigLeavesJobDir proves a pane claude launch with an MCP
// action writes nothing under the working directory, and the run end removes
// the private file.
func TestPaneClaudeMCPConfigLeavesJobDir(t *testing.T) {
	svc, ctx := paneStack(t)
	wd := t.TempDir()
	opts := LaunchOpts{Actions: []ConnectorAction{{
		Name: "gh",
		MCP:  &agentwire.MCPServer{Name: "gh", Command: "npx", Env: map[string]string{"TOKEN": "tok"}},
	}}}
	opts.WorkingDir = wd
	if _, err := preparePlan(tmux.Detect("claude"), "claude", &opts); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(wd); len(entries) != 0 {
		t.Fatalf("working dir holds %v", entries)
	}
	id := opts.CreateOpts.ID
	path := runFiles(id).Path("mcp.json")
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("private file %s: %v %v", path, st, err)
	}
	run, err := svc.Create(ctx, opts.CreateOpts)
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != id {
		t.Fatalf("run id %s; want the preset %s", run.ID, id)
	}
	if err := svc.Terminate(ctx, id, StatusExited, EndKill, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file survived the run end: %v", err)
	}
}

// TestRemoveStaleRunFiles proves the start sweep removes the private files
// of unknown and terminal runs and keeps those of a live run.
func TestRemoveStaleRunFiles(t *testing.T) {
	svc, ctx := paneStack(t)
	live, err := svc.Create(ctx, CreateOpts{Job: "live"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home.RunsLogDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	keep := runFiles(live.ID).Path("mcp.json")
	gone := runFiles("01JABCDEFGHJKMNPQRSTVWXYZ0").Path("mcp.json")
	for _, p := range []string{keep, gone} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := svc.RemoveStaleRunFiles(ctx); err != nil || n != 1 {
		t.Fatalf("removed %d: %v", n, err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("live run file removed: %v", err)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatalf("stale file kept: %v", err)
	}
}

// paneStack opens a runs service over a fresh home.
func paneStack(t *testing.T) (*Service, context.Context) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st, nil, nil), context.Background()
}

// TestPaneEnvFileHidesActionEnvFromArgv proves the pane env file carries the
// connector env and stays mode 0600.
func TestPaneEnvFileHidesActionEnvFromArgv(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, path, err := paneEnvFile(dir, "01RUN", "tok", "claude",
		map[string]string{"ROOT": "/srv"}, map[string]string{"EXTRA": "1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v; want 0600", st.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "ROOT") || !strings.Contains(text, "/srv") {
		t.Fatalf("env file = %s", text)
	}
	if !strings.Contains(text, "EXTRA") {
		t.Fatalf("env file drops the launch env: %s", text)
	}
}
