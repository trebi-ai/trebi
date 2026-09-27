package alerts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
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
	findings := agentKitFindings(st)
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
	for _, finding := range agentKitFindings(st) {
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
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	if err := os.WriteFile(filepath.Join(home.Base(), "connectors.yaml"), []byte(
		"actions-only:\n  actions:\n    cli:\n      commands: [trebi-definitely-missing-command]\n"+
			"events-only:\n  events:\n    command: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agents", "research")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nkit:\n  connectors: [actions-only, events-only, ghost]\n---\n\nRun.\n"
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
	findings := agentKitFindings(st)
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
