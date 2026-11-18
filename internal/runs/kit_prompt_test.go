package runs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
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

// TestSkillHintDropsArchived proves an archived skill that the job or the kit
// names is not in "Preferred skills".
func TestSkillHintDropsArchived(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := skills.New(st)
	ctx := context.Background()
	for _, name := range []string{"pdf", "research"} {
		if _, err := svc.Create(ctx, skills.CreateOpts{Ref: skills.GlobalRef(name), SkillMD: "# " + name + "\n"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Archive(ctx, skills.GlobalRef("pdf"), "", time.Now()); err != nil {
		t.Fatal(err)
	}
	resolved, err := svc.Resolve(ctx, skills.RunScope{})
	if err != nil {
		t.Fatal(err)
	}
	got := combinedSkillHint(&job.KitYAML{Skills: []string{"research"}}, []string{"pdf"}, usedSkillNames(resolved))
	if got != "Preferred skills: research" {
		t.Fatalf("hint = %q", got)
	}
}
