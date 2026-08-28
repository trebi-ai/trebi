package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

func TestCleanRel(t *testing.T) {
	t.Parallel()
	ok := []struct{ in, want string }{
		{"a/b.py", "a/b.py"},
		{"./a/b.py", "a/b.py"},
		{"a//b.py", "a/b.py"},
		{"a/./b.py", "a/b.py"},
	}
	for _, c := range ok {
		got, err := cleanRel(c.in)
		if err != nil || got != c.want {
			t.Errorf("cleanRel(%q) = %q %v, want %q", c.in, got, err, c.want)
		}
	}
	bad := []string{"", "../x", "a/../../x", "..", "/etc/passwd", `a\..\..\x`, `a\b.py`}
	for _, in := range bad {
		if _, err := cleanRel(in); !errors.Is(err, ErrEscapesRoot) {
			t.Errorf("cleanRel(%q) err=%v, want ErrEscapesRoot", in, err)
		}
	}
}

func TestUnderRoot(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if !underRoot(root, filepath.Join(root, "a.py")) {
		t.Fatal("child should be under root")
	}
	if !underRoot(root, root) {
		t.Fatal("root should be under itself")
	}
	if underRoot(root, filepath.Join(base, "ws-evil", "f")) {
		t.Fatal("/ws must not authorize /ws-evil")
	}
}

