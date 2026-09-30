package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrationV27ChannelPointersWorkspace builds a v26 database with a
// pointer and an event row, then applies v27 (plan 2026-09-27 G.1). The
// pointer keeps its conversation, and the key becomes workspace-scoped.
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
	t.Cleanup(func() { _ = db.Close() })
	if err := applyThrough(db, 32); err != nil {
		t.Fatalf("apply v32: %v", err)
	}
	var conv string
	if err := db.QueryRow(`SELECT conversation_id FROM channel_pointers
		WHERE workspace_id='ws1' AND connector='telegram' AND instance='telegram' AND room='chat1'`).Scan(&conv); err != nil {
		t.Fatalf("pointer kept: %v", err)
	}
	if conv != "c1" {
		t.Fatalf("pointer conversation = %q, want c1", conv)
	}
	// The key is workspace-scoped, so the second workspace holds the same pair.
	if _, err := db.Exec(`INSERT INTO channel_pointers
		(workspace_id, connector, instance, room, thread, conversation_id, updated_at)
		VALUES ('ws2', 'telegram', 'telegram', 'chat1', '', 'c2', '2026-09-27T00:00:02Z')`); err != nil {
		t.Fatalf("second workspace pointer: %v", err)
	}
	var src string
	if err := db.QueryRow(`SELECT source_id FROM events WHERE id='e1'`).Scan(&src); err != nil {
		t.Fatalf("events row kept: %v", err)
	}
	if src != "src1" {
		t.Fatalf("source_id = %q, want src1", src)
	}
}

// TestMigrationV29Outbox builds a v28 database with channel_outbox rows and
// a `sending` envelope, then applies v29 (plan 2026-09-28 A.1). Each channel
// row moves to outbox, a sent row drops its body, and the stale envelope
// becomes send_failed.
func TestMigrationV29Outbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v28.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyThrough(db, 28); err != nil {
		t.Fatalf("v28 setup: %v", err)
	}
	seed := []string{
		`INSERT INTO channel_outbox
		(id, connector, workspace_id, conversation_id, channel_ref, message_id, text, status, attempts, error_message, created_at)
		VALUES ('o1', 'telegram', 'ws1', 'c1', 'chat1', 'm1', 'hi', 'failed', 5, 'boom', '2026-09-28T00:00:00Z')`,
		`INSERT INTO channel_outbox
		(id, connector, workspace_id, conversation_id, channel_ref, message_id, text, status, attempts, created_at, sent_at)
		VALUES ('o2', 'telegram', 'ws1', 'c1', 'chat1', 'm2', 'done', 'sent', 1, '2026-09-28T00:00:00Z', '2026-09-28T00:00:01Z')`,
		`INSERT INTO email_messages (id, direction, message_id, disposition, created_at)
		VALUES ('e1', 'out', '<a@x>', 'sending', '2026-09-28T00:00:00Z')`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v (%s)", err, q)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyThrough(db, 32); err != nil {
		t.Fatalf("apply v32: %v", err)
	}

	var kind, purpose, target, dedupe, status, errMsg string
	var body sql.NullString
	var attempts int
	if err := db.QueryRow(`SELECT kind, purpose, room, dedupe_key, body, status, attempts, error_message
		FROM outbox WHERE id='o1'`).Scan(&kind, &purpose, &target, &dedupe, &body, &status, &attempts, &errMsg); err != nil {
		t.Fatal(err)
	}
	if kind != "channel" || purpose != "reply" || target != "chat1" || dedupe != "m1" ||
		body.String != "hi" || status != "failed" || attempts != 5 || errMsg != "boom" {
		t.Errorf("o1 = %s %s %s %s %q %s %d %s", kind, purpose, target, dedupe, body.String, status, attempts, errMsg)
	}
	if err := db.QueryRow(`SELECT body FROM outbox WHERE id='o2'`).Scan(&body); err != nil || body.Valid {
		t.Errorf("sent row body = %v, %v; want NULL", body, err)
	}
	if _, err := db.Exec(`SELECT 1 FROM channel_outbox`); err == nil {
		t.Error("channel_outbox still exists")
	}
	var disp string
	if err := db.QueryRow(`SELECT disposition FROM email_messages WHERE id='e1'`).Scan(&disp); err != nil || disp != "send_failed" {
		t.Errorf("envelope disposition = %q, %v; want send_failed", disp, err)
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

// TestMigrationV30Rooms builds a v29 database with email and telegram
// rows, then applies v30. An email pointer moves to the mailbox room and
// keeps its thread. An email outbox target moves to recipients.
func TestMigrationV30Rooms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v29.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyThrough(db, 29); err != nil {
		t.Fatalf("v29 setup: %v", err)
	}
	seed := []string{
		`INSERT INTO channel_pointers (workspace_id, channel_type, channel_ref, conversation_id, updated_at)
		 VALUES ('ws1', 'email', '<root@x>', 'c1', '2026-09-28T00:00:00Z')`,
		`INSERT INTO channel_pointers (workspace_id, channel_type, channel_ref, conversation_id, updated_at)
		 VALUES ('ws1', 'telegram', 'chat1', 'c2', '2026-09-28T00:00:00Z')`,
		`INSERT INTO outbox (id, kind, purpose, workspace_id, connector, target, status, created_at)
		 VALUES ('o1', 'email', 'reply', 'ws1', 'email', 'a@x.com', 'queued', '2026-09-28T00:00:00Z')`,
		`INSERT INTO outbox (id, kind, purpose, workspace_id, connector, target, status, created_at)
		 VALUES ('o2', 'channel', 'reply', 'ws1', 'telegram', 'chat1', 'queued', '2026-09-28T00:00:00Z')`,
		`INSERT INTO connector_cursors (name, last_event_id, updated_at) VALUES ('tick', 'e9', '2026-09-28T00:00:00Z')`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v (%s)", err, q)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyThrough(db, 32); err != nil {
		t.Fatalf("apply v32: %v", err)
	}

	var room, thread string
	if err := db.QueryRow(`SELECT room, thread FROM channel_pointers
		WHERE instance='email'`).Scan(&room, &thread); err != nil || room != "ws1" || thread != "<root@x>" {
		t.Errorf("email pointer = %q %q, %v", room, thread, err)
	}
	if err := db.QueryRow(`SELECT room, thread FROM channel_pointers
		WHERE instance='telegram'`).Scan(&room, &thread); err != nil || room != "chat1" || thread != "" {
		t.Errorf("telegram pointer = %q %q, %v", room, thread, err)
	}
	var instance, recipients string
	if err := db.QueryRow(`SELECT instance, room, recipients FROM outbox WHERE id='o1'`).
		Scan(&instance, &room, &recipients); err != nil || instance != "email" || room != "" || recipients != "a@x.com" {
		t.Errorf("email outbox = %q %q %q, %v", instance, room, recipients, err)
	}
	if err := db.QueryRow(`SELECT room, recipients FROM outbox WHERE id='o2'`).
		Scan(&room, &recipients); err != nil || room != "chat1" || recipients != "" {
		t.Errorf("channel outbox = %q %q, %v", room, recipients, err)
	}
	var last string
	if err := db.QueryRow(`SELECT last_event_id FROM connector_cursors WHERE instance='tick'`).
		Scan(&last); err != nil || last != "e9" {
		t.Errorf("connector cursor = %q, %v", last, err)
	}
}

