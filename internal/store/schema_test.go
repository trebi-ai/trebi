package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// TestMigrationV42IDs proves that v42 stops on rows with no ID and changes
// nothing, and that it drops the name columns once the IDs are filled.
func TestMigrationV42IDs(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v41.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyThrough(db, 41); err != nil {
		t.Fatalf("v41 setup: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO job_index (workspace_id,name,path,updated_at) VALUES ('ws','daily','/j','x')`,
		`INSERT INTO runs (id,workspace_id,job,status,trace_id,started_at,updated_at) VALUES ('R','ws','daily','complete','t','x','x')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := migrate(db); !errors.Is(err, ErrIDMigration) {
		t.Fatalf("migrate with no IDs = %v, want ErrIDMigration", err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 41 {
		t.Fatalf("version = %d, %v; want 41", version, err)
	}

	for _, q := range []string{
		`UPDATE job_index SET id='jb_daily'`,
		`UPDATE runs SET job_id='jb_daily'`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate with IDs: %v", err)
	}
	for _, tc := range []struct{ table, column string }{{"runs", "job"}, {"job_index", "owner"}} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, tc.table, tc.column).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s.%s still exists (%d, %v)", tc.table, tc.column, n, err)
		}
	}
	var idx int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('idx_job_index_name','idx_job_index_owner')`).Scan(&idx); err != nil || idx != 2 {
		t.Fatalf("indexes = %d, %v; want 2", idx, err)
	}
	var jobID string
	if err := db.QueryRow(`SELECT job_id FROM runs WHERE id='R'`).Scan(&jobID); err != nil || jobID != "jb_daily" {
		t.Fatalf("runs.job_id = %q, %v", jobID, err)
	}
}

// TestMigrationV45Forms moves each request into a request with one form. Of
// two pending requests on one run, only the newest stays open.
func TestMigrationV45Forms(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v44.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyThrough(db, 44); err != nil {
		t.Fatalf("v44 setup: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO requests (id,run_id,workspace_id,job_id,kind,type,title,payload_json,allow_json,status,answer_json,created_at,updated_at)
		 VALUES ('A','R','ws','jb','job','confirm','Ship?','{}','["approve","reject"]','answered','{"decision":"approve","note":"ok","by":"user","via":"ui","at":"t"}','t','t')`,
		`INSERT INTO requests (id,run_id,workspace_id,job_id,kind,type,title,payload_json,allow_json,status,created_at,updated_at)
		 VALUES ('B','R','ws','jb','job','choose','Which?','{"options":[{"id":"a","label":"A"}]}','["choose"]','pending','t','t')`,
		`INSERT INTO requests (id,run_id,workspace_id,job_id,kind,type,title,payload_json,allow_json,status,created_at,updated_at)
		 VALUES ('C','R','ws','jb','job','confirm','Again?','{}','["approve"]','pending','t','t')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	type form struct {
		typ, title string
		answer     sql.NullString
	}
	got := map[string]form{}
	rows, err := db.Query(`SELECT request_id, type, title, answer_json FROM request_forms WHERE idx = 0`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var f form
		if err := rows.Scan(&id, &f.typ, &f.title, &f.answer); err != nil {
			t.Fatal(err)
		}
		got[id] = f
	}
	if len(got) != 3 || got["A"].typ != "confirm" || !strings.Contains(got["A"].answer.String, `"approve"`) || got["B"].answer.Valid {
		t.Fatalf("forms = %+v", got)
	}
	status := map[string]string{}
	srows, err := db.Query(`SELECT id, status FROM requests`)
	if err != nil {
		t.Fatal(err)
	}
	defer srows.Close()
	for srows.Next() {
		var id, st string
		if err := srows.Scan(&id, &st); err != nil {
			t.Fatal(err)
		}
		status[id] = st
	}
	if status["A"] != "answered" || status["B"] != "cancelled" || status["C"] != "pending" {
		t.Fatalf("status = %v", status)
	}
	if _, err := db.Exec(`INSERT INTO requests (id,run_id,workspace_id,job_id,kind,title,created_at,updated_at) VALUES ('D','R','ws','jb','job','x','t','t')`); err == nil {
		t.Fatal("a second open request on one run was accepted")
	}
}

// TestMigrationV50Artifacts adds the artifact tables and the new email and
// schedule columns. An old email row reads the defaults.
func TestMigrationV50Artifacts(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v49.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyThrough(db, 49); err != nil {
		t.Fatalf("v49 setup: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO email_messages (id, direction, disposition, created_at) VALUES ('m1', 'out', 'sent', 't')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var artifactID, snapshotID, decisionID string
	var version sql.NullInt64
	if err := db.QueryRow(`SELECT artifact_id, artifact_version, snapshot_id, decision_id FROM email_messages WHERE id='m1'`).
		Scan(&artifactID, &version, &snapshotID, &decisionID); err != nil {
		t.Fatal(err)
	}
	if artifactID != "" || version.Valid || snapshotID != "" || decisionID != "" {
		t.Fatalf("defaults %q %v %q %q", artifactID, version, snapshotID, decisionID)
	}
	for _, q := range []string{
		`INSERT INTO artifacts (id, workspace_id, owner_kind, name, title, kind, latest_version, created_at, updated_at) VALUES ('a1', 'w', 'job', 'r', 'R', 'html', 1, 't', 't')`,
		`INSERT INTO artifact_versions (artifact_id, n, sha256, size_bytes, mime, file_name, origin_kind, created_at) VALUES ('a1', 1, 'x', 1, 'text/html', 'r.html', 'run', 't')`,
		`INSERT INTO artifact_snapshots (id, artifact_id, version_n, reason, created_at) VALUES ('s1', 'a1', 1, 'refresh', 't')`,
		`INSERT INTO decisions (id, workspace_id, kind, kind_version, subject_kind, subject_id, decider, method, status, outcome, questions_json, took_ms, created_at) VALUES ('d1', 'w', 'k', 1, 'email', 'm1', 'fake', 'rule', 'ok', 'deliver', '[]', 1, 't')`,
		`SELECT artifact_id FROM schedules`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO artifacts (id, workspace_id, owner_kind, name, title, kind, latest_version, created_at, updated_at) VALUES ('a2', 'w', 'job', 'r', 'R', 'html', 1, 't', 't')`); err == nil {
		t.Fatal("a second artifact with the same key was accepted")
	}
}

// TestMigrationV51JobFolder adds job_index.folder; an old row reads "".
func TestMigrationV51JobFolder(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v50.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyThrough(db, 50); err != nil {
		t.Fatalf("v50 setup: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO job_index (id, workspace_id, name, path, kind, active, owner_id, updated_at) VALUES ('jb_1', 'w', 'a', '/a', 'job', 1, '', 't')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var folder string
	if err := db.QueryRow(`SELECT folder FROM job_index WHERE id='jb_1'`).Scan(&folder); err != nil {
		t.Fatal(err)
	}
	if folder != "" {
		t.Fatalf("folder=%q want empty", folder)
	}
}

// TestMigrationV52InboxState splits status into read_at, done_at, and
// done_by, and drops status and resolved_at.
func TestMigrationV52InboxState(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v51.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyThrough(db, 51); err != nil {
		t.Fatalf("v51 setup: %v", err)
	}
	const ins = `INSERT INTO inbox_items (id, kind, status, created_at, updated_at, resolved_at) VALUES (?, 'health', ?, 'c', ?, ?)`
	for _, r := range [][]any{
		{"u", "unread", "t1", nil},
		{"r", "read", "t2", nil},
		{"d", "resolved", "t3", "t4"},
		{"d2", "resolved", "t5", nil},
	} {
		if _, err := db.Exec(ins, r...); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	want := map[string][3]string{
		"u":  {"", "", ""},
		"r":  {"t2", "", ""},
		"d":  {"t3", "t4", "system"},
		"d2": {"t5", "t5", "system"},
	}
	for id, w := range want {
		var readAt, doneAt, doneBy sql.NullString
		if err := db.QueryRow(`SELECT read_at, done_at, done_by FROM inbox_items WHERE id=?`, id).Scan(&readAt, &doneAt, &doneBy); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got := [3]string{readAt.String, doneAt.String, doneBy.String}; got != w {
			t.Fatalf("%s: got %q want %q", id, got, w)
		}
	}
	for _, col := range []string{"status", "resolved_at"} {
		if _, err := db.Exec(`SELECT ` + col + ` FROM inbox_items`); err == nil {
			t.Fatalf("column %s still exists", col)
		}
	}
}

// TestMigrationV53ConnectionStats backfills the lifetime count of each
// connection with the same numbers as COUNT(*) on events.
func TestMigrationV53ConnectionStats(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v52.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyThrough(db, 52); err != nil {
		t.Fatalf("v52 setup: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO events (id, connector, instance, received_at, summary, raw_json, trace_id) VALUES ('e1', 'x', 'c1', '2026-10-01T00:00:00Z', '', '{}', 't')`,
		`INSERT INTO events (id, connector, instance, received_at, summary, raw_json, trace_id) VALUES ('e2', 'x', 'c1', '2026-10-03T00:00:00Z', '', '{}', 't')`,
		`INSERT INTO events (id, connector, instance, received_at, summary, raw_json, trace_id) VALUES ('e3', 'x', 'c1', '2026-10-02T00:00:00Z', '', '{}', 't')`,
		`INSERT INTO events (id, connector, instance, received_at, summary, raw_json, trace_id) VALUES ('e4', 'y', 'c2', '2026-10-05T00:00:00Z', '', '{}', 't')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	want := map[string]int64{}
	rows, err := db.Query(`SELECT instance, COUNT(*) FROM events GROUP BY instance`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		want[id] = n
	}
	_ = rows.Close() // read-only query
	for id, n := range want {
		var total int64
		var first, last string
		if err := db.QueryRow(`SELECT events_total, first_event_at, last_event_at FROM connection_stats WHERE instance=?`, id).
			Scan(&total, &first, &last); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if total != n {
			t.Fatalf("%s total=%d want %d", id, total, n)
		}
		if id == "c1" && (first != "2026-10-01T00:00:00Z" || last != "2026-10-03T00:00:00Z") {
			t.Fatalf("c1 first=%q last=%q", first, last)
		}
	}
}

// TestMigrationV54DecisionTokens adds the token columns to decisions with 0
// for an old row.
func TestMigrationV54DecisionTokens(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v53.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyThrough(db, 53); err != nil {
		t.Fatalf("v53 setup: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO decisions (id, workspace_id, kind, kind_version, subject_kind, subject_id, decider,
		method, status, outcome, questions_json, took_ms, cost_usd, created_at)
		VALUES ('d1', 'w', 'k', 1, 'email', 's', 'harness:claude', 'verbal', 'ok', 'deliver', '[]', 5, 0.01, 't')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var in, out, cr, cc int64
	var cost float64
	if err := db.QueryRow(`SELECT tokens_input, tokens_output, tokens_cache_read, tokens_cache_creation, cost_usd
		FROM decisions WHERE id='d1'`).Scan(&in, &out, &cr, &cc, &cost); err != nil {
		t.Fatal(err)
	}
	if in != 0 || out != 0 || cr != 0 || cc != 0 || cost != 0.01 {
		t.Fatalf("tokens %d/%d/%d/%d cost %v", in, out, cr, cc, cost)
	}
}

// TestMigrationV69PrunedAt adds runs.pruned_at, turns on incremental vacuum,
// and keeps only the two newest backups.
func TestMigrationV69PrunedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v68.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyThrough(db, 68); err != nil {
		t.Fatalf("v68 setup: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int{9, 65, 67, 66} {
		if err := os.WriteFile(fmt.Sprintf("%s.v%d.bak", path, v), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	other := path + ".vx.bak"
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := OpenPath(path)
	if err != nil {
		t.Fatalf("open applies v69: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.DB().Exec(`SELECT pruned_at FROM runs`); err != nil {
		t.Fatalf("pruned_at: %v", err)
	}
	var mode int
	if err := s.DB().QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode != 2 {
		t.Fatalf("auto_vacuum = %d, %v; want 2 (incremental)", mode, err)
	}
	var limit int64
	if err := s.DB().QueryRow(`PRAGMA journal_size_limit`).Scan(&limit); err != nil || limit != 64<<20 {
		t.Fatalf("journal_size_limit = %d, %v; want 64 MiB", limit, err)
	}
	if err := s.IncrementalVacuum(t.Context()); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(path + ".v*.bak") //nolint:errcheck // a bad pattern is a test bug
	want := []string{path + ".v67.bak", path + ".v68.bak", other}
	slices.Sort(left)
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Fatalf("backups = %v, want %v", left, want)
	}
}
