package eval

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

func TestCommandsJQAgainstFixtures(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	t.Parallel()
	evals := catalogDir(t)
	tasks, err := LoadTasks(evals, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		for _, c := range task.Checks {
			if c.CommandsJQ == "" {
				continue
			}
			for _, surface := range []string{surfaceCLI, surfaceMCP} {
				if !slices.Contains(task.Surfaces, surface) || !c.appliesTo(surface) {
					continue
				}
				t.Run(task.ID+"/"+surface+"/"+c.Name, func(t *testing.T) {
					t.Parallel()
					assertFixturePasses(t, evals, task.ID, surface, c.CommandsJQ)
				})
			}
		}
	}
}

// Every judge dimension a task names must resolve to a rubric section on every
// surface the task runs. An unresolved use silently grades an empty rubric.
func TestTaskDimensionsResolve(t *testing.T) {
	t.Parallel()
	evals := catalogDir(t)
	tasks, err := LoadTasks(evals, "", "")
	if err != nil {
		t.Fatal(err)
	}
	j, err := newJudge(evals, "claude")
	if err != nil {
		t.Fatal(err)
	}
	for _, tsk := range tasks {
		for _, surf := range tsk.Surfaces {
			for _, d := range tsk.Judge.Dimensions {
				if d.Use == "" {
					continue
				}
				if _, ok := j.rubricFor(d.Use, surf); !ok {
					t.Errorf("%s/%s: dimension %q has no rubric section", tsk.ID, surf, d.Use)
				}
			}
		}
	}
}

// Every commands_jq must pass on its fixture transcript and fail on an empty
// command list, on each surface the check runs. The fixture goes through the
// same projection a trial uses, so an mcp-only check is validated too.

func assertFixturePasses(t *testing.T, evals, id, surface, expr string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(evals, "fixtures", id+"."+surface+".json"))
	if err != nil {
		t.Fatalf("missing fixture: %v", err)
	}
	cmds, err := transcriptCommands(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) == 0 {
		t.Fatal("fixture projects no commands")
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
		t.Fatalf("fixture does not satisfy %s: %s\ncommands: %s", expr, detail, data)
	}
	// A vacuous pass on an empty list is caught by the grader, which fails a
	// check before jq runs when the agent produced no commands.
	if r := (&Grader{p: Placeholders{Transcript: emptyTranscript(t)}}).checkCommandsJQ(Check{Name: "x", CommandsJQ: expr}); r.Pass {
		t.Errorf("%s passes with no commands in the transcript", expr)
	}
}

// emptyTranscript writes a transcript that carries no agent action.
func emptyTranscript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.json")
	if err := os.WriteFile(p, []byte(`[{"role":"assistant","content":"done"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func catalogDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "evals")
}
