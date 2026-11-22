package skills

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFolderCacheHit: an equal stamp on an old folder reads no content, and a
// moved mtime reloads it.
func TestFolderCacheHit(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "s")
	writeSkill(t, dir, "# a\n")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	setTimes(t, dir, old)
	c := newFolderCache()
	if e, err := c.read(dir); err != nil || e.md != "# a\n" || len(e.scripts) != 1 {
		t.Fatalf("first read %+v %v", e, err)
	}
	md := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(md, []byte("# b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTimes(t, dir, old)
	if e, _ := c.read(dir); e.md != "# a\n" {
		t.Fatalf("equal stamp reloaded: %q", e.md)
	}
	if err := os.Chtimes(md, old.Add(time.Second), old.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if e, _ := c.read(dir); e.md != "# b\n" {
		t.Fatalf("moved stamp kept old content: %q", e.md)
	}
	c.forget(dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := c.read(dir); err == nil {
		t.Fatal("read of a removed folder gave no error")
	}
}

// TestFolderCacheRacyWindow: a folder that changed just before the read is
// never trusted, so a same-size rewrite in the same mtime tick shows.
func TestFolderCacheRacyWindow(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "s")
	writeSkill(t, dir, "# a\n")
	at := time.Now().Truncate(time.Second)
	setTimes(t, dir, at)
	c := newFolderCache()
	if e, _ := c.read(dir); e.md != "# a\n" {
		t.Fatalf("first read %q", e.md)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTimes(t, dir, at)
	if e, _ := c.read(dir); e.md != "# b\n" {
		t.Fatalf("racy folder served from cache: %q", e.md)
	}
}

func writeSkill(t *testing.T, dir, md string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "run.sh"), []byte("echo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setTimes(t *testing.T, dir string, at time.Time) {
	t.Helper()
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
