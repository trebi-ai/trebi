package harness

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestPrintArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, model string
		want        []string
	}{
		{name: "claude", want: []string{"-p", "--output-format", "json", "--tools", "", "--strict-mcp-config", "--disable-slash-commands"}},
		{name: "claude", model: "haiku", want: []string{"-p", "--output-format", "json", "--tools", "", "--strict-mcp-config", "--disable-slash-commands", "--model", "haiku"}},
		{name: "codex", want: []string{"exec", "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "--color", "never", "--output-last-message", "/tmp/r", "-"}},
		{name: "codex", model: "gpt-5", want: []string{"exec", "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "--color", "never", "--output-last-message", "/tmp/r", "--model", "gpt-5", "-"}},
	}
	for _, tc := range tests {
		got, err := PrintArgs(tc.name, tc.model, "/tmp/r")
		if err != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("%s %s: %q %v", tc.name, tc.model, got, err)
		}
		for _, a := range got {
			if strings.Contains(a, "dangerously") || strings.Contains(a, "skip-permissions") || a == "--yolo" {
				t.Fatalf("%s skips permissions: %q", tc.name, got)
			}
		}
	}
	if _, err := PrintArgs("gemini", "", ""); !errors.Is(err, ErrNoPrintMode) {
		t.Fatalf("gemini: %v", err)
	}
}

// TestSpecOMP pins the harness picker entry. omp has no config files yet:
// the bridge has no YAML settings mode.
func TestSpecOMP(t *testing.T) {
	spec, ok := SpecFor("omp")
	if !ok {
		t.Fatal("omp has no spec")
	}
	if spec.Label != "Oh My Pi" || spec.Vendor != "Oh My Pi" || spec.UserDir != ".omp" || len(spec.Files) != 0 {
		t.Fatalf("omp spec = %+v", spec)
	}
	if !Known("omp") {
		t.Fatal("omp is not a known harness")
	}
}
