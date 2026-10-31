package moves

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestEveryTableHasAMoveRule fails when a new table is neither moved nor
// skipped with a reason. Add the table to tables or skipped.
func TestEveryTableHasAMoveRule(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	known := map[string]bool{"workspaces": true}
	for _, sp := range tables {
		known[sp.name] = true
	}
	for _, sk := range skipped {
		if sk.Reason == "" {
			t.Errorf("skipped table %s has no reason", sk.Table)
		}
		known[sk.Table] = true
	}
	rows, err := st.DB().Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		have[name] = true
		if !known[name] && !strings.HasPrefix(name, "state_fts_") {
			t.Errorf("table %s has no move rule", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name := range known {
		if !have[name] {
			t.Errorf("move rule for a table that does not exist: %s", name)
		}
	}
}

// TestHomeDirsSkipMachineFolders: harness homes and logins live under the
// visor folder and stay on the machine.
func TestHomeDirsSkipMachineFolders(t *testing.T) {
	visor, err := filepath.Rel(home.Base(), home.VisorDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range homeDirs() {
		for _, machine := range []string{filepath.ToSlash(visor), "harness", "secrets", "connectors"} {
			if d == machine || strings.HasPrefix(d, machine+"/") || strings.HasPrefix(machine, d+"/") {
				t.Errorf("archive home folder %s overlaps the machine folder %s", d, machine)
			}
		}
	}
}
