package alerts

import (
	"fmt"
	"hash/crc32"
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
	if _, err := st.DB().Exec(`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES ('agt_0000000001','ws','agents/research',?,'agent',1,'claude','hash',datetime())`, filepath.Join(agentDir, "AGENT.md")); err != nil {
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
	if _, err := st.DB().Exec(`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES ('agt_0000000001','ws','agents/research',?,'agent',1,'claude','hash',datetime())`, filepath.Join(agentDir, "AGENT.md")); err != nil {
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
// the agent adds to its kit, the built-in email name is no finding, and the
// support table is the launch one.
func TestDoctorUsesLaunchView(t *testing.T) {
	st := kitStore(t, []string{inlineYAML("gh", "actions:\n  mcp:\n    command: npx\n")}, "kit-only")
	dir := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	// The agent research (from kitStore) names kit-only in its kit. An owned
	// job with no list of its own inherits that kit.
	writeJob(t, st, dir, "agents/research/daily", "job", "agents/research", "---\n---\n\nRun.\n")
	// An agent with a top-level list: it adds to the kit list.
	writeJob(t, st, dir, "agents/pi-agent", "agent", "", "---\nharness: pi\nconnectors: [gh]\nkit:\n  connectors: [kit-ghost, email]\n---\n\nRun.\n")
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
	if _, ok := byID["connector-ws-pi-agent-kit-ghost|kit_connector_missing"]; !ok {
		t.Fatalf("kit list not merged with the top-level list: %+v", byID)
	}
	for k := range byID {
		if strings.Contains(k, "-email|") {
			t.Fatalf("built-in email reported: %s", k)
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
	if _, err := st.DB().Exec(`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES ('agt_0000000001','ws','agents/research',?,'agent',1,'claude','hash',datetime())`, filepath.Join(agentDir, "AGENT.md")); err != nil {
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
	prefix := "jb_"
	if kind == "agent" {
		prefix = "agt_"
	}
	id := fmt.Sprintf("%s%010x", prefix, crc32.ChecksumIEEE([]byte(name)))
	if _, err := st.DB().Exec(`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at,owner_id)
		VALUES (?,'ws',?,?,?,1,'claude','hash',datetime(),COALESCE((SELECT o.id FROM job_index o WHERE o.name = ?),''))`, id, name, path, kind, owner); err != nil {
		t.Fatal(err)
	}
}

func TestStateRetiredFinding(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db := st.DB()
	if _, ok := stateRetiredFinding(t.Context(), db); ok {
		t.Fatal("finding on an empty store")
	}
	for _, q := range []string{
		`INSERT INTO job_index (id, workspace_id, name, path, updated_at, retired_at) VALUES
			('jb_live','ws','live','/x','2026-10-01T00:00:00Z',NULL),
			('jb_gone','ws','gone','/y','2026-10-01T00:00:00Z','2026-10-02T00:00:00Z'),
			('jb_empty','ws','empty','/z','2026-10-01T00:00:00Z','2026-09-01T00:00:00Z')`,
		`INSERT INTO state_rows (owner_id, collection, key, value, created_at, updated_at) VALUES
			('jb_live','c','k','{}','2026-10-01T00:00:00Z','2026-10-01T00:00:00Z'),
			('jb_gone','c','k','{}','2026-10-01T00:00:00Z','2026-10-01T00:00:00Z'),
			('jb_gone','c','k2','{}','2026-10-01T00:00:00Z','2026-10-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	f, ok := stateRetiredFinding(t.Context(), db)
	if !ok || f.Code != "state_retired" || f.Severity != "info" || !strings.Contains(f.Message, "1 deleted") || !strings.Contains(f.Message, "2026-11-01") {
		t.Fatalf("finding %+v", f)
	}
}

func TestDuplicateIDFinding(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, reason := range []string{"missing", "duplicate"} {
		if _, err := st.Append(t.Context(), store.BusEvent{
			Topic: "job.id_assigned", Entity: "job", EntityID: "jb_" + reason, Action: "updated",
			Attrs: map[string]string{"workspace": "ws", "name": "copy-" + reason, "id": "jb_" + reason, "reason": reason},
		}); err != nil {
			t.Fatal(err)
		}
	}
	f, ok := duplicateIDFinding(t.Context(), st)
	if !ok || f.Code != "id_duplicate" || !strings.Contains(f.Message, "copy-duplicate") || strings.Contains(f.Message, "copy-missing") {
		t.Fatalf("finding %+v", f)
	}
}

func TestAgentTeamUnknownFinding(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	dir := t.TempDir()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id,name,path,git_enabled,created_at,updated_at) VALUES ('ws','ws',?,0,datetime(),datetime())`, dir); err != nil {
		t.Fatal(err)
	}
	for i, a := range []struct{ name, md string }{
		{"ceo", "---\ndescription: Runs the company.\nteam:\n  can_ask: [cfo, ghost]\n---\n\nLead.\n"},
		{"cfo", "---\ndescription: Money.\n---\n\nMoney.\n"},
	} {
		path := filepath.Join(dir, "agents", a.name, "AGENT.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(a.md), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES (?,'ws',?,?,'agent',1,'claude','hash',datetime())`, fmt.Sprintf("agt_000000000%d", i), "agents/"+a.name, path); err != nil {
			t.Fatal(err)
		}
	}
	var found []DoctorFinding
	for _, f := range agentKitFindings(st, nil) {
		if f.Code == "agent_team_unknown" {
			found = append(found, f)
		}
	}
	if len(found) != 1 || !strings.Contains(found[0].Message, "ghost") {
		t.Fatalf("team findings = %+v", found)
	}
}
