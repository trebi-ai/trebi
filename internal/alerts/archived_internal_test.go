package alerts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestDoctorSkipsArchivedJobs(t *testing.T) {
	st, _ := archivedStore(t)
	var msgs []string
	for _, f := range agentKitFindings(st, launchView(t, st)) {
		msgs = append(msgs, f.ID+" "+f.Message)
	}
	joined := strings.Join(msgs, "\n")
	if !strings.Contains(joined, "live") || strings.Contains(joined, "old") {
		t.Fatalf("findings:\n%s", joined)
	}
}

func TestHarnessOffSkipsArchived(t *testing.T) {
	st, _ := archivedStore(t)
	var msgs []string
	for _, f := range harnessOffFindings(context.Background(), st.DB()) {
		msgs = append(msgs, f.ID)
	}
	joined := strings.Join(msgs, "\n")
	if !strings.Contains(joined, "live") || strings.Contains(joined, "old") {
		t.Fatalf("findings:\n%s", joined)
	}
	svc := New(st, Options{})
	if n := svc.jobsOnHarness(context.Background(), "claude"); n != 1 {
		t.Fatalf("jobsOnHarness = %d, want 1", n)
	}
}

// archivedStore indexes a live and an archived agent, and a live and an
// archived job on a harness that is off by default.
func archivedStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	dir := t.TempDir()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id,name,path,git_enabled,created_at,updated_at) VALUES ('ws','ws',?,0,datetime(),datetime())`, dir); err != nil {
		t.Fatal(err)
	}
	md := "---\nkit:\n  skills: [missing-skill]\n---\n\nRun.\n"
	for _, name := range []string{"live", "old"} {
		folder := filepath.Join(dir, "agents", name)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(folder, "AGENT.md")
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES (?,'ws',?,?,'agent',1,'claude','h',datetime())`,
			"agt_"+name, name, path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().Exec(`UPDATE job_index SET archived_at='2026-11-01T10:00:00Z' WHERE id='agt_old'`); err != nil {
		t.Fatal(err)
	}
	return st, dir
}
