package alerts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func TestAgentKitFindings(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agents", "research")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nkit:\n  skills: [missing-skill]\n  connectors: [missing-connector]\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id,name,path,git_enabled,created_at,updated_at) VALUES ('ws','ws',?,0,datetime(),datetime())`, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO job_index (workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES ('ws','agents/research',?,'agent',1,'claude','hash',datetime())`, filepath.Join(agentDir, "AGENT.md")); err != nil {
		t.Fatal(err)
	}
	findings := agentKitFindings(st, launchView(t, st))
	codes := map[string]bool{}
	for _, finding := range findings {
		codes[finding.Code] = true
	}
	if !codes["agent_skill_missing"] || !codes["kit_connector_missing"] {
		t.Fatalf("findings = %+v", findings)
	}
}

// TestAgentEnvsetFinding covers plan H.3.8: an envset name that is not
// defined in envsets.yaml fires a finding; a defined name does not.
func TestAgentEnvsetFinding(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	if err := os.WriteFile(filepath.Join(home.Base(), "envsets.yaml"), []byte("prod:\n  K: v\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agents", "research")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nkit:\n  envset: [prod, ghost]\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id,name,path,git_enabled,created_at,updated_at) VALUES ('ws','ws',?,0,datetime(),datetime())`, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO job_index (workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES ('ws','agents/research',?,'agent',1,'claude','hash',datetime())`, filepath.Join(agentDir, "AGENT.md")); err != nil {
		t.Fatal(err)
	}

	var found []DoctorFinding
	for _, finding := range agentKitFindings(st, launchView(t, st)) {
		if finding.Code == "agent_envset_missing" {
			found = append(found, finding)
		}
	}
	if len(found) != 1 {
		t.Fatalf("envset findings = %+v", found)
	}
	if !strings.Contains(found[0].Message, "ghost") || strings.Contains(found[0].Message, "prod") {
		t.Fatalf("finding = %+v", found[0])
	}
}

func TestInsideAgentFolder(t *testing.T) {
	if !insideAgentFolder("research/notes.md", "agents/research") ||
		!insideAgentFolder("research", "agents/research") ||
		insideAgentFolder("research/../outside.txt", "agents/research") {
		t.Fatal("agent folder containment mismatch")
	}
}

// TestConnectorKitFindings covers the F.5 doctor checks: an inherited name, a
// connector with no actions block, and a CLI command that is not on PATH.
func TestConnectorKitFindings(t *testing.T) {
	st := kitStore(t, []string{
		inlineYAML("actions-only", "actions:\n  cli:\n    commands: [trebi-definitely-missing-command]\n"),
		inlineYAML("events-only", "events:\n  command: \"true\"\n"),
	}, "actions-only, events-only, ghost")
	findings := agentKitFindings(st, launchView(t, st))
	codes := map[string]bool{}
	for _, finding := range findings {
		codes[finding.Code] = true
	}
	for _, want := range []string{"connector_command_missing", "kit_connector_no_actions", "kit_connector_missing"} {
		if !codes[want] {
			t.Fatalf("missing %s in %+v", want, findings)
		}
	}
}

// TestConnectorKitFindingsMCPOnly proves a kit that names an MCP-only
// connector does not panic and gives no command finding.
func TestConnectorKitFindingsMCPOnly(t *testing.T) {
	st := kitStore(t, []string{inlineYAML("gh", "actions:\n  mcp:\n    command: npx\n")}, "gh")
	for _, finding := range agentKitFindings(st, launchView(t, st)) {
		if finding.Code == "connector_command_missing" {
			t.Fatalf("unexpected finding %+v", finding)
		}
	}
}

// TestDoctorUsesLaunchView proves the doctor sees the connectors a launch
// uses (P1-24): an owned job gets the owner kit, a top-level connectors: on
// the agent replaces its kit, and the support table is the launch one.
func TestDoctorUsesLaunchView(t *testing.T) {
	st := kitStore(t, []string{inlineYAML("gh", "actions:\n  mcp:\n    command: npx\n")}, "kit-only")
	dir := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	// The agent research (from kitStore) names kit-only in its kit. An owned
	// job with no list of its own inherits that kit.
	writeJob(t, st, dir, "agents/research/daily", "job", "agents/research", "---\n---\n\nRun.\n")
	// An agent with a top-level list: the kit list is not used.
	writeJob(t, st, dir, "agents/pi-agent", "agent", "", "---\nharness: pi\nconnectors: [gh]\nkit:\n  connectors: [kit-ghost]\n---\n\nRun.\n")
	byID := map[string]DoctorFinding{}
	for _, f := range agentKitFindings(st, launchView(t, st)) {
		byID[f.ID+"|"+f.Code] = f
	}
	if _, ok := byID["connector-ws-research/daily-kit-only|kit_connector_missing"]; !ok {
		t.Fatalf("owned job does not see the owner kit: %+v", byID)
	}
	if _, ok := byID["connector-ws-pi-agent-gh|kit_actions_unsupported"]; !ok {
		t.Fatalf("pi MCP action not reported: %+v", byID)
	}
	for k := range byID {
		if strings.Contains(k, "kit-ghost") {
			t.Fatalf("kit list used next to a top-level list: %s", k)
		}
	}
}

