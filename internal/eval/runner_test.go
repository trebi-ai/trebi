package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeClient struct {
	name   string
	launch *LaunchResult
	err    error
	called *int
}

func (f *fakeClient) Name() string { return f.name }

func (f *fakeClient) Launch(_ context.Context, opts LaunchOpts) (*LaunchResult, error) {
	if f.called != nil {
		*f.called++
	}
	if f.launch != nil && opts.Transcript != "" {
		_ = writeTranscript(opts.Transcript, f.launch.Raw, f.launch.Steps)
	}
	return f.launch, f.err
}

func TestDeadClientGate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	launch := authFailLaunch()
	called := 0
	r := &Runner{
		newClient: func(string) (Client, error) {
			return &fakeClient{name: "codex", launch: launch, called: &called}, nil
		},
		scoreAll: func(context.Context, []JudgeDim, []byte, []byte) []JudgeResult {
			t.Fatal("judge invoked")
			return nil
		},
	}
	env := &trialEnv{workdir: dir, artDir: dir, bin: "true", apiURL: "http://127.0.0.1:1"}
	p := Placeholders{Workdir: dir, Home: dir, Transcript: filepath.Join(dir, "transcript.json"), Snapshot: filepath.Join(dir, "snapshot.json")}
	task := &Task{ID: "x", Prompt: "hi", TimeoutS: 5, Checks: []Check{{Name: "f", FileExists: filepath.Join(dir, "missing")}}}
	tr := &TrialResult{TaskID: "x"}
	got := r.finishClient(context.Background(), context.Background(), task, env, p, tr, "codex", surfaceMCP)
	if got.Outcome != OutcomeInfraFailure {
		t.Fatalf("outcome %s err %s", got.Outcome, got.Error)
	}
	if !strings.Contains(got.Error, "401") {
		t.Fatalf("error %q", got.Error)
	}
	if len(got.Checks) != 0 {
		t.Fatalf("checks ran %+v", got.Checks)
	}
	if called != 1 {
		t.Fatalf("called %d", called)
	}
}

func TestDeadClientOneAssistantGrades(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exist := filepath.Join(dir, "ok.txt")
	if err := os.WriteFile(exist, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	judgeCalled := 0
	r := &Runner{
		newClient: func(string) (Client, error) {
			return &fakeClient{name: "codex", launch: &LaunchResult{
				Steps: Steps{{Role: "assistant", Content: "DONE"}},
			}}, nil
		},
		scoreAll: func(context.Context, []JudgeDim, []byte, []byte) []JudgeResult {
			judgeCalled++
			return nil
		},
	}
	env := &trialEnv{workdir: dir, artDir: dir, bin: "true", apiURL: "http://127.0.0.1:1"}
	p := Placeholders{Workdir: dir, Home: dir, Transcript: filepath.Join(dir, "transcript.json"), Snapshot: filepath.Join(dir, "snapshot.json")}
	task := &Task{ID: "x", Prompt: "hi", TimeoutS: 5, Checks: []Check{{Name: "f", FileExists: exist}}}
	tr := &TrialResult{TaskID: "x"}
	got := r.finishClient(context.Background(), context.Background(), task, env, p, tr, "codex", surfaceCLI)
	if got.Outcome == OutcomeInfraFailure {
		t.Fatalf("infra: %s", got.Error)
	}
	if len(got.Checks) == 0 {
		t.Fatal("expected checks")
	}
	if judgeCalled != 1 {
		t.Fatalf("judge %d", judgeCalled)
	}
}

// TestClientTransportFaultRetries pins that a client cut off mid-stream is
// infra, not a fail. It made tool calls but reported a fault, so its answer is
// missing for a reason a retry can fix.
func TestClientTransportFaultRetries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := &Runner{
		newClient: func(string) (Client, error) {
			return &fakeClient{name: "codex", launch: &LaunchResult{
				Steps:  []Step{{Role: "tool", Name: "mcp__trebi__system", Content: `{"action":"health"}`}},
				Errors: []string{"stream disconnected before completion: Transport error"},
			}}, nil
		},
		scoreAll: func(context.Context, []JudgeDim, []byte, []byte) []JudgeResult {
			t.Fatal("judge invoked")
			return nil
		},
	}
	env := &trialEnv{workdir: dir, artDir: dir, bin: "true", apiURL: "http://127.0.0.1:1"}
	p := Placeholders{Workdir: dir, Home: dir, Transcript: filepath.Join(dir, "transcript.json"), Snapshot: filepath.Join(dir, "snapshot.json")}
	task := &Task{ID: "x", Prompt: "hi", TimeoutS: 5, Checks: []Check{{Name: "f", FileExists: filepath.Join(dir, "missing")}}}
	got := r.finishClient(context.Background(), context.Background(), task, env, p, &TrialResult{TaskID: "x"}, "codex", surfaceMCP)
	if got.Outcome != OutcomeInfraFailure {
		t.Fatalf("outcome %s err %q", got.Outcome, got.Error)
	}
	if !strings.Contains(got.Error, "Transport error") {
		t.Fatalf("error %q", got.Error)
	}
}

