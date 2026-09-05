package eval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJudgeConfirmFail(t *testing.T) {
	failJSON := `echo '{"score":0,"verdict":"fail","evidence":"looks bad","unknown":false}'`
	passJSON := `echo '{"score":1,"verdict":"pass","evidence":"ok","unknown":false}'`
	tests := []struct {
		name    string
		script  string
		min     float64
		unknown bool
		verdict string
	}{
		{
			name:    "both fail keeps fail",
			script:  failJSON,
			min:     1.0,
			unknown: false,
			verdict: "fail",
		},
		{
			name:    "non-strict skip retry",
			script:  failJSON,
			min:     0.7,
			unknown: false,
			verdict: "fail",
		},
		{
			name:    "pass is not retried",
			script:  passJSON,
			min:     1.0,
			unknown: false,
			verdict: "pass",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubClaude(t, tt.script)
			j := &judgeRunner{model: "claude-sonnet-5"}
			v, err := j.call(context.Background(), JudgeDim{Name: "d", Rubric: "r"}, tt.min, nil, nil, surfaceCLI)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if v.Unknown != tt.unknown || v.Verdict != tt.verdict {
				t.Fatalf("got verdict=%s unknown=%v, want verdict=%s unknown=%v", v.Verdict, v.Unknown, tt.verdict, tt.unknown)
			}
		})
	}
}

func TestJudgeConfirmFailDisagreement(t *testing.T) {
	nfile := filepath.Join(t.TempDir(), "n")
	script := "if [ ! -f '" + nfile + "' ]; then echo 1 > '" + nfile + "'; echo '{\"score\":0,\"verdict\":\"fail\",\"evidence\":\"looks bad\",\"unknown\":false}'\nelse echo '{\"score\":1,\"verdict\":\"pass\",\"evidence\":\"actually ok\",\"unknown\":false}'\nfi"
	stubClaude(t, script)
	j := &judgeRunner{model: "claude-sonnet-5"}
	v, err := j.call(context.Background(), JudgeDim{Name: "d", Rubric: "r"}, 1.0, nil, nil, surfaceCLI)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !v.Unknown || v.Verdict != "fail" {
		t.Fatalf("got verdict=%s unknown=%v, want fail/unknown", v.Verdict, v.Unknown)
	}
	// A split counts as pass. The evidence must say so, or the report cannot
	// tell it apart from a grader that genuinely could not tell.
	if !strings.Contains(v.Evidence, "graders split") {
		t.Fatalf("evidence %q, want split note", v.Evidence)
	}
	if !strings.Contains(v.Evidence, "looks bad") || !strings.Contains(v.Evidence, "actually ok") {
		t.Fatalf("evidence %q, want both grader opinions", v.Evidence)
	}
}

// TestJudgeConfirmFailInvokeErrorIsInfra pins the other half of confirmFail: a
// confirming grader that could not run at all is infra, not an abstention. An
// expired judge token used to turn a voted fail into a silent pass, because the
// error was swallowed into unknown, which counts as pass and never retried.
func TestJudgeConfirmFailInvokeErrorIsInfra(t *testing.T) {
	nfile := filepath.Join(t.TempDir(), "n")
	// First call votes fail; the confirming call dies the way an expired token does.
	script := "if [ ! -f '" + nfile + "' ]; then echo 1 > '" + nfile + "'; echo '{\"score\":0,\"verdict\":\"fail\",\"evidence\":\"looks bad\",\"unknown\":false}'\nelse echo 'Failed to refresh token: 401 Unauthorized' >&2; exit 1\nfi"
	stubClaude(t, script)
	j := &judgeRunner{model: "claude-sonnet-5"}
	min := 1.0 // confirmFail only guards binary dimensions
	res := j.score(context.Background(), JudgeDim{Name: "d", Rubric: "r", Min: &min}, nil, nil, surfaceCLI)
	if !res.Infra {
		t.Fatalf("got infra=%v, want infra: a judge that could not run is not an abstention", res.Infra)
	}
	if !strings.Contains(res.Evidence, "confirm-fail invoke") {
		t.Fatalf("evidence %q, want confirm-fail invoke note", res.Evidence)
	}
}

// TestTrialInfraDimensionIsNotAPass pins the outcome side: a binary dimension
// whose judge never answered leaves the trial unproven, so it retries as infra
// rather than passing on the dimensions that did answer.
func TestTrialInfraDimensionIsNotAPass(t *testing.T) {
	t.Parallel()
	checks := []CheckResult{{Name: "c", Pass: true}}
	judge := []JudgeResult{
		{Name: "ok", Min: 1.0, Verdict: "pass", Pass: true},
		{Name: "dead", Min: 1.0, Infra: true, Unknown: true, Pass: true},
	}
	if got := trialOutcome(checks, judge); got != OutcomeInfraFailure {
		t.Fatalf("outcome %s, want infra-failure", got)
	}
	// A telemetry dimension never fails a trial, infra or not.
	tele := []JudgeResult{
		{Name: "ok", Min: 1.0, Verdict: "pass", Pass: true},
		{Name: "efficiency", Min: 0.5, Telemetry: true, Infra: true, Pass: true},
	}
	if got := trialOutcome(checks, tele); got != OutcomePass {
		t.Fatalf("outcome %s, want pass for telemetry infra", got)
	}
}

func TestJudgePromptConsistencyRule(t *testing.T) {
	t.Parallel()
	p := (JudgeDim{Name: "used -w", Rubric: "cite -w"}).prompt(nil, nil, surfaceCLI)
	if !strings.Contains(p, "Your verdict must follow from your evidence") {
		t.Fatal("missing consistency rule")
	}
	if !strings.Contains(p, "do not re-derive end-state facts from file paths") {
		t.Fatal("missing snapshot-inference rule")
	}
}

