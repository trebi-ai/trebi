package alerts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/store"
)

func TestAgentKitFindings(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agents", "research")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nkit:\n  skills: [missing-skill]\n  mcp: [missing-mcp]\n---\n\nRun.\n"
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
	if !codes["agent_skill_missing"] || !codes["agent_mcp_missing"] {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestInsideAgentFolder(t *testing.T) {
	if !insideAgentFolder("research/notes.md", "agents/research") ||
		!insideAgentFolder("research", "agents/research") ||
		insideAgentFolder("research/../outside.txt", "agents/research") {
		t.Fatal("agent folder containment mismatch")
	}
}