// TestClientTimeoutStillGrades pins the other half: a client that simply ran
// out of time reports no fault, and its work is on the daemon, so it grades.
func TestClientTimeoutStillGrades(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exist := filepath.Join(dir, "ok.txt")
	if err := os.WriteFile(exist, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		newClient: func(string) (Client, error) {
			return &fakeClient{name: "codex", launch: &LaunchResult{
				Steps: Steps{{Role: "tool", Name: "mcp__trebi__system", Content: `{"action":"health"}`}},
			}}, nil
		},
		scoreAll: func(context.Context, []JudgeDim, []byte, []byte) []JudgeResult { return nil },
	}
	env := &trialEnv{workdir: dir, artDir: dir, bin: "true", apiURL: "http://127.0.0.1:1"}
	p := Placeholders{Workdir: dir, Home: dir, Transcript: filepath.Join(dir, "transcript.json"), Snapshot: filepath.Join(dir, "snapshot.json")}
	task := &Task{ID: "x", Prompt: "hi", TimeoutS: 5, Checks: []Check{{Name: "f", FileExists: exist}}}
	got := r.finishClient(context.Background(), context.Background(), task, env, p, &TrialResult{TaskID: "x"}, "codex", surfaceCLI)
	if got.Outcome == OutcomeInfraFailure {
		t.Fatalf("infra: %s", got.Error)
	}
	if len(got.Checks) == 0 {
		t.Fatal("expected checks to run")
	}
	if !strings.Contains(got.Error, "timeout") {
		t.Fatalf("error %q, want timeout note", got.Error)
	}
}

func TestSeedChecksGreenSkipsClient(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exist := filepath.Join(dir, "seeded.txt")
	if err := os.WriteFile(exist, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := 0
	r := &Runner{
		newClient: func(string) (Client, error) {
			called++
			return &fakeClient{name: "codex"}, nil
		},
	}
	env := &trialEnv{workdir: dir, home: dir, artDir: dir, bin: "true"}
	p := Placeholders{Workdir: dir, Home: dir, Transcript: filepath.Join(dir, "t.json"), Snapshot: filepath.Join(dir, "s.json")}
	g := newGrader(env, p)
	task := &Task{ID: "x", Prompt: "hi", TimeoutS: 5, Checks: []Check{{Name: "seeded", FileExists: exist}}}
	tr := &TrialResult{TaskID: "x"}
	got := r.runAfterSeed(context.Background(), context.Background(), task, env, g, p, tr, "codex", surfaceCLI, false)
	if got.Outcome != OutcomeInfraFailure || got.Error != "checks green on seed state" {
		t.Fatalf("got %s %q", got.Outcome, got.Error)
	}
	if called != 0 {
		t.Fatalf("client called %d", called)
	}
}

func TestSeedChecksRedProceeds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	called := 0
	r := &Runner{
		newClient: func(string) (Client, error) {
			called++
			return &fakeClient{name: "codex", launch: &LaunchResult{
				Steps: Steps{{Role: "assistant", Content: "DONE"}},
			}}, nil
		},
		scoreAll: func(context.Context, []JudgeDim, []byte, []byte) []JudgeResult { return nil },
	}
	env := &trialEnv{workdir: dir, home: dir, artDir: dir, bin: "true"}
	p := Placeholders{Workdir: dir, Home: dir, Transcript: filepath.Join(dir, "t.json"), Snapshot: filepath.Join(dir, "s.json")}
	g := newGrader(env, p)
	task := &Task{ID: "x", Prompt: "hi", TimeoutS: 5, Checks: []Check{{Name: "missing", FileExists: filepath.Join(dir, "nope")}}}
	tr := &TrialResult{TaskID: "x"}
	_ = r.runAfterSeed(context.Background(), context.Background(), task, env, g, p, tr, "codex", surfaceCLI, false)
	if called != 1 {
		t.Fatalf("client called %d", called)
	}
	if tr.Error == "checks green on seed state" {
		t.Fatal("should not abort on red seed")
	}
}