// TestMigrationV33Connections builds a v32 database with native and
// non-native connector rows, then applies v33. The native rows stay, the
// other rows go, and connector_cursors is gone.
func TestMigrationV33Connections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v32.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyThrough(db, 32); err != nil {
		t.Fatalf("v32 setup: %v", err)
	}
	seed := []string{
		`INSERT INTO events (id, connector, instance, received_at, trace_id) VALUES ('e1', 'email', 'email', '2026-09-30T00:00:00Z', 't1')`,
		`INSERT INTO events (id, connector, instance, received_at, trace_id) VALUES ('e2', 'whatsapp', 'whatsapp', '2026-09-30T00:00:00Z', 't2')`,
		`INSERT INTO outbox (id, kind, purpose, workspace_id, instance, status, created_at)
		 VALUES ('o1', 'email', 'reply', 'ws1', 'email', 'queued', '2026-09-30T00:00:00Z')`,
		`INSERT INTO outbox (id, kind, purpose, workspace_id, instance, status, created_at)
		 VALUES ('o2', 'channel', 'reply', 'ws1', 'whatsapp', 'queued', '2026-09-30T00:00:00Z')`,
		`INSERT INTO connector_cursors (instance, last_event_id, updated_at) VALUES ('whatsapp', 'e9', '2026-09-30T00:00:00Z')`,
		`INSERT INTO connector_installs (name, version, installed_at) VALUES ('whatsapp', '0.6.0', '2026-09-30T00:00:00Z')`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v (%s)", err, q)
		}
	}
	if err := applyThrough(db, 33); err != nil {
		t.Fatalf("apply v33: %v", err)
	}
	for _, c := range []struct {
		q    string
		want int
	}{
		{`SELECT COUNT(*) FROM events WHERE id='e1'`, 1},
		{`SELECT COUNT(*) FROM events WHERE id='e2'`, 0},
		{`SELECT COUNT(*) FROM outbox WHERE id='o1'`, 1},
		{`SELECT COUNT(*) FROM outbox WHERE id='o2'`, 0},
		{`SELECT COUNT(*) FROM connector_installs`, 0},
	} {
		var n int
		if err := db.QueryRow(c.q).Scan(&n); err != nil || n != c.want {
			t.Errorf("%s = %d, %v; want %d", c.q, n, err, c.want)
		}
	}
	if _, err := db.Exec(`SELECT 1 FROM connector_cursors`); err == nil {
		t.Error("connector_cursors still exists")
	}
	if _, err := db.Exec(`INSERT INTO connector_installs (type, version, installed_at) VALUES ('whatsapp', '0.7.0', 'x')`); err != nil {
		t.Errorf("connector_installs keys on type: %v", err)
	}
}
