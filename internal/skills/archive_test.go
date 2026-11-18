package skills_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
)

// fakeOwners serves one workspace, one agent, and one job from temp folders.
type fakeOwners struct{ list []skills.Owner }

func (f fakeOwners) SkillOwners(context.Context) ([]skills.Owner, error) { return f.list, nil }

func (f fakeOwners) SkillOwner(_ context.Context, scope skills.Scope, id string) (skills.Owner, error) {
	for _, o := range f.list {
		if o.Kind == scope && o.ID == id {
			return o, nil
		}
	}
	return skills.Owner{}, os.ErrNotExist
}

type archiveEnv struct {
	svc *skills.Service
	st  *store.Store
	ctx context.Context
}

func (e archiveEnv) create(t *testing.T, ref skills.Ref, md string) {
	t.Helper()
	if _, err := e.svc.Create(e.ctx, skills.CreateOpts{Ref: ref, SkillMD: md, Provenance: skills.ProvenanceLocal}); err != nil {
		t.Fatal(err)
	}
}

func (e archiveEnv) indexArchived(t *testing.T, ref skills.Ref) string {
	t.Helper()
	var at *string
	if err := e.st.DB().QueryRow(`SELECT archived_at FROM skill_index WHERE ref=?`, ref.String()).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at == nil {
		return ""
	}
	return *at
}

