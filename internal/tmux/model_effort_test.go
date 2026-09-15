package tmux_test

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/tmux"
)

func TestModelEffortFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tool          string
		model, effort string
		want          []string
	}{
		{"claude", "sonnet", "high", []string{"--model", "sonnet", "--effort", "high"}},
		{"grok", "grok-4.6", "low", []string{"--model", "grok-4.6", "--effort", "low"}},
		{"codex", "gpt-5.4", "xhigh", []string{"--model", "gpt-5.4", "-c", "model_reasoning_effort=xhigh"}},
		{"opencode", "anthropic/claude-sonnet-4-5", "high", []string{"--model", "anthropic/claude-sonnet-4-5", "--variant", "high"}},
		{"copilot", "auto", "medium", []string{"--model", "auto", "--effort", "medium"}},
		{"pi", "claude-sonnet-4-5", "off", []string{"--model", "claude-sonnet-4-5", "--effort", "off"}},
		{"cursor", "gpt-5", "", []string{"--model", "gpt-5"}},
		{"cursor", "gpt-5", "high", []string{"--model", "gpt-5"}}, // cursor has no effort flag
		{"gemini", "gemini-2.5-flash", "", []string{"--model", "gemini-2.5-flash"}},
		{"claude", "", "", nil},
		{"fake", "x", "high", nil},
	}
	for _, tc := range cases {
		got := tmux.ModelEffortFlags(tc.tool, tc.model, tc.effort)
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("%s %q %q: got %v want %v", tc.tool, tc.model, tc.effort, got, tc.want)
		}
	}
}

func TestApplyModelEffortRespectsCaller(t *testing.T) {
	t.Parallel()
	cmd := "claude --dangerously-skip-permissions"
	got := tmux.ApplyModelEffort(cmd, "claude", "sonnet", "high")
	if !strings.Contains(got, "--model sonnet") || !strings.Contains(got, "--effort high") {
		t.Fatalf("append: %s", got)
	}

	// A caller-supplied --model wins; effort still lands.
	got = tmux.ApplyModelEffort("claude --model opus", "claude", "sonnet", "high")
	if strings.Contains(got, "sonnet") || !strings.Contains(got, "--effort high") {
		t.Fatalf("model override: %s", got)
	}

	// Codex effort marker.
	got = tmux.ApplyModelEffort("codex -c model_reasoning_effort=low", "codex", "gpt-5.4", "high")
	if strings.Contains(got, "model_reasoning_effort=high") || !strings.Contains(got, "--model gpt-5.4") {
		t.Fatalf("codex effort override: %s", got)
	}

	if got := tmux.ApplyModelEffort("claude", "claude", "", ""); got != "claude" {
		t.Fatalf("no-op: %q", got)
	}
}
