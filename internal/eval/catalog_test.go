package eval

import (
	"encoding/json"
	"os/exec"
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

func TestConvertedTaskCommandsJQ(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	t.Parallel()
	evals := catalogDir(t)
	payloads := map[string][]string{
		"ambiguous-workspace":  {`/bin/bash -lc 'trebi -w staging job run report'`},
		"bus-inspect":          {`/bin/bash -lc 'trebi bus --replay --topics run --limit 10 --json'`},
		"file-exists-conflict": {`printf 'a,b\n9,9\n' | trebi file put data.csv --if-match abc`},
		"run-lifecycle":        {`trebi runs message id "hi"`, `trebi runs kill id`},
		"no-direct-state":      {`trebi doctor --json`},
	}
	for id, cmds := range payloads {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			tasks, err := LoadTasks(evals, id, "")
			if err != nil {
				t.Fatal(err)
			}
			var expr string
			for _, c := range tasks[0].Checks {
				if c.CommandsJQ != "" {
					expr = c.CommandsJQ
					break
				}
			}
			if expr == "" {
				t.Fatal("missing commands_jq")
			}
			data, err := json.Marshal(cmds)
			if err != nil {
				t.Fatal(err)
			}
			ok, detail, qerr := jqTruthy(expr, data)
			if qerr != nil {
				t.Fatalf("jq %s: %v", expr, qerr)
			}
			if !ok {
				t.Fatalf("predicate failed: %s", detail)
			}
		})
	}
	t.Run("bus without --limit fails", func(t *testing.T) {
		t.Parallel()
		tasks, err := LoadTasks(evals, "bus-inspect", "")
		if err != nil {
			t.Fatal(err)
		}
		var expr string
		for _, c := range tasks[0].Checks {
			if c.CommandsJQ != "" {
				expr = c.CommandsJQ
			}
		}
		data, err := json.Marshal([]string{`trebi bus --replay --topics run --json`})
		if err != nil {
			t.Fatal(err)
		}
		ok, _, qerr := jqTruthy(expr, data)
		if qerr != nil {
			t.Fatal(qerr)
		}
		if ok {
			t.Fatal("expected fail without --limit")
		}
	})
	t.Run("sqlite is rejected", func(t *testing.T) {
		t.Parallel()
		tasks, err := LoadTasks(evals, "no-direct-state", "")
		if err != nil {
			t.Fatal(err)
		}
		var expr string
		for _, c := range tasks[0].Checks {
			if c.CommandsJQ != "" {
				expr = c.CommandsJQ
			}
		}
		data, err := json.Marshal([]string{`sqlite3 $TREBI_HOME/trebi.db '.tables'`})
		if err != nil {
			t.Fatal(err)
		}
		ok, _, qerr := jqTruthy(expr, data)
		if qerr != nil {
			t.Fatal(qerr)
		}
		if ok {
			t.Fatal("expected fail on sqlite3")
		}
	})
}

func catalogDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "evals")
}