// TestKitMCPRemovedFinding proves an old kit.mcp list gets a finding that
// names the connectors to add (P1-25).
func TestKitMCPRemovedFinding(t *testing.T) {
	st := kitStore(t, nil, "")
	dir := t.TempDir()
	writeJob(t, st, dir, "agents/old", "agent", "", "---\nkit:\n  mcp: [github, linear]\n---\n\nRun.\n")
	for _, f := range agentKitFindings(st, launchView(t, st)) {
		if f.Code == "kit_mcp_removed" {
			if !strings.Contains(f.Message, "github, linear") || !strings.Contains(f.Message, "kit.connectors") {
				t.Fatalf("message = %q", f.Message)
			}
			return
		}
	}
	t.Fatal("no kit_mcp_removed finding")
}

// TestChannelOperatorConflictFinding: an agent and the workspace assistant
// that both trigger on one channel connector give one finding; a connector
// without a channel gives none.
func TestChannelOperatorConflictFinding(t *testing.T) {
	st := kitStore(t, []string{
		inlineYAML("chat", "events:\n  protocol: trebi-connector/1\n  command: cat\n  types:\n    - type: message\nchannel:\n  features: [typing]\n"),
		inlineYAML("feed", "events:\n  command: cat\n"),
	}, "")
	var dir string
	if err := st.DB().QueryRow(`SELECT path FROM workspaces WHERE id='ws'`).Scan(&dir); err != nil {
		t.Fatal(err)
	}
	md := "---\ntriggers:\n  - event: chat\n  - event: feed\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(dir, "agents", "research", "AGENT.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	yml := "assistant:\n  triggers:\n    - event: chat\n    - event: feed\n"
	if err := os.WriteFile(filepath.Join(dir, "trebi.yaml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	var found []DoctorFinding
	for _, f := range agentKitFindings(st, nil) {
		if f.Code == "channel_operator_conflict" {
			found = append(found, f)
		}
	}
	if len(found) != 1 {
		t.Fatalf("conflict findings = %+v", found)
	}
	if found[0].ID != "channel-operator-conflict-ws-chat" ||
		!strings.Contains(found[0].Message, "agents/research") || !strings.Contains(found[0].Message, "assistant") {
		t.Fatalf("finding = %+v", found[0])
	}
}

// inlineYAML is the connection.yaml of one inline connection whose slug and
// type are name; body is manifest YAML.
func inlineYAML(name, body string) string {
	m := "schema: trebi-connector/1\nname: " + name + "\ntitle: T\ndescription: D\nversion: 1.0.0\n" + body
	return "id: " + connectors.NewConnectionID() + "\ntype: " + name + "\nslug: " + name +
		"\nfrom: inline\nmanifest:\n  " + strings.ReplaceAll(strings.TrimRight(m, "\n"), "\n", "\n  ") + "\n"
}

// kitStore writes one folder per connection and one agent whose kit names
// connectors.
func kitStore(t *testing.T, conns []string, kitConnectors string) *store.Store {
	t.Helper()
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	for _, c := range conns {
		id := strings.TrimPrefix(strings.SplitN(c, "\n", 2)[0], "id: ")
		if err := os.MkdirAll(home.ConnectionDir(id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home.ConnectionDir(id), "connection.yaml"), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agents", "research")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nkit:\n  connectors: [" + kitConnectors + "]\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id,name,path,git_enabled,created_at,updated_at) VALUES ('ws','ws',?,0,datetime(),datetime())`, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO job_index (workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES ('ws','agents/research',?,'agent',1,'claude','hash',datetime())`, filepath.Join(agentDir, "AGENT.md")); err != nil {
		t.Fatal(err)
	}
	return st
}

// launchView is the runs service the daemon wires into the doctor.
func launchView(t *testing.T, st *store.Store) ActionsChecker {
	t.Helper()
	r := runs.New(st, nil, nil)
	r.SetKitSource(workspaces.New(st))
	return r
}

// writeJob writes one job file and indexes it.
func writeJob(t *testing.T, st *store.Store, dir, name, kind, owner, md string) {
	t.Helper()
	file := "JOB.md"
	if kind == "agent" {
		file = "AGENT.md"
	}
	path := filepath.Join(dir, filepath.FromSlash(name), file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO job_index (workspace_id,name,path,kind,active,harness,hash,updated_at,owner) VALUES ('ws',?,?,?,1,'claude','hash',datetime(),?)`, name, path, kind, owner); err != nil {
		t.Fatal(err)
	}
}