func TestJudgePreflight(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		wantErr string
	}{
		{"ok", `echo '{"ok":true}'`, ""},
		{"empty reply", `echo ""`, "empty reply"},
		{"quota error on stdout", `echo "usage limit reached"; exit 1`, "usage limit reached"},
		{"error on stderr", `echo "not logged in" >&2; exit 1`, "not logged in"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubBin(t, "claude", tt.script)
			j := &judgeRunner{model: "claude-sonnet-5"}
			err := j.preflight(context.Background())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("preflight: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("preflight err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseJudgeSpec(t *testing.T) {
	t.Parallel()
	cases := []struct {
		spec    string
		name    string
		model   string
		wantErr bool
	}{
		{spec: "", name: clientClaude, model: defaultJudgeModel},
		{spec: "claude", name: clientClaude, model: defaultJudgeModel},
		{spec: "claude:sonnet", name: clientClaude, model: "sonnet"},
		{spec: "codex", name: clientCodex, model: ""},
		{spec: "codex:gpt-5", name: clientCodex, model: "gpt-5"},
		{spec: "unknown", wantErr: true},
		{spec: "gpt-5", wantErr: true},
	}
	for _, tt := range cases {
		name := tt.spec
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			name, model, err := parseJudgeSpec(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil || name != tt.name || model != tt.model {
				t.Fatalf("got %s %s %v", name, model, err)
			}
		})
	}
}

func TestNewJudgeInvalidSpec(t *testing.T) {
	t.Parallel()
	if _, err := newJudge(t.TempDir(), "nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestJudgePreflightCodex(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	stubBin(t, "codex", `echo '{"type":"agent_message","text":"{\"ok\":true}"}'`)
	j := &judgeRunner{harness: clientCodex}
	if err := j.preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}
}

func TestJudgeInvokeCodexPassesModel(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	argsfile := filepath.Join(t.TempDir(), "args")
	stubBin(t, "codex", "printf '%s\\n' \"$@\" > '"+argsfile+"'\necho '{\"type\":\"agent_message\",\"text\":\"{\\\"ok\\\":true}\"}'")
	j := &judgeRunner{harness: clientCodex, model: "gpt-5"}
	if _, err := j.invoke(context.Background(), `{"ok":true}`); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	got, err := os.ReadFile(argsfile)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "-m") || !strings.Contains(s, "gpt-5") {
		t.Fatalf("args missing -m gpt-5:\n%s", s)
	}
}

func TestJudgeInvokeCodexMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	j := &judgeRunner{harness: clientCodex}
	_, err := j.invoke(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "codex not on PATH") {
		t.Fatalf("err %v", err)
	}
}

func TestJudgeReplyFromCodex(t *testing.T) {
	t.Parallel()
	text, err := (&LaunchResult{Steps: Steps{
		{Role: "tool", Content: "ignored"},
		{Role: "assistant", Content: `{"ok":true}`},
	}}).judgeReply()
	if err != nil || string(text) != `{"ok":true}` {
		t.Fatalf("got %q %v", text, err)
	}
	if _, err := (&LaunchResult{Errors: []string{"401"}}).judgeReply(); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err %v", err)
	}
	if _, err := (&LaunchResult{}).judgeReply(); err == nil {
		t.Fatal("expected empty reply")
	}
}

func TestCodexJudgeArgs(t *testing.T) {
	t.Parallel()
	got := strings.Join(codexJudgeArgs(""), " ")
	if !strings.Contains(got, "exec --json") || !strings.HasSuffix(got, " -") {
		t.Fatalf("args %q", got)
	}
	if strings.Contains(got, "-m") {
		t.Fatalf("unexpected -m in %q", got)
	}
	got = strings.Join(codexJudgeArgs("gpt-5"), " ")
	if !strings.Contains(got, "-m gpt-5") {
		t.Fatalf("args %q", got)
	}
}

// TestJudgeRetriesInvokeFailure pins that a transient judge fault is retried.
// An invoke failure abstains, and an abstention counts as pass, so without the
// retry a revoked token silently drops the dimension.
func TestJudgeRetriesInvokeFailure(t *testing.T) {
	t.Parallel()
	calls := 0
	j := &judgeRunner{
		rubrics: map[string]Rubric{"guardrails": {Name: "guardrails", Min: 1}},
		invokeFn: func(context.Context, string) ([]byte, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("judge invoke: context deadline exceeded")
			}
			return []byte(`{"score":1.0,"verdict":"pass","evidence":"ok","unknown":false}`), nil
		},
	}
	out := j.scoreAll(t.Context(), []JudgeDim{{Name: "guardrails"}}, nil, nil, surfaceMCP)
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if len(out) != 1 || out[0].Infra || out[0].Unknown {
		t.Fatalf("result %+v", out)
	}
}

// TestJudgeRetryGivesUp pins that a persistent fault still ends as infra, so
// the report can count it as ungraded rather than hide it.
func TestJudgeRetryGivesUp(t *testing.T) {
	t.Parallel()
	calls := 0
	j := &judgeRunner{
		rubrics: map[string]Rubric{"guardrails": {Name: "guardrails", Min: 1}},
		invokeFn: func(context.Context, string) ([]byte, error) {
			calls++
			return nil, errors.New("token_revoked")
		},
	}
	out := j.scoreAll(t.Context(), []JudgeDim{{Name: "guardrails"}}, nil, nil, surfaceMCP)
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if !out[0].Infra {
		t.Fatalf("result %+v", out[0])
	}
}

func stubClaude(t *testing.T, script string) {
	stubBin(t, "claude", script)
}

// stubBin puts a fake binary on PATH that runs the given script.
func stubBin(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}
