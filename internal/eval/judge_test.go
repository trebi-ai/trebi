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
