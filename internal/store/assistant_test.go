package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestMigrationV24FromV23 builds a v23 database with assistant leftovers,
// then applies v24 and checks the new tables, the clean cutoff, and the
// dropped column.
func TestMigrationV24FromV23(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v23.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyThrough(db, 23); err != nil {
		t.Fatalf("v23 setup: %v", err)
	}
	seed := []string{
		// v1 objects the v24 statements touch.
		`INSERT INTO workspaces (id, name, path, git_enabled, orphaned, assistant_seeded, created_at, updated_at)
		 VALUES ('ws1', 'sling', '/tmp/sling', 0, 0, 1, '2026-09-25T00:00:00Z', '2026-09-25T00:00:00Z')`,
		`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at, operator)
		 VALUES ('c1', 'ws1', 'k', '', '2026-09-25T00:00:00Z', '2026-09-25T00:00:00Z', '')`,
		`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at, operator)
		 VALUES ('c2', 'ws1', 'k2', '', '2026-09-25T00:00:00Z', '2026-09-25T00:00:00Z', 'agents/x')`,
		`INSERT INTO job_index (workspace_id, name, path, kind, active, updated_at)
		 VALUES ('ws1', 'sling', '/tmp/sling/TREBI.md', 'assistant', 1, '2026-09-25T00:00:00Z')`,
		`INSERT INTO runs (id, workspace_id, job, kind, status, runtime, trace_id, started_at, updated_at)
		 VALUES ('r1', 'ws1', 'sling', 'assistant', 'running', 'tui', 'tr', '2026-09-25T00:00:00Z', '2026-09-25T00:00:00Z')`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v (%s)", err, q)
		}
	}
	// assistant_usage and event_queue rows exist pre-v24 by construction of
	// earlier migrations; v24 drops the table and clears pending items.
	if _, err := db.Exec(`INSERT INTO event_queue (id, connector, workspace_id, job, kind, status, created_at, updated_at)
		VALUES ('q1', 'whatsapp', 'ws1', 'sling', 'assistant', 'pending', '2026-09-25T00:00:00Z', '2026-09-25T00:00:00Z')`); err != nil {
		t.Fatalf("seed queue: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenPath(path)
	if err != nil {
		t.Fatalf("open applies v24: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if v, _ := s.SchemaVersion(); v != SchemaVersion {
		t.Fatalf("schema version %d", v)
	}
	db2 := s.DB()
	var n int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM assistant_sessions`).Scan(&n); err != nil {
		t.Fatalf("assistant_sessions missing: %v", err)
	}
	if err := db2.QueryRow(`SELECT COUNT(*) FROM conversation_events`).Scan(&n); err != nil {
		t.Fatalf("conversation_events missing: %v", err)
	}
	if err := db2.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind='assistant'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("assistant runs must be gone: %d %v", n, err)
	}
	if err := db2.QueryRow(`SELECT COUNT(*) FROM job_index WHERE kind='assistant'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("assistant index rows must be gone: %d %v", n, err)
	}
	if err := db2.QueryRow(`SELECT COUNT(*) FROM event_queue WHERE kind='assistant' AND status='pending'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("pending assistant queue items must be gone: %d %v", n, err)
	}
	rows, err := db2.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name IN ('assistant_usage')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("assistant_usage must be dropped")
	}
	rows.Close()
	if err := db2.QueryRow(`SELECT assistant_seeded FROM workspaces LIMIT 1`).Scan(&n); err == nil {
		t.Fatal("assistant_seeded column must be dropped")
	}
}

// applyThrough applies migrations v1..n, for a v23-shaped fixture.
func applyThrough(db *sql.DB, n int) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	var from int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&from); err != nil {
		return err
	}
	steps := migrations()
	for i := from; i < n; i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(steps[i]); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			i+1, time.Now().UTC().Format(time.RFC3339)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
