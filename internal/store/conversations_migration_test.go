package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrationV27ChannelPointersWorkspace builds a v26 database with a
// pointer and an event row, then applies v27 (plan 2026-09-27 G.1). The
// pointer keeps its conversation, the key becomes workspace-scoped, and the
// new channel_outbox table exists.
func TestMigrationV27ChannelPointersWorkspace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v26.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyThrough(db, 26); err != nil {
		t.Fatalf("v26 setup: %v", err)
	}
	seed := []string{
		`INSERT INTO workspaces (id, name, path, git_enabled, orphaned, created_at, updated_at)
		 VALUES ('ws1', 'one', '/tmp/one', 0, 0, '2026-09-27T00:00:00Z', '2026-09-27T00:00:00Z')`,
		`INSERT INTO workspaces (id, name, path, git_enabled, orphaned, created_at, updated_at)
		 VALUES ('ws2', 'two', '/tmp/two', 0, 0, '2026-09-27T00:00:00Z', '2026-09-27T00:00:00Z')`,
		`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at, operator)
		 VALUES ('c1', 'ws1', 'k1', 'One', '2026-09-27T00:00:00Z', '2026-09-27T00:00:00Z', '')`,
		`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at, operator)
		 VALUES ('c2', 'ws2', 'k2', 'Two', '2026-09-27T00:00:00Z', '2026-09-27T00:00:00Z', '')`,
		`INSERT INTO channel_pointers (channel_type, channel_ref, conversation_id, updated_at)
		 VALUES ('telegram', 'chat1', 'c1', '2026-09-27T00:00:01Z')`,
		`INSERT INTO events (id, connector, received_at, summary, raw_json, trace_id, source_id)
		 VALUES ('e1', 'tick', '2026-09-27T00:00:01Z', 'kept', '{}', 't1', 'src1')`,
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
		t.Fatalf("open applies v27: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if v, _ := s.SchemaVersion(); v != SchemaVersion {
		t.Fatalf("schema version %d", v)
	}
	var conv string
	if err := s.DB().QueryRow(`SELECT conversation_id FROM channel_pointers
		WHERE workspace_id='ws1' AND channel_type='telegram' AND channel_ref='chat1'`).Scan(&conv); err != nil {
		t.Fatalf("pointer kept: %v", err)
	}
	if conv != "c1" {
		t.Fatalf("pointer conversation = %q, want c1", conv)
	}
	// The key is workspace-scoped, so the second workspace holds the same pair.
	if _, err := s.DB().Exec(`INSERT INTO channel_pointers
		(workspace_id, channel_type, channel_ref, conversation_id, updated_at)
		VALUES ('ws2', 'telegram', 'chat1', 'c2', '2026-09-27T00:00:02Z')`); err != nil {
		t.Fatalf("second workspace pointer: %v", err)
	}
	var src string
	if err := s.DB().QueryRow(`SELECT source_id FROM events WHERE id='e1'`).Scan(&src); err != nil {
		t.Fatalf("events row kept: %v", err)
	}
	if src != "src1" {
		t.Fatalf("source_id = %q, want src1", src)
	}
	if _, err := s.DB().Exec(`INSERT INTO channel_outbox
		(id, connector, workspace_id, conversation_id, channel_ref, message_id, text, status, created_at)
		VALUES ('o1', 'telegram', 'ws1', 'c1', 'chat1', 'm1', 'hi', 'pending', '2026-09-27T00:00:03Z')`); err != nil {
		t.Fatalf("channel_outbox exists: %v", err)
	}
}

// TestMigrationV25BackfillsTitles builds a v24 database, then applies v25
// and checks the three outcomes of plan 2026-09-26 E: an untitled
// conversation gains the first user line, a named one keeps its title, and
// an untitled one with no user rows stays untitled.
func TestMigrationV25BackfillsTitles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v24.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// v24 is the last version before the title backfill; later versions
	// (v26+) must not change its outcome.
	if err := applyThrough(db, 24); err != nil {
		t.Fatalf("v24 setup: %v", err)
	}
	long := ""
	for range 70 {
		long += "x"
	}
	seed := []string{
		`INSERT INTO workspaces (id, name, path, git_enabled, orphaned, created_at, updated_at)
		 VALUES ('ws1', 'sling', '/tmp/sling', 0, 0, '2026-09-26T00:00:00Z', '2026-09-26T00:00:00Z')`,
		// Untitled with user rows: the first line wins, later rows never run.
		`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at, operator)
		 VALUES ('c1', 'ws1', 'k1', '', '2026-09-26T00:00:00Z', '2026-09-26T00:00:00Z', '')`,
		`INSERT INTO conversation_messages (id, conversation_id, role, text, created_at)
		 VALUES ('m1', 'c1', 'user', 'What is the queue depth?', '2026-09-26T00:00:01Z')`,
		`INSERT INTO conversation_messages (id, conversation_id, role, text, created_at)
		 VALUES ('m2', 'c1', 'assistant', 'It is zero.', '2026-09-26T00:00:02Z')`,
		`INSERT INTO conversation_messages (id, conversation_id, role, text, created_at)
		 VALUES ('m3', 'c1', 'user', 'Thanks', '2026-09-26T00:00:03Z')`,
		// Titled: never overwritten.
		`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at, operator)
		 VALUES ('c2', 'ws1', 'k2', 'Custom', '2026-09-26T00:00:00Z', '2026-09-26T00:00:00Z', '')`,
		`INSERT INTO conversation_messages (id, conversation_id, role, text, created_at)
		 VALUES ('m4', 'c2', 'user', 'A question', '2026-09-26T00:00:01Z')`,
		// Untitled with no user rows: stays untitled.
		`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at, operator)
		 VALUES ('c3', 'ws1', 'k3', '', '2026-09-26T00:00:00Z', '2026-09-26T00:00:00Z', '')`,
		// Untitled with a 70-character first line: 60 characters plus the ellipsis.
		`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at, operator)
		 VALUES ('c4', 'ws1', 'k4', '', '2026-09-26T00:00:00Z', '2026-09-26T00:00:00Z', '')`,
		`INSERT INTO conversation_messages (id, conversation_id, role, text, created_at)
		 VALUES ('m5', 'c4', 'user', '` + long + `', '2026-09-26T00:00:01Z')`,
		// Multi-line first message: the first line only.
		`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at, operator)
		 VALUES ('c5', 'ws1', 'k5', '', '2026-09-26T00:00:00Z', '2026-09-26T00:00:00Z', '')`,
		`INSERT INTO conversation_messages (id, conversation_id, role, text, created_at)
		 VALUES ('m6', 'c5', 'user', 'Header line` + "\n" + `body line', '2026-09-26T00:00:01Z')`,
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
		t.Fatalf("open applies v25: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if v, _ := s.SchemaVersion(); v != SchemaVersion {
		t.Fatalf("schema version %d", v)
	}
	want := map[string]string{
		"c1": "What is the queue depth?",
		"c2": "Custom",
		"c3": "",
		"c4": long[:60] + "\u2026",
		"c5": "Header line",
	}
	for id, title := range want {
		var got string
		if err := s.DB().QueryRow(`SELECT title FROM conversations WHERE id=?`, id).Scan(&got); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got != title {
			t.Fatalf("%s title = %q, want %q", id, got, title)
		}
	}
}
