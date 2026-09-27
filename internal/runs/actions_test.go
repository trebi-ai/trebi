package runs

import (
	"context"
	"encoding/json"
	"errors"
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

// TestActionsSupportedMatrix proves decision 8: every harness takes CLI
// actions, the CLI runtime takes MCP actions where agent-wire injects them,
// and the pane runtime needs an MCPInjector tool.
func TestActionsSupportedMatrix(t *testing.T) {
	svc, ctx := paneStack(t)
	// No vendor binary on PATH: Detect then reports only the static features.
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{"claude", "pi", "grok", "gemini"} {
		for _, rt := range []string{"cli", "tui"} {
			if err := svc.ActionsSupported(ctx, name, rt, false); err != nil {
				t.Errorf("%s %s: CLI actions: %v", name, rt, err)
			}
		}
	}
	for _, name := range []string{"claude", "codex", "opencode", "copilot", "cursor", "gemini"} {
		if err := svc.ActionsSupported(ctx, name, "cli", true); err != nil {
			t.Errorf("%s: cli runtime takes MCP actions: %v", name, err)
		}
	}
	// pi needs a per-run home and grok has no MCP seam (decision 8).
	for _, name := range []string{"pi", "grok"} {
		if svc.ActionsSupported(ctx, name, "cli", true) == nil || svc.ActionsSupported(ctx, name, "tui", true) == nil {
			t.Errorf("%s: no runtime takes MCP actions", name)
		}
	}
	for _, name := range []string{"pi", "cursor", "grok", "gemini", "copilot"} {
		if err := svc.ActionsSupported(ctx, name, "tui", true); !errors.Is(err, ErrActionsUnsupported) {
			t.Errorf("%s: pane runtime takes no MCP action: %v", name, err)
		}
	}
	for _, name := range []string{"claude", "codex", "opencode"} {
		if err := svc.ActionsSupported(ctx, name, "tui", true); err != nil {
			t.Errorf("%s: pane runtime takes MCP actions: %v", name, err)
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
	tool := tmux.Detect("claude")
	plan, err := preparePlan(tool, "claude", &opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := injectMCPPlan(tool, &plan, &opts); err != nil {
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

// TestPaneResumeInjectsMCP proves the pane resume plan gets the MCP config
// like a fresh launch (P1-20).
func TestPaneResumeInjectsMCP(t *testing.T) {
	paneStack(t)
	opts := LaunchOpts{ResumeSessionID: "sess-1", Actions: []ConnectorAction{{
		Name: "gh", MCP: &agentwire.MCPServer{Name: "gh", Command: "npx"},
	}}}
	tool := tmux.Detect("claude")
	plan, err := preparePlan(tool, "claude", &opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := injectMCPPlan(tool, &plan, &opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Command, "--resume") || !strings.Contains(plan.Command, "--mcp-config") {
		t.Fatalf("resume command = %q", plan.Command)
	}
}

// TestPaneCodexSecretsOffCommandLine proves the codex pane command names env
// vars only; the values go to the 0600 env file (P1-21).
func TestPaneCodexSecretsOffCommandLine(t *testing.T) {
	paneStack(t)
	opts := LaunchOpts{Actions: []ConnectorAction{
		{Name: "gh", MCP: &agentwire.MCPServer{Name: "gh", Command: "npx", Env: map[string]string{"GH_TOKEN": "s3cret-env"}}},
		{Name: "api", MCP: &agentwire.MCPServer{Name: "api", URL: "https://x.test/mcp", Headers: map[string]string{"Authorization": "Bearer s3cret-hdr"}}},
	}}
	tool := tmux.Detect("codex")
	plan, err := preparePlan(tool, "codex", &opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := injectMCPPlan(tool, &plan, &opts); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.Command, "s3cret") {
		t.Fatalf("secret on the command line: %q", plan.Command)
	}
	if !strings.Contains(plan.Command, "env_vars") || !strings.Contains(plan.Command, "env_http_headers") {
		t.Fatalf("command = %q", plan.Command)
	}
	_, path, err := paneEnvFile(t.TempDir(), "01RUN", "tok", "codex", nil, nil, plan.Env)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "s3cret-env") || !strings.Contains(string(body), "s3cret-hdr") {
		t.Fatalf("env file = %s", body)
	}

	// A job env value that differs from the MCP env is a conflict.
	opts.Env = map[string]string{"GH_TOKEN": "other"}
	plan, err = preparePlan(tool, "codex", &opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := injectMCPPlan(tool, &plan, &opts); !errors.Is(err, ErrActionsInvalid) {
		t.Fatalf("conflict err = %v", err)
	}
}

// TestPaneOpenCodeMergesJobConfig proves the injected servers merge into an
// OPENCODE_CONFIG_CONTENT from the job env, and a value that is not a JSON
// object is refused (P1-22).
func TestPaneOpenCodeMergesJobConfig(t *testing.T) {
	paneStack(t)
	opts := LaunchOpts{
		Env: map[string]string{"OPENCODE_CONFIG_CONTENT": `{"theme":"dark","mcp":{"mine":{"type":"remote","url":"https://m.test"}}}`},
		Actions: []ConnectorAction{{
			Name: "gh", MCP: &agentwire.MCPServer{Name: "gh", Command: "npx"},
		}},
	}
	tool := tmux.Detect("opencode")
	plan, err := preparePlan(tool, "opencode", &opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := injectMCPPlan(tool, &plan, &opts); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Theme string                    `json:"theme"`
		MCP   map[string]map[string]any `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(plan.Env["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Theme != "dark" || cfg.MCP["mine"] == nil || cfg.MCP["gh"] == nil {
		t.Fatalf("config = %+v", cfg)
	}

	opts.Env = map[string]string{"OPENCODE_CONFIG_CONTENT": `["not","an","object"]`}
	plan, err = preparePlan(tool, "opencode", &opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := injectMCPPlan(tool, &plan, &opts); !errors.Is(err, ErrActionsInvalid) {
		t.Fatalf("bad config err = %v", err)
	}
}