func TestPreflightAuthAbortsBeforeArtifacts(t *testing.T) {
	out := t.TempDir()
	skill := filepath.Join(out, "SKILL.md")
	if err := os.WriteFile(skill, []byte("# skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(Options{
		EvalsDir:  catalogDir(t),
		OutDir:    out,
		SkillPath: skill,
		TrebiBin:  lookTrebi(t),
		Clients:   []string{"codex"},
		Surface:   surfaceMCP,
		TaskID:    "onboarding-health",
		Trials:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.scoreAll = func(context.Context, []JudgeDim, []byte, []byte) []JudgeResult { return nil }
	r.preflightLaunch = func(context.Context, string, string) (*LaunchResult, string, error) {
		return authFailLaunch(), "", nil
	}
	_, err = r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(err.Error(), "preflight: codex/mcp cannot reach its model") {
		t.Fatalf("err %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("trial artifact created: %s", e.Name())
		}
	}
}

func TestPreflightHandshakeNamesConfig(t *testing.T) {
	t.Parallel()
	launch := &LaunchResult{Steps: Steps{{Role: "assistant", Content: "DONE"}}}
	err := preflightVerdict("codex", surfaceMCP, launch, "/tmp/client-home/config.toml")
	if err == nil || !strings.Contains(err.Error(), "config.toml") {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(err.Error(), "mcp__trebi") {
		t.Fatalf("err %v", err)
	}
}

func TestPreflightOK(t *testing.T) {
	t.Parallel()
	launch := &LaunchResult{Steps: Steps{
		{Role: "tool", Name: "mcp__trebi__system", Content: `{"action":"health"}`},
		{Role: "assistant", Content: "DONE"},
	}}
	if err := preflightVerdict("codex", surfaceMCP, launch, "/tmp/config.toml"); err != nil {
		t.Fatal(err)
	}
}

func TestGuardSeedChecksEmptyPreClient(t *testing.T) {
	t.Parallel()
	tr := &TrialResult{}
	task := &Task{Checks: []Check{{Name: "jq", CommandsJQ: "any(true)"}}}
	if guardSeedChecks(context.Background(), task, &Grader{}, tr) {
		t.Fatal("commands_jq-only should proceed")
	}
}

// A task whose only state checks assert that nothing changed cannot start red.
// The guard must let it through instead of calling the trial infra.
func TestGuardSeedChecksSkipsStartsGreen(t *testing.T) {
	t.Parallel()
	tr := &TrialResult{}
	task := &Task{Checks: []Check{
		{Name: "home untouched", HomeUntouched: true, StartsGreen: true},
		{Name: "read a guide", CommandsJQ: "any(true)"},
	}}
	if guardSeedChecks(context.Background(), task, &Grader{}, tr) {
		t.Fatal("a starts_green task must run the client")
	}
	if got := mustStartRed(task.Checks); len(got) != 1 || got[0].Name != "read a guide" {
		t.Fatalf("mustStartRed kept %+v", got)
	}
}

func TestPreflightTimeoutBudget(t *testing.T) {
	t.Parallel()
	if preflightTimeout < 60*time.Second {
		t.Fatalf("preflight timeout %s too short for 401 reconnect", preflightTimeout)
	}
}

func TestPlanCoversTrialMatrix(t *testing.T) {
	t.Parallel()
	task := &Task{ID: "t1", Clients: []string{"codex", "claude"}, Trials: 2, Surfaces: []string{surfaceCLI, surfaceMCP}}
	r := &Runner{opts: Options{Surface: "both"}}
	plans := r.plan([]*Task{task}, "/out")
	if len(plans) != 8 {
		t.Fatalf("plans %d", len(plans))
	}
	dirs := map[string]bool{}
	for _, p := range plans {
		if dirs[p.art] {
			t.Fatalf("duplicate art dir %s", p.art)
		}
		dirs[p.art] = true
	}
	if !dirs[filepath.Join("/out", "t1", "codex", "0")] || !dirs[filepath.Join("/out", "t1", "mcp", "claude", "1")] {
		t.Fatalf("dirs %v", dirs)
	}
}

func TestParallelRunsEveryTrial(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	task := &Task{ID: "t1", Clients: []string{"codex"}, Trials: 6, Surfaces: []string{surfaceCLI}, Prompt: "hi", TimeoutS: 1}
	r := &Runner{opts: Options{Parallel: 3, TrebiBin: "true"}}
	rep := newReport()
	r.runTrials(context.Background(), r.plan([]*Task{task}, dir), rep, dir)
	if len(rep.Trials) != 6 {
		t.Fatalf("trials %d", len(rep.Trials))
	}
	seen := map[int]int{}
	for _, tr := range rep.Trials {
		seen[tr.Index]++
	}
	for i := range 6 {
		if seen[i] != 1 {
			t.Fatalf("index %d ran %d times", i, seen[i])
		}
	}
}

// A task whose checks are all commands_jq leaves an oracle replay nothing to
// run. finishOracle passes it, so such a task is not permanently red.
func TestSkipOracleChecksEmptiesCommandsJQOnlyTask(t *testing.T) {
	t.Parallel()
	checks := []Check{
		{Name: "cli jq", CommandsJQ: "any(true)"},
		{Name: "mcp jq", CommandsJQ: "any(true)"},
	}
	if got := skipOracleChecks(checks); len(got) != 0 {
		t.Fatalf("kept %+v, want none", got)
	}
	if checksOutcome(nil) != OutcomeFail {
		t.Fatal("a real trial with no checks still fails")
	}
}

func authFailLaunch() *LaunchResult {
	raw := []byte(`{"type":"error","message":"unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: wss://api.openai.com/v1/responses"}`)
	return parseCodexJSON(raw)
}
