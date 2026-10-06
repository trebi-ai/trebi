package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationV32Handles derives a handle for each workspace and drops the
// old addresses, so the daemon mints <handle>-<token> again.
func TestMigrationV32Handles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v31.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyThrough(db, 31); err != nil {
		t.Fatalf("v31 setup: %v", err)
	}
	seed := []string{
		`INSERT INTO workspaces (id, name, path, created_at, updated_at) VALUES
		 ('w1', 'Sling', '/tmp/a', 't', 't'),
		 ('w2', 'my_big.project name that is long', '/tmp/b', 't', 't'),
		 ('w3', 'Über', '/tmp/c', 't', 't'),
		 ('w4', 'x', '/tmp/d', 't', 't')`,
		`INSERT INTO email_addresses (workspace_id, local_part, domain, created_at)
		 VALUES ('w1', 'a-01m3544c43t5p7x378f2', 'in.example.com', 't')`,
		`INSERT INTO cloud_addresses (id, workspace_id, address, local_part, created_at)
		 VALUES ('c1', 'w1', 'a-01m3544c43t5p7x378f2@in.example.com', 'a-01m3544c43t5p7x378f2', 't')`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v (%s)", err, q)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenPath(path)
	if err != nil {
		t.Fatalf("open applies v32: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	want := map[string]string{"w1": "sling", "w2": "my-big-project-name", "w3": "ws", "w4": "ws"}
	for id, h := range want {
		var got string
		if err := s.DB().QueryRow(`SELECT handle FROM workspaces WHERE id=?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != h {
			t.Errorf("handle %s = %q, want %q", id, got, h)
		}
	}
	for _, table := range []string{"email_addresses", "cloud_addresses"} {
		var n int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s rows = %d, %v; want 0", table, n, err)
		}
	}
}

// TestMigrationV36Conversations adds the conversation columns to a v35 DB.
// The unique source index serves the parent lookup.
func TestMigrationV36Conversations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v35.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyThrough(db, 35); err != nil {
		t.Fatalf("v35 setup: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO events (id, connector, instance, received_at, trace_id, source_id)
		VALUES ('e1', 'discord', 'c1', 't', 'tr', 's1')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenPath(path)
	if err != nil {
		t.Fatalf("open applies v36: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var root sql.NullString
	if err := s.DB().QueryRow(`SELECT COALESCE(root, source_id) FROM events WHERE instance='c1' AND source_id='s1'`).Scan(&root); err != nil || root.String != "s1" {
		t.Fatalf("root = %v, %v", root, err)
	}
	var followUp int
	if err := s.DB().QueryRow(`SELECT COUNT(follow_up) FROM event_queue`).Scan(&followUp); err != nil {
		t.Fatalf("follow_up: %v", err)
	}
	var id, parent, notused int
	var detail string
	if err := s.DB().QueryRow(`EXPLAIN QUERY PLAN SELECT COALESCE(root, source_id) FROM events WHERE instance=? AND source_id=?`, "c1", "s1").
		Scan(&id, &parent, &notused, &detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "idx_events_source") {
		t.Fatalf("parent lookup plan = %q", detail)
	}
}

// TestMigrationV38 merges each resume chain with one session into its first
// run: the chain runs become turns, and their child rows move to the root.
func TestMigrationV38(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v37.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyThrough(db, 37); err != nil {
		t.Fatalf("v37 setup: %v", err)
	}
	seed := []string{
		// A -> B -> C share session s1. F is a Fresh run after C. L is alone.
		`INSERT INTO runs (id,workspace_id,job,status,trace_id,session_id,reason,started_at,ended_at,updated_at,tokens_input,first_run_id)
			VALUES ('A','ws','j','resumed','t','s1','manual','2026-09-01T10:00:00Z','2026-09-01T10:00:10Z','x',100,'A')`,
		`INSERT INTO runs (id,workspace_id,job,status,trace_id,session_id,reason,resumed_from,started_at,ended_at,updated_at,tokens_input,first_run_id)
			VALUES ('B','ws','j','resumed','t','s1','resume','A','2026-09-01T11:00:00Z','2026-09-01T11:00:20Z','x',20,'A')`,
		`INSERT INTO runs (id,workspace_id,job,status,end_reason,trace_id,session_id,reason,resumed_from,started_at,ended_at,updated_at,tokens_input,first_run_id)
			VALUES ('C','ws','j','complete','result','t','s1','request','B','2026-09-01T12:00:00Z','2026-09-01T12:00:30Z','x',3,'A')`,
		`INSERT INTO runs (id,workspace_id,job,status,trace_id,session_id,reason,resumed_from,started_at,ended_at,updated_at,first_run_id)
			VALUES ('F','ws','j','complete','t','s2','manual','C','2026-09-01T13:00:00Z','2026-09-01T13:00:01Z','x','A')`,
		`INSERT INTO runs (id,workspace_id,job,status,trace_id,session_id,started_at,updated_at,first_run_id)
			VALUES ('L','ws','j','complete','t','s9','2026-09-02T10:00:00Z','x','L')`,
		`INSERT INTO run_events (run_id,at,kind) VALUES ('A','a','user'), ('B','b','user'), ('C','c','user')`,
		`INSERT INTO requests (id,run_id,workspace_id,job,kind,type,title,allow_json,created_at,updated_at,delivered_run_id)
			VALUES ('R1','B','ws','j','job','approval','t','[]','x','x','C')`,
		`INSERT INTO approvals (id,run_id,created_at) VALUES ('P1','C','x')`,
		`INSERT INTO conversations (id,workspace_id,key,created_at,updated_at) VALUES ('cv','ws','k','x','x')`,
		// B and C repeat the transcript of A, so their first messages are duplicates.
		`INSERT INTO conversation_messages (id,conversation_id,role,text,run_id,turn_index,created_at) VALUES
			('m1','cv','user','hi','A',0,'x'), ('m2','cv','user','hi','B',0,'x'), ('m3','cv','user','more','B',1,'x'),
			('m4','cv','user','hi','C',0,'x'), ('m5','cv','user','more','C',1,'x'), ('m6','cv','user','last','C',2,'x')`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	// v42 needs the ID script, so stop before it.
	if err := applyThrough(db, 40); err != nil {
		t.Fatalf("apply v38: %v", err)
	}
	q := db

	ids := scanStrings(t, q, `SELECT id FROM runs ORDER BY id`)
	if strings.Join(ids, ",") != "A,F,L" {
		t.Fatalf("runs %v", ids)
	}
	var turns, active int
	var status, end, prev, session string
	var tokens int64
	if err := q.QueryRow(`SELECT turn_count, active_ms, status, COALESCE(end_reason,''), COALESCE(previous_run_id,''),
		session_id, tokens_input FROM runs WHERE id='A'`).Scan(&turns, &active, &status, &end, &prev, &session, &tokens); err != nil {
		t.Fatal(err)
	}
	if turns != 3 || active != 60_000 || status != "complete" || end != "result" || prev != "" || session != "s1" || tokens != 123 {
		t.Fatalf("root turns %d active %d status %s/%s prev %q session %s tokens %d", turns, active, status, end, prev, session, tokens)
	}
	if got := scanStrings(t, q, `SELECT n || ':' || reason || ':' || status FROM run_turns WHERE run_id='A' ORDER BY n`); strings.Join(got, ",") != "1:manual:complete,2:resume:complete,3:request:complete" {
		t.Fatalf("root turns %v", got)
	}
	if err := q.QueryRow(`SELECT previous_run_id FROM runs WHERE id='F'`).Scan(&prev); err != nil || prev != "A" {
		t.Fatalf("fresh run previous %q err %v", prev, err)
	}
	for name, query := range map[string]string{
		"events":    `SELECT COUNT(*) FROM run_events WHERE run_id != 'A'`,
		"requests":  `SELECT COUNT(*) FROM requests WHERE run_id != 'A' OR delivered_run_id != 'A'`,
		"approvals": `SELECT COUNT(*) FROM approvals WHERE run_id != 'A'`,
		"messages":  `SELECT COUNT(*) FROM conversation_messages WHERE run_id != 'A'`,
	} {
		var n int
		if err := q.QueryRow(query).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s left on old runs: %d %v", name, n, err)
		}
	}
	if got := scanStrings(t, q, `SELECT text FROM conversation_messages ORDER BY turn_index`); strings.Join(got, ",") != "hi,more,last" {
		t.Fatalf("messages %v", got)
	}
	if got := scanStrings(t, q, `SELECT root || ':' || old FROM v38_log_merge ORDER BY n`); strings.Join(got, ",") != "A:B,A:C" {
		t.Fatalf("log merge %v", got)
	}
}

// TestMergeRunLogs appends the old logs to the root log in turn order.
func TestMergeRunLogs(t *testing.T) {
	s, err := OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	dir := t.TempDir()
	for name, body := range map[string]string{"A.log": "a\n", "C.log": "c\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec(`INSERT INTO v38_log_merge VALUES ('A','C',3), ('A','B',2)`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.MergeRunLogs(ctx, dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "A.log"))
	if err != nil || string(got) != "a\nc\n" {
		t.Fatalf("root log %q err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "C.log")); !os.IsNotExist(err) {
		t.Fatalf("old log kept: %v", err)
	}
	// The table is gone, so a second call does nothing.
	if err := s.MergeRunLogs(ctx, dir); err != nil {
		t.Fatal(err)
	}
}

func scanStrings(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}
