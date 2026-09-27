package runs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentwire "github.com/trebi-ai/agent-wire"
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
// an mcp-config file mode 0600 and names it on the command line.
func TestClaudeInjectMCPWritesTightConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	plan := &tmux.LaunchPlan{Command: "claude"}
	servers := []agentwire.MCPServer{{Name: "gh", Command: "npx", Args: []string{"-y", "gh"}}}
	if err := tmux.Detect("claude").(tmux.MCPInjector).InjectMCP(plan, servers, dir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Command, "--mcp-config") {
		t.Fatalf("command = %q", plan.Command)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, e := range entries {
		if strings.Contains(e.Name(), "mcp") {
			found = filepath.Join(dir, e.Name())
		}
	}
	if found == "" {
		t.Fatalf("no mcp config in %v", entries)
	}
	st, err := os.Stat(found)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v; want 0600", st.Mode().Perm())
	}
	body, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"gh"`) || !strings.Contains(string(body), "npx") {
		t.Fatalf("config = %s", body)
	}
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
