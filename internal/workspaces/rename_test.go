package workspaces_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
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

// TestMoveJobToOtherAgent: a move to another agent keeps the ID, schedule,
// state, memory, and runs, and sets the new folder in the file.
func TestMoveJobToOtherAgent(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	svc, wsID, root, _, jobID := renameFixture(ctx, t, st)
	writeFile(t, filepath.Join(root, "agents/old/AGENT.md"), "---\ndescription: Old.\nkit:\n  connectors: [email, discord]\n---\n\nRun brief daily.\n")
	writeFile(t, filepath.Join(root, "agents/cs/AGENT.md"), "---\ndescription: CS.\nkit:\n  connectors: [discord, slack]\n---\n\nCS.\n")
	writeFile(t, filepath.Join(root, "agents/old/brief/JOB.md"), "---\nharness: claude\nfolder: Old\n---\n\nDo the task.\n")
	writeFile(t, filepath.Join(root, "agents/old/brief/MEMORY.md"), "# Memory\n")
	ws, err := svc.Get(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	cs, err := svc.JobByName(ctx, wsID, "agents/cs")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE runs SET working_dir=? WHERE id='run-owned'`, filepath.Join(root, "agents/old/brief"))
	exec(`UPDATE schedules SET next_due_at='2030-01-01T00:00:00Z' WHERE job_id=?`, jobID)
	agent, folder := "cs", "Health"

	dry, _, err := svc.MoveJob(ctx, wsID, "agents/old/brief", workspaces.JobMove{Agent: &agent, Folder: &folder, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Moved || dry.To != "agents/cs/brief" || dry.FromAgent != "old" || dry.ToAgent != "cs" {
		t.Fatalf("dry=%+v", dry)
	}
	notes := strings.Join(dry.Changes, "\n")
	for _, want := range []string{"runs as cs", "loses these connectors: email", "gets these connectors: slack", "instructions of old name this job"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("changes %q lack %q", notes, want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "agents/old/brief/JOB.md")); err != nil {
		t.Fatalf("dry run moved the job: %v", err)
	}

	seq := mustSeq(t, st)
	res, moved, err := svc.MoveJob(ctx, wsID, "agents/old/brief", workspaces.JobMove{Agent: &agent, Folder: &folder})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Moved || res.ID != jobID || len(moved) != 1 || moved[0].Folder != "Health" {
		t.Fatalf("res=%+v moved=%+v", res, moved)
	}
	row, err := svc.JobByID(ctx, jobID)
	if err != nil || row.Name != "agents/cs/brief" || row.OwnerID != cs.ID || row.Folder != "Health" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
	if _, err := os.Stat(filepath.Join(root, "agents/cs/brief/MEMORY.md")); err != nil {
		t.Fatalf("memory: %v", err)
	}
	checks := []struct{ q, want string }{
		{`SELECT working_dir FROM runs WHERE id='run-owned'`, filepath.Join(root, "agents/cs/brief")},
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
	found := false
	for _, e := range events {
		found = found || e.Topic == "job.moved"
	}
	if !found {
		t.Fatalf("events=%+v", events)
	}

	// A move to no agent is refused.
	none := ""
	if _, _, err = svc.MoveJob(ctx, wsID, jobID, workspaces.JobMove{Agent: &none}); !errors.Is(err, workspaces.ErrMoveNeedsAgent) {
		t.Fatalf("err=%v", err)
	}
}

// TestMoveJobFolderOnly: the same agent keeps the path; only the folder changes.
func TestMoveJobFolderOnly(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	svc, wsID, root, _, jobID := renameFixture(ctx, t, st)
	folder := "SEO"
	res, moved, err := svc.MoveJob(ctx, wsID, "agents/old/brief", workspaces.JobMove{Folder: &folder})
	if err != nil || !res.Moved || res.To != "agents/old/brief" || len(res.Changes) != 0 || moved != nil {
		t.Fatalf("res=%+v moved=%v err=%v", res, moved, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "agents/old/brief/JOB.md"))
	if err != nil || !strings.Contains(string(data), "folder: SEO") {
		t.Fatalf("file=%q err=%v", data, err)
	}
	ws, _ := svc.Get(ctx, wsID)
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if row, err := svc.JobByID(ctx, jobID); err != nil || row.Folder != "SEO" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func TestMoveJobRefusals(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "reports/weekly/JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "agents/a/AGENT.md"), "---\ndescription: A.\n---\n\nA.\n")
	writeFile(t, filepath.Join(root, "agents/a/weekly/JOB.md"), jobMD)
	svc := workspaces.New(st)
	ws, err := svc.Add(ctx, root, "moves")
	if err != nil {
		t.Fatal(err)
	}
	str := func(s string) *string { return &s }
	cases := []struct {
		name string
		m    workspaces.JobMove
		want error
	}{
		{"reports/weekly", workspaces.JobMove{Agent: str("nope")}, workspaces.ErrNoAgent},
		{"reports/weekly", workspaces.JobMove{Folder: str("SEO")}, workspaces.ErrMoveNeedsAgent},
		{"agents/a/weekly", workspaces.JobMove{Agent: str("")}, workspaces.ErrMoveNeedsAgent},
		{"reports/weekly", workspaces.JobMove{Agent: str("a")}, workspaces.ErrExists},
		{"agents/a/weekly", workspaces.JobMove{Folder: str("a/b")}, nil},
		{filepath.Base(root), workspaces.JobMove{Agent: str("a")}, workspaces.ErrRootJob},
		{"agents/a", workspaces.JobMove{Agent: str("")}, workspaces.ErrNotJob},
		{"reports/nope", workspaces.JobMove{Agent: str("a")}, workspaces.ErrNotFound},
	}
	for _, c := range cases {
		_, _, err := svc.MoveJob(ctx, ws.ID, c.name, c.m)
		if c.want == nil {
			if err == nil {
				t.Fatalf("%s: want an error", c.name)
			}
			continue
		}
		if !errors.Is(err, c.want) {
			t.Fatalf("%s: err=%v want %v", c.name, err, c.want)
		}
	}
	// A job outside an agent has the parse error and moves into an agent,
	// which clears the error.
	row, err := svc.JobByName(ctx, ws.ID, "reports/weekly")
	if err != nil || !strings.Contains(row.ParseError, job.ErrNoOwner.Error()) {
		t.Fatalf("row=%+v err=%v", row, err)
	}
	if err := os.Rename(filepath.Join(root, "agents/a/weekly"), filepath.Join(root, "agents/a/old-weekly")); err != nil {
		t.Fatal(err)
	}
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	res, _, err := svc.MoveJob(ctx, ws.ID, "reports/weekly", workspaces.JobMove{Agent: str("a")})
	if err != nil || !res.Moved || res.To != "agents/a/weekly" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if row, err = svc.JobByID(ctx, row.ID); err != nil || row.ParseError != "" || row.Name != "agents/a/weekly" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func TestRenameFolder(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents/a/AGENT.md"), "---\ndescription: A.\n---\n\nA.\n")
	writeFile(t, filepath.Join(root, "agents/a/one/JOB.md"), "---\nharness: claude\nfolder: Customers\n---\n\nOne.\n")
	writeFile(t, filepath.Join(root, "agents/a/two/JOB.md"), "---\nharness: claude\nfolder: Customers\n---\n\nTwo.\n")
	writeFile(t, filepath.Join(root, "agents/a/three/JOB.md"), "---\nharness: claude\nfolder: SEO\n---\n\nThree.\n")
	svc := workspaces.New(st)
	ws, err := svc.Add(ctx, root, "folders")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RenameFolder(ctx, ws.ID, "a", "Customers", "a/b"); err == nil {
		t.Fatal("nested folder: want an error")
	}
	if _, err := svc.RenameFolder(ctx, ws.ID, "nope", "Customers", "Health"); !errors.Is(err, workspaces.ErrNoAgent) {
		t.Fatalf("err=%v", err)
	}
	if _, err := svc.RenameFolder(ctx, ws.ID, "a", "Missing", "Health"); !errors.Is(err, workspaces.ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
	res, err := svc.RenameFolder(ctx, ws.ID, "a", "Customers", "Health")
	if err != nil || res.Jobs != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	want := map[string]string{"agents/a/one": "Health", "agents/a/two": "Health", "agents/a/three": "SEO"}
	for name, folder := range want {
		if row, err := svc.JobByName(ctx, ws.ID, name); err != nil || row.Folder != folder {
			t.Fatalf("%s: row=%+v err=%v", name, row, err)
		}
	}
	if res, err := svc.RenameFolder(ctx, ws.ID, "a", "Health", ""); err != nil || res.Jobs != 2 {
		t.Fatalf("remove: res=%+v err=%v", res, err)
	}
	if row, err := svc.JobByName(ctx, ws.ID, "agents/a/one"); err != nil || row.Folder != "" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func TestArchivedNameStaysReserved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, ws := archiveFixture(t)
	if _, err := svc.SetArchived(ctx, ws, archiveRow(t, svc, ws, "agents/boss/a"), true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.RenameJob(ctx, ws.ID, "agents/boss/b", "a"); !errors.Is(err, workspaces.ErrExists) {
		t.Fatalf("rename onto an archived name: err = %v, want ErrExists", err)
	}
}

func TestRenameJobUpdatesRoutes(t *testing.T) {
	ctx := context.Background()
	svc, st, ws := routingWS(t)
	path := writeDigestRoutes(t, ws.Path)
	before := routingUpdates(t, st)
	if _, _, err := svc.RenameJob(ctx, ws.ID, "agents/ops/digest", "brief"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "job: ops/brief") || !strings.Contains(string(got), "to: ops/brief") || !strings.Contains(string(got), "agent: ops\n") {
		t.Fatalf("routes after rename:\n%s", got)
	}
	if n := routingUpdates(t, st) - before; n != 1 {
		t.Fatalf("routing.updated = %d, want 1", n)
	}
}

func TestMoveJobUpdatesRoutes(t *testing.T) {
	ctx := context.Background()
	svc, _, ws := routingWS(t)
	path := writeDigestRoutes(t, ws.Path)
	agent := "cmo"
	if _, _, err := svc.MoveJob(ctx, ws.ID, "agents/ops/digest", workspaces.JobMove{Agent: &agent}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "job: cmo/digest") || !strings.Contains(string(got), "to: cmo/digest") {
		t.Fatalf("routes after move:\n%s", got)
	}
}

func TestRenameAgentUpdatesRoutes(t *testing.T) {
	ctx := context.Background()
	svc, _, ws := routingWS(t)
	path := writeDigestRoutes(t, ws.Path)
	if _, err := svc.RenameAgent(ctx, ws.ID, "ops", "ops2"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	for _, want := range []string{"agent: ops2", "job: ops2/digest", "to: ops2/digest"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
}

func writeDigestRoutes(t *testing.T, root string) string {
	t.Helper()
	return routingtest.Write(t, root, "gmail", routing.File{Routes: []routing.Route{
		{ID: "rt_aaaaaaaaaa", Agent: "ops", Gets: routing.GetsOff},
		{ID: "rt_bbbbbbbbbb", Job: "ops/digest", Gets: routing.GetsOff},
	}, Rules: []routing.Rule{{Field: routing.RuleSenderIs, Value: "a@b.c", To: "ops/digest"}}})
}
