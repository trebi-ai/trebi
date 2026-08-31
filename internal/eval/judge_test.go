package eval

import (
	"context"
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
	p := judgePrompt(JudgeDim{Name: "used -w", Rubric: "cite -w"}, nil, nil, surfaceCLI)
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
			stubClaude(t, tt.script)
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

// stubClaude puts a fake claude binary on PATH that runs the given script.
func stubClaude(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}
