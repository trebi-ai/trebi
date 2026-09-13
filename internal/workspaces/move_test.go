package workspaces_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/workspaces"
)

func TestRewritePathPrefix(t *testing.T) {
	t.Parallel()
	old, neu := "/tmp/old-ws", "/tmp/new-ws"
	cases := []struct {
		in, want string
		ok       bool
	}{
		{old, neu, true},
		{old + "/job/run.sh", neu + "/job/run.sh", true},
		{"cwd: " + old + "\n", "cwd: " + neu + "\n", true},
		{old + "-extra", old + "-extra", false},
		{old + ".bak", old + ".bak", false},
		{"/tmp/old-ws-2/x", "/tmp/old-ws-2/x", false},
		{"nothing here", "nothing here", false},
		{"working_dir: " + old, "working_dir: " + neu, true},
	}
	for _, c := range cases {
		got, ok := workspaces.RewritePathPrefix(c.in, old, neu)
		if got != c.want || ok != c.ok {
			t.Errorf("in %q: got (%q, %v) want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestMoveRelocateRewritesFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	parent := t.TempDir()
	src := filepath.Join(parent, "src")
	writeFile(t, filepath.Join(src, "alpha", "JOB.md"),
		"---\nharness: claude\nworking_dir: "+src+"\n---\n\nRun "+src+"/alpha/run.sh\n")
	writeFile(t, filepath.Join(src, "alpha", "run.sh"), "#!/bin/sh\necho "+src+"\n")
	writeFile(t, filepath.Join(src, "alpha", "blob.bin"), "x\x00y")

	ws, err := svc.Add(ctx, src, "demo")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(parent, "dest")
	preview, err := svc.Move(ctx, ws.ID, workspaces.MoveOpts{Path: dest, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.DryRun || preview.Path != dest {
		t.Fatalf("dry-run %+v", preview)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("dry-run moved the folder")
	}
	if !containsAll(preview.Rewritten, "alpha/JOB.md", "alpha/run.sh") {
		t.Fatalf("rewritten %v", preview.Rewritten)
	}
	for _, rel := range preview.Rewritten {
		if rel == "alpha/blob.bin" {
			t.Fatal("rewrote binary")
		}
	}

	got, err := svc.Move(ctx, ws.ID, workspaces.MoveOpts{Path: dest})
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != dest {
		t.Fatalf("path %s", got.Path)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("src still exists: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "alpha", "JOB.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), src) || !strings.Contains(string(body), dest) {
		t.Fatalf("JOB.md not rewritten: %s", body)
	}
	script, err := os.ReadFile(filepath.Join(dest, "alpha", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), src) {
		t.Fatalf("script still has old path: %s", script)
	}
	fresh, err := svc.Get(ctx, ws.ID)
	if err != nil || fresh.Path != dest {
		t.Fatalf("registry %v %v", fresh, err)
	}
	jobs, err := svc.ListJobs(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || !strings.HasPrefix(jobs[0].Path, dest) {
		t.Fatalf("job_index path %v", jobs)
	}
}

func TestMoveRebind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	parent := t.TempDir()
	src := filepath.Join(parent, "src")
	writeFile(t, filepath.Join(src, "alpha", "JOB.md"),
		"---\nharness: claude\nworking_dir: "+src+"\n---\n\nDo it.\n")
	ws, err := svc.Add(ctx, src, "demo")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(parent, "dest")
	if err := os.Rename(src, dest); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Move(ctx, ws.Name, workspaces.MoveOpts{Path: dest})
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != dest {
		t.Fatalf("path %s", got.Path)
	}
	body, _ := os.ReadFile(filepath.Join(dest, "alpha", "JOB.md"))
	if strings.Contains(string(body), src) {
		t.Fatalf("not rewritten: %s", body)
	}
}

func TestMoveRefusesNestedAndSame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	src := t.TempDir()
	ws, err := svc.Add(ctx, src, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Move(ctx, ws.ID, workspaces.MoveOpts{Path: src}); err == nil || !strings.Contains(err.Error(), "current path") {
		t.Fatalf("same path: %v", err)
	}
	if _, err := svc.Move(ctx, ws.ID, workspaces.MoveOpts{Path: filepath.Join(src, "nested")}); err == nil {
		t.Fatal("want dest inside")
	}
	other := t.TempDir()
	if _, err := svc.Add(ctx, other, "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Move(ctx, ws.ID, workspaces.MoveOpts{Path: other}); err == nil {
		t.Fatal("want dest exists")
	}
}

func TestMoveMkdirParent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "alpha", "JOB.md"), jobMD)
	ws, err := svc.Add(ctx, src, "demo")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "missing", "dest")
	if _, err := svc.Move(ctx, ws.ID, workspaces.MoveOpts{Path: dest}); err == nil {
		t.Fatal("want parent missing")
	}
	got, err := svc.Move(ctx, ws.ID, workspaces.MoveOpts{Path: dest, Mkdir: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != dest {
		t.Fatalf("path %s", got.Path)
	}
}

func containsAll(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}
