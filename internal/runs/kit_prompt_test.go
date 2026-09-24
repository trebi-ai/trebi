package runs

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

func TestKitPromptHint(t *testing.T) {
	got := kitPromptHint(&job.KitYAML{Skills: []string{"research", "pdf"}, MCP: []string{"notion"}})
	if !strings.Contains(got, "Preferred skills: research, pdf") || !strings.Contains(got, "Preferred MCP servers: notion") {
		t.Fatalf("hint = %q", got)
	}
	if kitPromptHint(&job.KitYAML{}) != "" {
		t.Fatal("empty kit produced a hint")
	}
}

func TestCombinedSkillHint(t *testing.T) {
	got := combinedSkillHint(&job.KitYAML{Skills: []string{"research", "pdf"}, MCP: []string{"notion"}}, []string{"PDF", "repo"})
	if !strings.Contains(got, "Preferred skills: PDF, repo, research") {
		t.Fatalf("combined hint = %q", got)
	}
	if combinedSkillHint(nil, nil) != "" {
		t.Fatal("nil kit produced a hint")
	}
}
