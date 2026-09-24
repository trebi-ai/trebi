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

func renameFixture(t *testing.T, ctx context.Context, st *store.Store) (*workspaces.Service, string, string) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "agents/old/AGENT.md"), "---\ntitle: Research\n---\n\nResearch.\n")
	writeFile(t, filepath.Join(root, "agents/old/brief/JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "TREBI.md"), "---\nharness: claude\n---\n\nAssist.\n")
	svc := workspaces.New(st)
	ws, err := svc.Add(ctx, root, "rename")
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
	exec(`INSERT INTO runs (id, workspace_id, job, kind, status, trace_id, started_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`, "run-old", ws.ID, "agents/old", "agent", "failed", "trace", now, now)
	exec(`INSERT INTO runs (id, workspace_id, job, kind, status, trace_id, started_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`, "run-owned", ws.ID, "agents/old/brief", "job", "complete", "trace", now, now)
	exec(`INSERT INTO event_queue (id,connector,workspace_id,job,kind,status,prompt,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?)`, "eq", "tick", ws.ID, "agents/old/brief", "job", "pending", "", now, now)
	exec(`INSERT INTO schedules (workspace_id,job,idx,spec,updated_at) VALUES (?,?,?,?,?)`, ws.ID, "agents/old/brief", 0, "* * * * *", now)
	exec(`INSERT INTO email_messages (id,direction,workspace_id,agent,disposition,created_at) VALUES (?,?,?,?,?,?)`, "em", "inbound", ws.ID, "old", "accepted", now)
	exec(`INSERT INTO conversations (id,workspace_id,key,title,origin_channel,operator,created_at,updated_at) VALUES ('conv',?,?,?,?,?,?,?)`,
		ws.ID, "conv-key", "Research", "email", "agents/old", now, now)
	return svc, ws.ID, root
}

func TestRenameAgentRewritesReferencesAndEmitsOnce(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	svc, wsID, root := renameFixture(t, ctx, st)
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

	var job string
	if err := st.DB().QueryRowContext(ctx, `SELECT job FROM runs WHERE id='run-owned'`).Scan(&job); err != nil {
		t.Fatal(err)
	}
	if job != "agents/new/brief" {
		t.Fatalf("owned run job=%q", job)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT job FROM event_queue WHERE id='eq'`).Scan(&job); err != nil || job != "agents/new/brief" {
		t.Fatalf("event_queue=%q %v", job, err)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT job FROM schedules WHERE workspace_id=? AND idx=0`, wsID).Scan(&job); err != nil || job != "agents/new/brief" {
		t.Fatalf("schedule=%q %v", job, err)
	}
	var agent string
	if err := st.DB().QueryRowContext(ctx, `SELECT agent FROM email_messages WHERE id='em'`).Scan(&agent); err != nil || agent != "new" {
		t.Fatalf("email agent=%q %v", agent, err)
	}
	var operator string
	if err := st.DB().QueryRowContext(ctx, `SELECT operator FROM conversations WHERE id='conv'`).Scan(&operator); err != nil || operator != "agents/new" {
		t.Fatalf("conversation operator=%q %v", operator, err)
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
		if row.Name == "agents/new" {
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
	if len(events) != 1 || events[0].Topic != "agent.renamed" {
		t.Fatalf("agent events=%+v", events)
	}
}

func TestRenameAgentConflictsAndRollsBackTransaction(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	svc, wsID, _ := renameFixture(t, ctx, st)

	if _, err := st.DB().ExecContext(ctx, `INSERT INTO runs (id,workspace_id,job,kind,status,trace_id,started_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`, "live", wsID, "agents/old", "agent", "running", "trace", time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
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
	svc2, ws2, root2 := renameFixture(t, ctx, st2)
	if _, err := st2.DB().ExecContext(ctx, `CREATE TRIGGER force_rename_failure BEFORE UPDATE ON runs WHEN new.job LIKE 'agents/new%' BEGIN SELECT RAISE(ABORT, 'forced'); END`); err != nil {
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
