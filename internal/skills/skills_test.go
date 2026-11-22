package skills_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

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

// TestSkillCacheOutsideEdit: an edit on disk with no service call shows in
// the next read.
func TestSkillCacheOutsideEdit(t *testing.T) {
	ref := skills.GlobalRef("demo")
	for _, tc := range []struct {
		name  string
		edit  func(t *testing.T, dir string)
		check func(t *testing.T, svc *skills.Service, before *skills.Skill)
	}{
		{"script edit in place", func(t *testing.T, dir string) {
			t.Helper()
			writeFile(t, filepath.Join(dir, "scripts", "run.sh"), "echo two\n")
		}, func(t *testing.T, svc *skills.Service, before *skills.Skill) {
			t.Helper()
			if got := getSkill(t, svc, ref); got.Hash == before.Hash {
				t.Fatal("hash did not move")
			}
		}},
		{"new script", func(t *testing.T, dir string) {
			t.Helper()
			writeFile(t, filepath.Join(dir, "scripts", "new.py"), "print()\n")
		}, func(t *testing.T, svc *skills.Service, _ *skills.Skill) {
			t.Helper()
			if got := getSkill(t, svc, ref); !slices.Contains(got.Scripts, filepath.Join("scripts", "new.py")) {
				t.Fatalf("scripts %v", got.Scripts)
			}
		}},
		{"description edit", func(t *testing.T, dir string) {
			t.Helper()
			writeFile(t, filepath.Join(dir, "SKILL.md"), "---\ndescription: Two\n---\n# Demo\n")
		}, func(t *testing.T, svc *skills.Service, _ *skills.Skill) {
			t.Helper()
			if got := getSkill(t, svc, ref); got.Description != "Two" {
				t.Fatalf("description %q", got.Description)
			}
		}},
		{"new folder", func(t *testing.T, _ string) {
			t.Helper()
			writeFile(t, filepath.Join(home.SkillsDir(), "other", "SKILL.md"), "# Other\n")
		}, func(t *testing.T, svc *skills.Service, _ *skills.Skill) {
			t.Helper()
			if got := listNames(t, svc); !slices.Equal(got, []string{"demo", "other"}) {
				t.Fatalf("list %v", got)
			}
		}},
		{"folder removed", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, svc *skills.Service, _ *skills.Skill) {
			t.Helper()
			if got := listNames(t, svc); len(got) != 0 {
				t.Fatalf("list %v", got)
			}
			if _, err := svc.Get(context.Background(), ref, false); err == nil {
				t.Fatal("get of a removed skill gave no error")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newCacheEnv(t)
			sk, err := svc.Create(context.Background(), skills.CreateOpts{Ref: ref, SkillMD: "---\ndescription: One\n---\n# Demo\n"})
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(sk.Path, "scripts", "run.sh"), "echo one\n")
			ageTree(t, sk.Path)
			before := getSkill(t, svc, ref)
			tc.edit(t, sk.Path)
			tc.check(t, svc, before)
		})
	}
}

