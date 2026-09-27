package runs

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

func TestKitPromptHint(t *testing.T) {
	got := kitPromptHint(&job.KitYAML{Skills: []string{"research", "pdf"}})
	if !strings.Contains(got, "Preferred skills: research, pdf") {
		t.Fatalf("hint = %q", got)
	}
	if strings.Contains(got, "MCP") {
		t.Fatalf("connector actions no longer travel by prompt line: %q", got)
	}
	if kitPromptHint(&job.KitYAML{}) != "" {
		t.Fatal("empty kit produced a hint")
	}
}

func TestCombinedSkillHint(t *testing.T) {
	got := combinedSkillHint(&job.KitYAML{Skills: []string{"research", "pdf"}}, []string{"PDF", "repo"})
	if !strings.Contains(got, "Preferred skills: PDF, repo, research") {
		t.Fatalf("combined hint = %q", got)
	}
	if combinedSkillHint(nil, nil) != "" {
		t.Fatal("nil kit produced a hint")
	}
}