func TestJailWrite(t *testing.T) {
	t.Parallel()
	s := New(1 << 20)
	ctx := context.Background()
	root := t.TempDir()

	info, created, err := s.Write(ctx, root, "a/b.py", []byte("hello"), "")
	if err != nil || !created || info.Path != "a/b.py" {
		t.Fatalf("write: created=%v info=%+v err=%v", created, info, err)
	}
	data, _, err := s.Read(ctx, root, "a/b.py")
	if err != nil || string(data) != "hello" {
		t.Fatalf("read: %q %v", data, err)
	}

	for _, rel := range []string{"../x", "a/../../x", "..", "/etc/passwd", `a\..\..\x`} {
		if _, _, err := s.Write(ctx, root, rel, []byte("x"), ""); !errors.Is(err, ErrEscapesRoot) {
			t.Errorf("write %q: %v", rel, err)
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if strings.Contains(msg, root) {
			t.Errorf("write %q echoed absolute path: %s", rel, msg)
		}
	}
}

func TestSymlinkEscape(t *testing.T) {
	t.Parallel()
	s := New(1 << 20)
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Write(ctx, root, "link/f", []byte("nope"), ""); !errors.Is(err, ErrEscapesRoot) {
		t.Fatalf("symlink escape: %v", err)
	}
}

func TestSymlinkInsideRoot(t *testing.T) {
	t.Parallel()
	s := New(1 << 20)
	ctx := context.Background()
	root := t.TempDir()
	other := filepath.Join(root, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Write(ctx, root, "link/f", []byte("ok"), ""); err != nil {
		t.Fatalf("inside symlink: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(other, "f"))
	if err != nil || string(got) != "ok" {
		t.Fatalf("target: %s %v", got, err)
	}
}

func TestProtectedPaths(t *testing.T) {
	t.Parallel()
	s := New(1 << 20)
	ctx := context.Background()
	root := t.TempDir()
	for _, rel := range []string{"trebi.yaml", ".git/config", ".trebi-write-abc"} {
		if _, _, err := s.Write(ctx, root, rel, []byte("x"), ""); !errors.Is(err, ErrProtectedPath) {
			t.Errorf("protected %q: %v", rel, err)
		}
	}
	if _, _, err := s.Write(ctx, root, "nested/trebi.yaml", []byte("x"), ""); err != nil {
		t.Fatalf("nested trebi.yaml should be allowed: %v", err)
	}
}

func TestIsDir(t *testing.T) {
	t.Parallel()
	s := New(1 << 20)
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read(ctx, root, "dir"); !errors.Is(err, ErrIsDir) {
		t.Fatalf("read dir: %v", err)
	}
	if _, _, err := s.Write(ctx, root, "dir", []byte("x"), ""); !errors.Is(err, ErrIsDir) {
		t.Fatalf("write dir: %v", err)
	}
}

func TestTooLarge(t *testing.T) {
	t.Parallel()
	s := New(8)
	ctx := context.Background()
	root := t.TempDir()
	if _, _, err := s.Write(ctx, root, "big.bin", []byte("0123456789"), ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("write: %v", err)
	}
}

func TestIfMatch(t *testing.T) {
	t.Parallel()
	s := New(1 << 20)
	ctx := context.Background()
	root := t.TempDir()
	body := []byte("v1")
	info, _, err := s.Write(ctx, root, "f.txt", body, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Write(ctx, root, "f.txt", []byte("v2"), "deadbeef")
	if !errors.Is(err, ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	var se *StaleError
	if !errors.As(err, &se) || se.Hash != info.Hash {
		t.Fatalf("stale hash: %+v (want %s)", se, info.Hash)
	}
	if _, _, err := s.Write(ctx, root, "f.txt", []byte("v2"), info.Hash); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if _, _, err := s.Write(ctx, root, "missing.txt", []byte("x"), "*"); !errors.Is(err, ErrStale) {
		t.Fatalf("star on missing: %v", err)
	}
	if _, created, err := s.Write(ctx, root, "f.txt", []byte("v3"), "*"); err != nil || created {
		t.Fatalf("star on existing: created=%v err=%v", created, err)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	s := New(1 << 20)
	ctx := context.Background()
	root := t.TempDir()
	if err := s.Delete(ctx, root, "nope.txt", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, _, err := s.Write(ctx, root, "f.txt", []byte("x"), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, root, "f.txt", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "f.txt")); !os.IsNotExist(err) {
		t.Fatal("still there")
	}
}

func TestListPagination(t *testing.T) {
	t.Parallel()
	s := New(4)
	ctx := context.Background()
	root := t.TempDir()
	for _, rel := range []string{"a.txt", "b.txt", "reports/c.txt", "reports/d.txt"} {
		if _, _, err := s.Write(ctx, root, rel, []byte("ok"), ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".trebi-write-tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "huge.bin"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	items, next, err := s.List(ctx, root, ListOpts{Limit: 2})
	if err != nil || len(items) != 2 || next != items[1].Path {
		t.Fatalf("page1: n=%d next=%q err=%v items=%v", len(items), next, err, names(items))
	}
	items2, next2, err := s.List(ctx, root, ListOpts{Limit: 10, AfterPath: next})
	if err != nil || next2 != "" {
		t.Fatalf("page2 next=%q err=%v", next2, err)
	}
	for _, it := range append(items, items2...) {
		if strings.HasPrefix(it.Path, ".git/") || strings.HasPrefix(it.Path, ".trebi-write-") {
			t.Fatalf("skipped path leaked: %s", it.Path)
		}
	}
	prefixed, _, err := s.List(ctx, root, ListOpts{Prefix: "reports/", Limit: 10})
	if err != nil || len(prefixed) != 2 {
		t.Fatalf("prefix: %v %v", names(prefixed), err)
	}

	all, _, err := s.List(ctx, root, ListOpts{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var huge *Info
	for i := range all {
		if all[i].Path == "huge.bin" {
			huge = &all[i]
		}
	}
	if huge == nil || !huge.Oversize {
		t.Fatalf("oversize flag: %+v", huge)
	}
}

func TestStatHashMatchesContentHash(t *testing.T) {
	t.Parallel()
	s := New(1 << 20)
	ctx := context.Background()
	root := t.TempDir()
	body := []byte("abc")
	if _, _, err := s.Write(ctx, root, "f.txt", body, ""); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(ctx, root, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Hash != job.ContentHash(body) {
		t.Fatalf("hash %s want %s", info.Hash, job.ContentHash(body))
	}
}

func names(items []Info) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Path
	}
	return out
}
