package runs

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/skills"
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
	got := combinedSkillHint(&job.KitYAML{Skills: []string{"research", "pdf"}}, []string{"PDF", "repo"}, nil)
	if !strings.Contains(got, "Preferred skills: PDF, repo, research") {
		t.Fatalf("combined hint = %q", got)
	}
	if combinedSkillHint(nil, nil, nil) != "" {
		t.Fatal("nil kit produced a hint")
	}
	used := map[string]bool{"pdf": true, "research": true}
	if got := combinedSkillHint(&job.KitYAML{Skills: []string{"research"}}, []string{"PDF", "gone"}, used); got != "Preferred skills: PDF, research" {
		t.Fatalf("filtered hint = %q", got)
	}
}

func TestScopedSkills(t *testing.T) {
	list := []skills.Resolved{
		{Skill: skills.Skill{Ref: "job/J1/deploy", Name: "deploy", Scope: skills.ScopeJob, Path: "/w/daily/skills/deploy", Description: "Ship it."}, Used: true},
		{Skill: skills.Skill{Ref: "global/deploy", Name: "deploy", Scope: skills.ScopeGlobal, Path: "/h/skills/deploy"}, OverwrittenBy: "job/J1/deploy"},
		{Skill: skills.Skill{Ref: "global/pdf", Name: "pdf", Scope: skills.ScopeGlobal, Path: "/h/skills/pdf"}, Used: true},
	}
	w := wireSkills(list)
	if len(w) != 1 || w[0].Name != "deploy" || w[0].Dir != "/w/daily/skills/deploy" {
		t.Fatalf("wire skills %+v", w)
	}
	hint := scopedSkillHint(list)
	if !strings.Contains(hint, "- deploy (job skill, /w/daily/skills/deploy): Ship it.") || strings.Contains(hint, "pdf") {
		t.Fatalf("hint %q", hint)
	}
	if scopedSkillHint(list[1:]) != "" {
		t.Fatal("global skills need no hint")
	}
	if names := usedSkillNames(list); !names["deploy"] || !names["pdf"] || len(names) != 2 {
		t.Fatalf("used %v", names)
	}
}
