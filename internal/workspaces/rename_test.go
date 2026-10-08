package workspaces_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/workspaces"
)

// TestRenameJobOwned: an owned job keeps its ID, owner, schedule, memory,
// and earlier runs. The stored working folder follows the move.
func TestRenameJobOwned(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	svc, wsID, root, agentID, jobID := renameFixture(ctx, t, st)
	writeFile(t, filepath.Join(root, "agents/old/brief/MEMORY.md"), "# Memory\n")
	oldDir := filepath.Join(root, "agents/old/brief")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE runs SET working_dir=? WHERE id='run-owned'`, oldDir)
	exec(`UPDATE runs SET working_dir=? WHERE id='run-old'`, oldDir+"-other")
	exec(`UPDATE schedules SET next_due_at='2030-01-01T00:00:00Z' WHERE job_id=?`, jobID)
	seq := mustSeq(t, st)

	res, _, err := svc.RenameJob(ctx, wsID, "agents/old/brief", "digest")
	if err != nil {
		t.Fatal(err)
	}
	if res != (workspaces.JobRenamed{ID: jobID, From: "agents/old/brief", To: "agents/old/digest"}) {
		t.Fatalf("reply=%+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "agents/old/digest/MEMORY.md")); err != nil {
		t.Fatalf("memory: %v", err)
	}
	row, err := svc.JobByID(ctx, jobID)
	if err != nil || row.Name != "agents/old/digest" || row.OwnerID != agentID || row.Path != filepath.Join(root, "agents/old/digest/JOB.md") {
		t.Fatalf("row=%+v err=%v", row, err)
	}
	checks := []struct{ q, want string }{
		{`SELECT working_dir FROM runs WHERE id='run-owned'`, filepath.Join(root, "agents/old/digest")},
		{`SELECT working_dir FROM runs WHERE id='run-old'`, oldDir + "-other"},
		{`SELECT next_due_at FROM schedules WHERE job_id='` + jobID + `'`, "2030-01-01T00:00:00Z"},
	}
	for _, c := range checks {
		var got string
		if err := st.DB().QueryRowContext(ctx, c.q).Scan(&got); err != nil || got != c.want {
			t.Fatalf("%s = %q (%v), want %q", c.q, got, err, c.want)
		}
	}
	events, err := st.ListBus(ctx, seq, []string{"job"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	topics := map[string]bool{}
	for _, e := range events {
		topics[e.Topic] = true
	}
	if !topics["job.renamed"] || !topics["job.updated"] {
		t.Fatalf("events=%+v", events)
	}
}

func TestRenameJobLooseAndRefusals(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "reports/weekly/JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "reports/daily/JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "agents/a/AGENT.md"), "---\ndescription: A.\n---\n\nA.\n")
	svc := workspaces.New(st)
	ws, err := svc.Add(ctx, root, "loose")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, to string
		want     error
	}{
		{"reports/weekly", "Bad_Name", workspaces.ErrInvalidName},
		{"reports/weekly", "daily", workspaces.ErrExists},
		{filepath.Base(root), "x", workspaces.ErrRootJob},
		{"agents/a", "b", workspaces.ErrNotJob},
		{"reports/nope", "b", workspaces.ErrNotFound},
	}
	for _, c := range cases {
		if _, _, err := svc.RenameJob(ctx, ws.ID, c.name, c.to); !errors.Is(err, c.want) {
			t.Fatalf("%s -> %s: err=%v want %v", c.name, c.to, err, c.want)
		}
	}
	weekly, err := svc.JobByName(ctx, ws.ID, "reports/weekly")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO runs (id,workspace_id,job_id,kind,status,trace_id,started_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`, "live", ws.ID, weekly.ID, "job", "running", "trace", now, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.RenameJob(ctx, ws.ID, "reports/weekly", "digest"); !errors.Is(err, workspaces.ErrLiveRun) {
		t.Fatalf("live err=%v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM runs WHERE id='live'`); err != nil {
		t.Fatal(err)
	}
	moved, _, err := svc.RenameJob(ctx, ws.ID, "reports/weekly", "digest")
	if err != nil || moved.To != "reports/digest" || moved.ID != weekly.ID {
		t.Fatalf("moved=%+v err=%v", moved, err)
	}
}

// TestRenameAgentRewritesWorkingDir: the runs of child jobs follow the move.
func TestRenameAgentRewritesWorkingDir(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	svc, wsID, root, _, jobID := renameFixture(ctx, t, st)
	if _, err := st.DB().ExecContext(ctx, `UPDATE runs SET working_dir=? WHERE id='run-owned'`, filepath.Join(root, "agents/old/brief")); err != nil {
		t.Fatal(err)
	}
	moved, err := svc.RenameAgent(ctx, wsID, "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 2 {
		t.Fatalf("moved=%d", len(moved))
	}
	var got string
	if err := st.DB().QueryRowContext(ctx, `SELECT working_dir FROM runs WHERE id='run-owned'`).Scan(&got); err != nil || got != filepath.Join(root, "agents/new/brief") {
		t.Fatalf("working_dir=%q err=%v", got, err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schedules WHERE job_id=?`, jobID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("schedules=%d err=%v", n, err)
	}
}
