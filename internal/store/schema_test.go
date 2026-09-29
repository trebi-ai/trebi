package store

import (
	"database/sql"
	"path/filepath"
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