// TestSkillCacheServiceWrites: each write through the service shows at once.
func TestSkillCacheServiceWrites(t *testing.T) {
	svc, _ := newCacheEnv(t)
	ctx := context.Background()
	ref := skills.GlobalRef("demo")
	sk, err := svc.Create(ctx, skills.CreateOpts{Ref: ref, SkillMD: "# One\n"})
	if err != nil {
		t.Fatal(err)
	}
	ageTree(t, sk.Path)
	cur := getSkill(t, svc, ref)
	if _, err := svc.Update(ctx, ref, "# Two\n", cur.Hash, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(ctx, ref, true); got.SkillMD != "# Two\n" || got.Hash == cur.Hash {
		t.Fatalf("after update %+v", got)
	}
	ageTree(t, sk.Path)
	if _, err := svc.Archive(ctx, ref, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := getSkill(t, svc, ref); got.ArchivedAt == "" {
		t.Fatal("archive did not show")
	}
	ageTree(t, sk.Path)
	if _, err := svc.Unarchive(ctx, ref, ""); err != nil {
		t.Fatal(err)
	}
	if got := getSkill(t, svc, ref); got.ArchivedAt != "" {
		t.Fatal("unarchive did not show")
	}
	if err := svc.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if got := listNames(t, svc); len(got) != 0 {
		t.Fatalf("after delete %v", got)
	}
}

// TestListWritesIndexOnlyOnChange: a List with no change writes no index row.
func TestListWritesIndexOnlyOnChange(t *testing.T) {
	svc, st := newCacheEnv(t)
	ctx := context.Background()
	ref := skills.GlobalRef("demo")
	sk, err := svc.Create(ctx, skills.CreateOpts{Ref: ref, SkillMD: "# One\n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE skill_index SET updated_at='mark' WHERE ref=?`, ref.String()); err != nil {
		t.Fatal(err)
	}
	updated := func() string {
		t.Helper()
		var at string
		if err := st.DB().QueryRow(`SELECT updated_at FROM skill_index WHERE ref=?`, ref.String()).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	listNames(t, svc)
	listNames(t, svc)
	if got := updated(); got != "mark" {
		t.Fatalf("list with no change wrote the row: %q", got)
	}
	writeFile(t, filepath.Join(sk.Path, "SKILL.md"), "# Two, longer\n")
	listNames(t, svc)
	if got := updated(); got == "mark" {
		t.Fatal("list after an edit did not write the row")
	}
}

// TestSnapshotListDoesNotAlias: one filter does not change what a second
// filter of the same snapshot sees.
func TestSnapshotListDoesNotAlias(t *testing.T) {
	svc, _ := newCacheEnv(t)
	ctx := context.Background()
	for _, n := range []string{"one", "two"} {
		if _, err := svc.Create(ctx, skills.CreateOpts{Ref: skills.GlobalRef(n), SkillMD: "# s\n"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Archive(ctx, skills.GlobalRef("one"), "", time.Now()); err != nil {
		t.Fatal(err)
	}
	sn, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := sn.List(skills.ListOpts{}); len(got) != 1 || got[0].Name != "two" {
		t.Fatalf("live %v", got)
	}
	if got := sn.List(skills.ListOpts{Archived: skills.ArchivedAll}); len(got) != 2 {
		t.Fatalf("all %v", got)
	}
}

// TestWatchDirsGlobal: the watcher covers the global skills folder, and a
// path in it maps to its global skill.
func TestWatchDirsGlobal(t *testing.T) {
	svc, _ := newCacheEnv(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, skills.CreateOpts{Ref: skills.GlobalRef("demo"), SkillMD: "# s\n"}); err != nil {
		t.Fatal(err)
	}
	dirs := svc.WatchDirs(ctx)
	for _, want := range []string{home.SkillsDir(), filepath.Join(home.SkillsDir(), "demo")} {
		if !slices.Contains(dirs, want) {
			t.Fatalf("watch dirs %v miss %s", dirs, want)
		}
	}
	for _, p := range []string{filepath.Join(home.SkillsDir(), "demo", "SKILL.md"), filepath.Join(home.SkillsDir(), "demo", "scripts", "x.sh")} {
		if ref, ok := svc.RefFor(ctx, p); !ok || ref != skills.GlobalRef("demo") {
			t.Fatalf("RefFor(%s) = %v %v", p, ref, ok)
		}
	}
	if _, ok := svc.RefFor(ctx, filepath.Join(home.Base(), "other", "x")); ok {
		t.Fatal("RefFor mapped a path outside the skills folders")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func getSkill(t *testing.T, svc *skills.Service, ref skills.Ref) *skills.Skill {
	t.Helper()
	sk, err := svc.Get(context.Background(), ref, false)
	if err != nil {
		t.Fatal(err)
	}
	return sk
}

func listNames(t *testing.T, svc *skills.Service) []string {
	t.Helper()
	list, err := svc.List(context.Background(), skills.ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, sk := range list {
		out = append(out, sk.Name)
	}
	slices.Sort(out)
	return out
}

func newCacheEnv(t *testing.T) (*skills.Service, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return skills.New(st), st
}

// ageTree moves every mtime under dir one hour back, so the cache trusts it.
func ageTree(t *testing.T, dir string) {
	t.Helper()
	at := time.Now().Add(-time.Hour)
	err := filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, at, at)
	})
	if err != nil {
		t.Fatal(err)
	}
}
