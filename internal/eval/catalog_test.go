package eval

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestCatalogLoads(t *testing.T) {
	t.Parallel()
	evals := catalogDir(t)
	tasks, err := LoadTasks(evals, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) < 20 {
		t.Fatalf("got %d tasks, want >= 20", len(tasks))
	}
	seen := map[string]bool{}
	for _, tsk := range tasks {
		if seen[tsk.ID] {
			t.Fatalf("duplicate id %s", tsk.ID)
		}
		seen[tsk.ID] = true
		if tsk.OraclePath() == "" {
			t.Fatalf("%s missing oracle", tsk.ID)
		}
	}
}

func catalogDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "evals")
}
