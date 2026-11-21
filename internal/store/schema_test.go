package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goldenPath = "testdata/schema-v80.sql"

func TestBaseSchemaMatchesGolden(t *testing.T) {
	t.Parallel()
	s, err := OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := schemaDump(t, s.DB())
	if os.Getenv("GEN_GOLDEN") != "" {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("schema differs from %s; the golden file comes from the old migration chain", goldenPath)
	}
	var av int
	if err := s.DB().QueryRow(`PRAGMA auto_vacuum`).Scan(&av); err != nil || av != 2 {
		t.Fatalf("auto_vacuum: got %d (err %v) want 2", av, err)
	}
	rows, err := s.DB().Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'state_fts_%' AND name != 'schema_migrations'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		var c int
		if err := s.DB().QueryRow(`SELECT count(*) FROM "` + n + `"`).Scan(&c); err != nil || c != 0 {
			t.Errorf("table %s: got %d rows (err %v) want 0", n, c, err)
		}
	}
}

func TestSchemaVersionIsHead(t *testing.T) {
	t.Parallel()
	if SchemaVersion != baseVersion+len(migrations()) {
		t.Fatalf("SchemaVersion %d, base %d, steps %d", SchemaVersion, baseVersion, len(migrations()))
	}
}

// schemaDump lists each schema object as one normalized line, sorted by type and name.
func schemaDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, tbl_name, sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		AND name NOT IN ('state_fts_data','state_fts_idx','state_fts_content','state_fts_docsize','state_fts_config')
		ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, name, tbl, def string
		if err := rows.Scan(&typ, &name, &tbl, &def); err != nil {
			t.Fatal(err)
		}
		b.WriteString(typ + "|" + name + "|" + tbl + "|" + strings.Join(strings.Fields(def), " ") + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
