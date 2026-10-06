package workspaces_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// TestRenameAgentKeepsReferencesAndEmitsOnce: a rename moves the folder and
// the index names. The IDs stay, so the state rows need no update.
func TestRenameAgentKeepsReferencesAndEmitsOnce(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	svc, wsID, root, agentID, jobID := renameFixture(ctx, t, st)
	seq := mustSeq(t, st)

	if err := svc.RenameAgent(ctx, wsID, "old", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "agents/new/AGENT.md")); err != nil {
		t.Fatalf("new folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "agents/old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old folder still exists: %v", err)
	}

	checks := []struct{ q, want string }{
		{`SELECT job_id FROM runs WHERE id='run-old'`, agentID},
		{`SELECT job_id FROM runs WHERE id='run-owned'`, jobID},
		{`SELECT job_id FROM event_queue WHERE id='eq'`, jobID},
		{`SELECT job_id FROM schedules WHERE idx=0`, jobID},
		{`SELECT agent_id FROM email_messages WHERE id='em'`, agentID},
		{`SELECT operator_id FROM conversations WHERE id='conv'`, agentID},
		{`SELECT ` + store.JobNameSQL("job_id") + ` FROM runs WHERE id='run-owned'`, "agents/new/brief"},
	}
	for _, c := range checks {
		var got string
		if err := st.DB().QueryRowContext(ctx, c.q).Scan(&got); err != nil || got != c.want {
			t.Fatalf("%s = %q (%v), want %q", c.q, got, err, c.want)
		}
	}
	brief, err := svc.JobByID(ctx, jobID)
	if err != nil || brief.Name != "agents/new/brief" || brief.OwnerID != agentID {
		t.Fatalf("job by id=%+v err=%v", brief, err)
	}
	rows, err := svc.ListJobs(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	var agentPath string
	agentCount := 0
	for _, row := range rows {
		if row.Kind != "agent" {
			continue
		}
		agentCount++
		if row.Name == "agents/new" && row.ID == agentID {
			agentPath = row.Path
		}
	}
	if agentCount != 1 || agentPath == "" || !strings.HasSuffix(agentPath, filepath.Join("agents", "new", "AGENT.md")) {
		t.Fatalf("index rows=%d path=%q", agentCount, agentPath)
	}

	events, err := st.ListBus(ctx, seq, []string{"agent"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Topic != "agent.renamed" || events[0].ID != agentID {
		t.Fatalf("agent events=%+v", events)
	}
}

func TestRenameAgentConflictsAndRollsBackTransaction(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	svc, wsID, _, agentID, _ := renameFixture(ctx, t, st)

	if _, err := st.DB().ExecContext(ctx, `INSERT INTO runs (id,workspace_id,job_id,kind,status,trace_id,started_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`, "live", wsID, agentID, "agent", "running", "trace", time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := svc.RenameAgent(ctx, wsID, "old", "new"); !errors.Is(err, workspaces.ErrAgentLiveRun) {
		t.Fatalf("live err=%v", err)
	}

	if _, err := st.DB().ExecContext(ctx, `DELETE FROM runs WHERE id='live'`); err != nil {
		t.Fatal(err)
	}
	if err := svc.RenameAgent(ctx, wsID, "old", "new"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RenameAgent(ctx, wsID, "new", "new"); err == nil {
		t.Fatal("same rename unexpectedly succeeded")
	}

	// Build an independent fixture and force the transaction to fail.
	st2 := openTest(t)
	svc2, ws2, root2, _, _ := renameFixture(ctx, t, st2)
	if _, err := st2.DB().ExecContext(ctx, `CREATE TRIGGER force_rename_failure BEFORE UPDATE ON job_index WHEN new.name LIKE 'agents/new%' BEGIN SELECT RAISE(ABORT, 'forced'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc2.RenameAgent(ctx, ws2, "old", "new"); err == nil {
		t.Fatal("forced transaction failure did not surface")
	}
	if _, err := os.Stat(filepath.Join(root2, "agents/old/AGENT.md")); err != nil {
		t.Fatalf("folder not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root2, "agents/new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new folder survived rollback: %v", err)
	}
}

// renameFixture makes an agent with one owned job, plus state rows that
// reference both by ID. It returns the agent ID and the job ID.
func renameFixture(ctx context.Context, t *testing.T, st *store.Store) (svc *workspaces.Service, wsID, root, agentID, jobID string) {
	t.Helper()
	root = t.TempDir()
	writeFile(t, filepath.Join(root, "agents/old/AGENT.md"), "---\ntitle: Research\n---\n\nResearch.\n")
	writeFile(t, filepath.Join(root, "agents/old/brief/JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "TREBI.md"), "---\nharness: claude\n---\n\nAssist.\n")
	svc = workspaces.New(st)
	ws, err := svc.Add(ctx, root, "rename")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := svc.JobByName(ctx, ws.ID, "agents/old")
	if err != nil {
		t.Fatal(err)
	}
	brief, err := svc.JobByName(ctx, ws.ID, "agents/old/brief")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO runs (id, workspace_id, job_id, kind, status, trace_id, started_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`, "run-old", ws.ID, agent.ID, "agent", "failed", "trace", now, now)
	exec(`INSERT INTO runs (id, workspace_id, job_id, kind, status, trace_id, started_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`, "run-owned", ws.ID, brief.ID, "job", "complete", "trace", now, now)
	exec(`INSERT INTO event_queue (id,connector,workspace_id,job_id,kind,status,prompt,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?)`, "eq", "tick", ws.ID, brief.ID, "job", "pending", "", now, now)
	exec(`INSERT INTO schedules (workspace_id,job_id,idx,spec,updated_at) VALUES (?,?,?,?,?)`, ws.ID, brief.ID, 0, "* * * * *", now)
	exec(`INSERT INTO email_messages (id,direction,workspace_id,agent_id,disposition,created_at) VALUES (?,?,?,?,?,?)`, "em", "inbound", ws.ID, agent.ID, "accepted", now)
	exec(`INSERT INTO conversations (id,workspace_id,key,title,origin_channel,operator_id,created_at,updated_at) VALUES ('conv',?,?,?,?,?,?,?)`,
		ws.ID, "conv-key", "Research", "email", agent.ID, now, now)
	return svc, ws.ID, root, agent.ID, brief.ID
}