func TestSkillArchive(t *testing.T) {
	e := newArchiveEnv(t)
	ref := skills.GlobalRef("deploy")
	e.create(t, ref, "---\ndescription: Deploy\n---\n# Deploy\n")
	at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	sk, err := e.svc.Archive(e.ctx, ref, "", at)
	if err != nil {
		t.Fatal(err)
	}
	if sk.ArchivedAt != "2026-11-18T09:00:00Z" || sk.Description != "Deploy" {
		t.Fatalf("skill %+v", sk)
	}
	if md := readMD(t, sk.Path); !strings.Contains(md, "archived_at: 2026-11-18T09:00:00Z\n") || !strings.Contains(md, "description: Deploy") {
		t.Fatalf("SKILL.md %q", md)
	}
	if got := e.indexArchived(t, ref); got != "2026-11-18T09:00:00Z" {
		t.Fatalf("index archived_at %q", got)
	}
	var attrs string
	if err := e.st.DB().QueryRow(`SELECT attrs_json FROM event_log WHERE topic='skill.updated' ORDER BY seq DESC LIMIT 1`).Scan(&attrs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(attrs, `"archived":"true"`) || !strings.Contains(attrs, "2026-11-18T09:00:00Z") {
		t.Fatalf("attrs %s", attrs)
	}
	// A second archive keeps the first time.
	sk, err = e.svc.Archive(e.ctx, ref, "", at.Add(time.Hour))
	if err != nil || sk.ArchivedAt != "2026-11-18T09:00:00Z" {
		t.Fatalf("second archive: %+v %v", sk, err)
	}
	sk, err = e.svc.Unarchive(e.ctx, ref, "")
	if err != nil || sk.ArchivedAt != "" {
		t.Fatalf("unarchive: %+v %v", sk, err)
	}
	if md := readMD(t, sk.Path); md != "---\ndescription: Deploy\n---\n# Deploy\n" {
		t.Fatalf("SKILL.md after unarchive %q", md)
	}
	if got := e.indexArchived(t, ref); got != "" {
		t.Fatalf("index archived_at %q", got)
	}
	if err := e.st.DB().QueryRow(`SELECT attrs_json FROM event_log WHERE topic='skill.updated' ORDER BY seq DESC LIMIT 1`).Scan(&attrs); err != nil || !strings.Contains(attrs, `"archived":"false"`) {
		t.Fatalf("attrs %s %v", attrs, err)
	}
}

func TestSkillArchiveIfMatch(t *testing.T) {
	e := newArchiveEnv(t)
	ref := skills.GlobalRef("deploy")
	e.create(t, ref, "# Deploy\n")
	if _, err := e.svc.Archive(e.ctx, ref, "stale", time.Now()); !errors.Is(err, skills.ErrPrecondition) {
		t.Fatalf("err %v", err)
	}
	sk, _ := e.svc.Get(e.ctx, ref, false)
	if _, err := e.svc.Archive(e.ctx, ref, sk.Hash, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestSkillArchiveRefusesConnector(t *testing.T) {
	e := newArchiveEnv(t)
	if _, err := e.svc.AddConnectorSkill(e.ctx, "plane", "# Plane\n", "plane"); err != nil {
		t.Fatal(err)
	}
	ref := skills.GlobalRef("plane")
	before := readMD(t, filepath.Join(home.SkillsDir(), "plane"))
	if _, err := e.svc.Archive(e.ctx, ref, "", time.Now()); !errors.Is(err, skills.ErrManaged) {
		t.Fatalf("err %v", err)
	}
	if got := readMD(t, filepath.Join(home.SkillsDir(), "plane")); got != before {
		t.Fatalf("file changed: %q", got)
	}
}

func TestSkillArchiveRefusesInherited(t *testing.T) {
	e := newArchiveEnv(t)
	user := t.TempDir()
	dir := filepath.Join(user, ".claude", "skills", "sling")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# Sling\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.svc.WithUserHome(user)
	if _, err := e.svc.Archive(e.ctx, skills.GlobalRef("sling"), "", time.Now()); !errors.Is(err, skills.ErrInherited) {
		t.Fatalf("archive: %v", err)
	}
	if _, err := e.svc.Unarchive(e.ctx, skills.GlobalRef("sling"), ""); !errors.Is(err, skills.ErrInherited) {
		t.Fatalf("unarchive: %v", err)
	}
}

func TestSkillArchiveGlobalLinks(t *testing.T) {
	e := newArchiveEnv(t)
	ref := skills.GlobalRef("deploy")
	e.create(t, ref, "# Deploy\n")
	if _, err := e.svc.Update(e.ctx, ref, "", "", map[string]bool{"claude": true, "codex": true}); err != nil {
		t.Fatal(err)
	}
	link := func(h, sub string) string { return filepath.Join(home.HarnessDir(h), sub, "deploy") }
	sk, err := e.svc.Archive(e.ctx, ref, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"claude", "codex"} {
		if _, err := os.Lstat(link(h, "skills")); err == nil {
			t.Fatalf("%s link stays", h)
		}
	}
	if md := readMD(t, sk.Path); !strings.Contains(md, "archived_links: claude,codex\n") {
		t.Fatalf("SKILL.md %q", md)
	}
	if sk.Harnesses["claude"] || sk.Harnesses["codex"] {
		t.Fatalf("harnesses %v", sk.Harnesses)
	}
	sk, err = e.svc.Unarchive(e.ctx, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"claude", "codex"} {
		if _, err := os.Lstat(link(h, "skills")); err != nil {
			t.Fatalf("%s link missing: %v", h, err)
		}
	}
	if _, err := os.Lstat(link("gemini", "skills")); err == nil {
		t.Fatal("unarchive linked a harness that had no link")
	}
	if md := readMD(t, sk.Path); strings.Contains(md, "archived") {
		t.Fatalf("keys stay: %q", md)
	}
}

func TestSkillUpdateKeepsArchived(t *testing.T) {
	e := newArchiveEnv(t)
	ref := skills.GlobalRef("deploy")
	e.create(t, ref, "# Deploy\n")
	if _, err := e.svc.Update(e.ctx, ref, "", "", map[string]bool{"claude": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Archive(e.ctx, ref, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	sk, err := e.svc.Update(e.ctx, ref, "---\ndescription: New\n---\n# New\n", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	md := readMD(t, sk.Path)
	if sk.ArchivedAt == "" || !strings.Contains(md, "archived_at:") || !strings.Contains(md, "archived_links: claude") || !strings.Contains(md, "description: New") {
		t.Fatalf("skill %+v md %q", sk, md)
	}
	if _, err := e.svc.Update(e.ctx, ref, "", "", map[string]bool{"claude": true}); !errors.Is(err, skills.ErrArchived) {
		t.Fatalf("harness change: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home.HarnessDir("claude"), "skills", "deploy")); err == nil {
		t.Fatal("harness change linked an archived skill")
	}
}

func TestSkillRefreshKeepsArchived(t *testing.T) {
	e := newArchiveEnv(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("# Src\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := skills.GlobalRef("synced")
	if _, err := e.svc.Create(e.ctx, skills.CreateOpts{Ref: ref, Source: &skills.Source{Kind: "folder", Path: src}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Update(e.ctx, ref, "", "", map[string]bool{"cursor": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Archive(e.ctx, ref, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("# Src v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sk, updated, err := e.svc.Refresh(e.ctx, ref)
	if err != nil || !updated {
		t.Fatalf("refresh: updated=%v err=%v", updated, err)
	}
	md := readMD(t, sk.Path)
	if sk.ArchivedAt == "" || !strings.Contains(md, "archived_links: cursor") || !strings.Contains(md, "# Src v2") {
		t.Fatalf("skill %+v md %q", sk, md)
	}
	// A refresh with no change in the source is no change.
	if _, updated, err := e.svc.Refresh(e.ctx, ref); err != nil || updated {
		t.Fatalf("second refresh: updated=%v err=%v", updated, err)
	}
}

func TestResolveSkipsArchived(t *testing.T) {
	e := newArchiveEnv(t)
	e.create(t, skills.GlobalRef("deploy"), "# Global\n")
	narrow := skills.Ref{Scope: skills.ScopeJob, Owner: "job", Name: "deploy"}
	e.create(t, narrow, "# Job\n")
	rs := skills.RunScope{WorkspaceID: "ws", JobID: "job"}
	used := func() string {
		list, err := e.svc.Resolve(e.ctx, rs)
		if err != nil {
			t.Fatal(err)
		}
		var refs []string
		for _, r := range list {
			if r.Used {
				refs = append(refs, r.Ref)
			}
		}
		return strings.Join(refs, ",")
	}
	if got := used(); got != narrow.String() {
		t.Fatalf("before: %q", got)
	}
	if _, err := e.svc.Archive(e.ctx, narrow, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := used(); got != "global/deploy" {
		t.Fatalf("archived: %q", got)
	}
	dir, err := e.svc.ProjectForRun(e.ctx, rs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(dir, "deploy"))
	if err != nil || target != filepath.Join(home.SkillsDir(), "deploy") {
		t.Fatalf("projected %q %v", target, err)
	}
	if _, err := e.svc.Unarchive(e.ctx, narrow, ""); err != nil {
		t.Fatal(err)
	}
	if got := used(); got != narrow.String() {
		t.Fatalf("after unarchive: %q", got)
	}
}

func TestArchivedForNamesAskedSkills(t *testing.T) {
	e := newArchiveEnv(t)
	jobRef := skills.Ref{Scope: skills.ScopeJob, Owner: "job", Name: "j-skill"}
	agentRef := skills.Ref{Scope: skills.ScopeAgent, Owner: "agt", Name: "a-skill"}
	wsRef := skills.Ref{Scope: skills.ScopeWorkspace, Owner: "ws", Name: "w-skill"}
	e.create(t, jobRef, "# J\n")
	e.create(t, agentRef, "# A\n")
	e.create(t, wsRef, "# W\n")
	e.create(t, skills.GlobalRef("asked"), "# G\n")
	e.create(t, skills.GlobalRef("unasked"), "# G\n")
	e.create(t, skills.GlobalRef("active"), "# G\n")
	for _, r := range []skills.Ref{jobRef, agentRef, wsRef, skills.GlobalRef("asked"), skills.GlobalRef("unasked")} {
		if _, err := e.svc.Archive(e.ctx, r, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	list, err := e.svc.ArchivedFor(e.ctx, skills.RunScope{WorkspaceID: "ws", JobID: "job"}, []string{"asked", "active"})
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, sk := range list {
		refs = append(refs, sk.Ref)
	}
	slices.Sort(refs)
	want := []string{agentRef.String(), "global/asked", jobRef.String()}
	slices.Sort(want)
	if !slices.Equal(refs, want) {
		t.Fatalf("refs %v, want %v", refs, want)
	}
}

func TestListArchivedFilter(t *testing.T) {
	e := newArchiveEnv(t)
	e.create(t, skills.GlobalRef("one"), "# 1\n")
	e.create(t, skills.GlobalRef("two"), "# 2\n")
	if _, err := e.svc.Archive(e.ctx, skills.GlobalRef("two"), "", time.Now()); err != nil {
		t.Fatal(err)
	}
	names := func(view string) []string {
		list, err := e.svc.List(e.ctx, skills.ListOpts{Archived: view})
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
	for view, want := range map[string][]string{"": {"one"}, "false": {"one"}, "true": {"two"}, "all": {"one", "two"}} {
		if got := names(view); !slices.Equal(got, want) {
			t.Fatalf("archived=%q: %v, want %v", view, got, want)
		}
	}
}

func TestArchivedSkillNameReserved(t *testing.T) {
	e := newArchiveEnv(t)
	ref := skills.Ref{Scope: skills.ScopeAgent, Owner: "agt", Name: "keep"}
	e.create(t, ref, "# Keep\n")
	if _, err := e.svc.Archive(e.ctx, ref, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Create(e.ctx, skills.CreateOpts{Ref: ref, SkillMD: "# Again\n", Provenance: skills.ProvenanceLocal})
	if !errors.Is(err, skills.ErrExists) {
		t.Fatalf("err %v", err)
	}
	// Name lookup still finds the archived skill.
	list, err := e.svc.Named(e.ctx, "keep")
	if err != nil || len(list) != 1 {
		t.Fatalf("named %v %v", list, err)
	}
}

func TestSkillOverridesIgnoreArchived(t *testing.T) {
	e := newArchiveEnv(t)
	e.create(t, skills.GlobalRef("deploy"), "# G\n")
	narrow := skills.Ref{Scope: skills.ScopeJob, Owner: "job", Name: "deploy"}
	e.create(t, narrow, "# J\n")
	if _, err := e.svc.Archive(e.ctx, narrow, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	g, err := e.svc.Get(e.ctx, skills.GlobalRef("deploy"), false)
	if err != nil || len(g.OverriddenBy) != 0 {
		t.Fatalf("global %+v %v", g, err)
	}
}

func newArchiveEnv(t *testing.T) archiveEnv {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	owners := fakeOwners{list: []skills.Owner{
		{Kind: skills.ScopeWorkspace, WorkspaceID: "ws", ID: "ws", Name: "ws", Dir: filepath.Join(dir, "ws")},
		{Kind: skills.ScopeAgent, WorkspaceID: "ws", ID: "agt", Name: "agents/a", Dir: filepath.Join(dir, "ws", "agents", "a")},
		{Kind: skills.ScopeJob, WorkspaceID: "ws", ID: "job", Name: "agents/a/j", AgentID: "agt", Dir: filepath.Join(dir, "ws", "agents", "a", "j")},
	}}
	return archiveEnv{svc: skills.New(st).WithOwners(owners), st: st, ctx: context.Background()}
}

func readMD(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
