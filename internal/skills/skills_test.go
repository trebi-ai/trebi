package skills_test

import (
	"context"
	"os"
	"path/filepath"
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

	sk, err := svc.Create(ctx, "demo-skill", "# Demo\n\n- bin: sh\n", skills.ProvenanceLocal)
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "demo-skill" || sk.Hash == "" {
		t.Fatalf("%+v", sk)
	}
	if len(sk.Requires) != 1 || !sk.Requires[0].Satisfied {
		t.Fatalf("requires=%+v", sk.Requires)
	}

	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}

	got, err := svc.Get(ctx, "demo-skill", true)
	if err != nil || got.SkillMD == "" {
		t.Fatalf("%v %v", got, err)
	}

	_, err = svc.Update(ctx, "demo-skill", "# Demo v2\n", got.Hash, map[string]bool{"claude": true})
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home.HarnessDir("claude"), "skills", "demo-skill")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("projection missing: %v", err)
	}

	proj, err := skills.ProjectForRun([]string{"demo-skill"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(proj, "demo-skill")); err != nil {
		t.Fatal(err)
	}

	if err := svc.Delete(ctx, "demo-skill"); err != nil {
		t.Fatal(err)
	}
	list, _ = svc.List(ctx)
	if len(list) != 0 {
		t.Fatalf("after delete: %v", list)
	}
}
