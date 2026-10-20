package skills_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestSkillCRUDAndProjection(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := skills.New(st)
	ctx := context.Background()

	sk, err := svc.Create(ctx, skills.CreateOpts{Ref: skills.GlobalRef("demo-skill"), SkillMD: "# Demo\n\n- bin: sh\n", Provenance: skills.ProvenanceLocal})
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "demo-skill" || sk.Hash == "" {
		t.Fatalf("%+v", sk)
	}
	if len(sk.Requires) != 1 || !sk.Requires[0].Satisfied {
		t.Fatalf("requires=%+v", sk.Requires)
	}

	list, err := svc.List(ctx, skills.ListOpts{})
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}

	got, err := svc.Get(ctx, skills.GlobalRef("demo-skill"), true)
	if err != nil || got.SkillMD == "" {
		t.Fatalf("%v %v", got, err)
	}

	_, err = svc.Update(ctx, skills.GlobalRef("demo-skill"), "# Demo v2\n", got.Hash, map[string]bool{"claude": true, "cursor": true})
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home.HarnessDir("claude"), "skills", "demo-skill")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("projection missing: %v", err)
	}
	// Cursor reads skills-cursor, not skills (bridge catalog), so the
	// projection must land there and nowhere else.
	cursor := filepath.Join(home.HarnessDir("cursor"), "skills-cursor", "demo-skill")
	if _, err := os.Lstat(cursor); err != nil {
		t.Fatalf("cursor projection missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home.HarnessDir("cursor"), "skills")); err == nil {
		t.Fatal("cursor projection landed in the path the harness never reads")
	}

	proj, err := svc.ProjectForRun(ctx, skills.RunScope{}, []string{"demo-skill"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(proj, "demo-skill")); err != nil {
		t.Fatal(err)
	}

	if err := svc.Delete(ctx, skills.GlobalRef("demo-skill")); err != nil {
		t.Fatal(err)
	}
	list, _ = svc.List(ctx, skills.ListOpts{})
	if len(list) != 0 {
		t.Fatalf("after delete: %v", list)
	}
}

func TestSkills_InheritedUnionAndGuard(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := skills.New(st)
	ctx := context.Background()

	user := t.TempDir()
	claude := filepath.Join(user, ".claude", "skills", "sling")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "SKILL.md"), []byte("# Sling\n\nInherited.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc.WithUserHome(user)

	list, err := svc.List(ctx, skills.ListOpts{})
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	row := list[0]
	if row.Name != "sling" || row.Provenance != skills.ProvenanceInherited {
		t.Fatalf("row %+v", row)
	}
	if len(row.Origins) != 1 || row.Origins[0].Tool != "claude" {
		t.Fatalf("origins %+v", row.Origins)
	}
	got, err := svc.Get(ctx, skills.GlobalRef("sling"), true)
	if err != nil {
		t.Fatal(err)
	}
	if got.SkillMD == "" || got.Path != claude {
		t.Fatalf("get %+v", got)
	}
	if _, err := svc.Update(ctx, skills.GlobalRef("sling"), "# no\n", "", nil); !errors.Is(err, skills.ErrInherited) {
		t.Fatalf("update inherited: %v", err)
	}
	if err := svc.Delete(ctx, skills.GlobalRef("sling")); !errors.Is(err, skills.ErrInherited) {
		t.Fatalf("delete inherited: %v", err)
	}
	if _, _, err := svc.Refresh(ctx, skills.GlobalRef("sling")); !errors.Is(err, skills.ErrInherited) {
		t.Fatalf("refresh inherited: %v", err)
	}

	sk, err := svc.Create(ctx, skills.CreateOpts{Ref: skills.GlobalRef("local-one"), SkillMD: "# Local\n", Provenance: skills.ProvenanceLocal})
	if err != nil {
		t.Fatal(err)
	}
	if sk.Provenance != skills.ProvenanceLocal {
		t.Fatalf("managed %+v", sk)
	}
	list, err = svc.List(ctx, skills.ListOpts{})
	if err != nil || len(list) != 2 {
		t.Fatalf("managed+inherited %d %v", len(list), list)
	}

	// Same name as inherited becomes one managed row with origins attached.
	if _, err := svc.Create(ctx, skills.CreateOpts{Ref: skills.GlobalRef("sling"), SkillMD: "# Managed sling\n", Provenance: skills.ProvenanceLocal}); err != nil {
		t.Fatal(err)
	}
	list, err = svc.List(ctx, skills.ListOpts{})
	if err != nil || len(list) != 2 {
		t.Fatalf("merged %d %v", len(list), list)
	}
	var merged *skills.Skill
	for i := range list {
		if list[i].Name == "sling" {
			merged = &list[i]
		}
	}
	if merged == nil || merged.Provenance != skills.ProvenanceLocal || len(merged.Origins) != 1 {
		t.Fatalf("want merged managed+origins, got %+v", merged)
	}
}

func TestSkills_InheritedPointerFollows(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := skills.New(st)
	ctx := context.Background()

	user := t.TempDir()
	realDir := filepath.Join(user, ".agents", "skills", "sling-api-specs")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# API Specifications\n\nReal body.\n"
	if err := os.WriteFile(filepath.Join(realDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "AUTHENTICATION.md"), []byte("auth\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ptrDir := filepath.Join(user, ".claude", "skills", "sling-api-specs")
	if err := os.MkdirAll(ptrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ptrDir, "SKILL.md"), []byte("@"+filepath.Join(realDir, "SKILL.md")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc.WithUserHome(user)

	got, err := svc.Get(ctx, skills.GlobalRef("sling-api-specs"), true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provenance != skills.ProvenanceInherited || got.Path != realDir {
		t.Fatalf("got %+v", got)
	}
	if got.SkillMD != body {
		t.Fatalf("skill_md %q", got.SkillMD)
	}
	if len(got.Origins) != 2 {
		t.Fatalf("origins %+v", got.Origins)
	}
}

// TestSyncConnectorSkillRoot proves a skill at the connector root copies only
// SKILL.md and the skill folders, and a skill sub-folder copies whole.
func TestSyncConnectorSkillRoot(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := skills.New(st)
	src := t.TempDir()
	for name, body := range map[string]string{
		"SKILL.md": "---\nname: vitally\ndescription: x\n---\n# Vitally\n", "main.go": "package main\n",
		"go.mod": "module x\n", "trebi-connector.yaml": "name: vitally\n",
		"references/api.md": "# API\n", "scripts/run.sh": "#!/bin/sh\n", "internal/x.go": "package x\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(src, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		root bool
		want []string
	}{
		{true, []string{"SKILL.md", "references", "scripts"}},
		{false, []string{"SKILL.md", "go.mod", "internal", "main.go", "references", "scripts", "trebi-connector.yaml"}},
	} {
		if _, err := svc.SyncConnectorSkill(context.Background(), "vitally", src, "vitally", tc.root); err != nil {
			t.Fatal(err)
		}
		ents, err := os.ReadDir(filepath.Join(home.SkillsDir(), "vitally"))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range ents {
			got = append(got, e.Name())
		}
		if !slices.Equal(got, tc.want) {
			t.Fatalf("root=%v: copied %v, want %v", tc.root, got, tc.want)
		}
	}
}
