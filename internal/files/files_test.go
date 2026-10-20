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

func TestDeleteKeepsEmptyParent(t *testing.T) {
	t.Parallel()
	s := New(1 << 20)
	ctx := context.Background()
	root := t.TempDir()
	if _, _, err := s.Write(ctx, root, "a/b/c.txt", []byte("x"), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, root, "a/b/c.txt", ""); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(root, "a", "b")); err != nil || !st.IsDir() {
		t.Fatalf("empty a/b must stay: %v", err)
	}
	items, _, err := s.List(ctx, root, ListOpts{Tree: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(items); strings.Join(got, ",") != "a,a/b" {
		t.Fatalf("tree list = %v", got)
	}
}

func TestMkdir(t *testing.T) {
	t.Parallel()
	s := New(1 << 20)
	ctx := context.Background()
	root := t.TempDir()
	info, err := s.Mkdir(ctx, root, "reports/2026")
	if err != nil || info.Path != "reports/2026" || !info.Dir {
		t.Fatalf("mkdir: %+v %v", info, err)
	}
	if st, err := os.Stat(filepath.Join(root, "reports", "2026")); err != nil || !st.IsDir() {
		t.Fatalf("folder missing: %v", err)
	}
	if _, err := s.Mkdir(ctx, root, "reports/2026"); !errors.Is(err, ErrExists) {
		t.Fatalf("again: %v", err)
	}
	if _, _, err := s.Write(ctx, root, "f.txt", []byte("x"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mkdir(ctx, root, "f.txt"); !errors.Is(err, ErrExists) {
		t.Fatalf("over a file: %v", err)
	}
	for _, rel := range []string{"../x", "", ".", "/abs"} {
		if _, err := s.Mkdir(ctx, root, rel); !errors.Is(err, ErrEscapesRoot) {
			t.Errorf("mkdir %q: %v", rel, err)
		}
	}
	if _, err := s.Mkdir(ctx, root, "a/.git"); !errors.Is(err, ErrProtectedPath) {
		t.Fatalf("protected: %v", err)
	}
}

func TestMove(t *testing.T) {
	t.Parallel()
	s := New(1 << 20)
	ctx := context.Background()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		if _, _, err := s.Write(ctx, root, rel, []byte(body), ""); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "a")
	write("b.txt", "b")
	write("dir/one.txt", "1")
	write("dir/sub/two.txt", "2")

	info, err := s.Move(ctx, root, "a.txt", "notes/a.md", MoveOpts{})
	if err != nil || info.Path != "notes/a.md" || info.Hash != job.ContentHash([]byte("a")) {
		t.Fatalf("file move: %+v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("source still there")
	}
	if _, err := s.Move(ctx, root, "b.txt", "notes/a.md", MoveOpts{}); !errors.Is(err, ErrExists) {
		t.Fatalf("no overwrite: %v", err)
	}
	if _, err := s.Move(ctx, root, "b.txt", "notes/a.md", MoveOpts{IfMatch: "stale"}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	if _, err := s.Move(ctx, root, "b.txt", "notes/a.md", MoveOpts{Overwrite: true}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if data, _, _ := s.Read(ctx, root, "notes/a.md"); string(data) != "b" {
		t.Fatalf("overwrite content = %q", data)
	}
	if _, err := s.Move(ctx, root, "notes/a.md", "dir", MoveOpts{Overwrite: true}); !errors.Is(err, ErrExists) {
		t.Fatalf("file over folder: %v", err)
	}

	info, err = s.Move(ctx, root, "dir", "archive/dir", MoveOpts{})
	if err != nil || info.Path != "archive/dir" || !info.Dir {
		t.Fatalf("folder move: %+v %v", info, err)
	}
	if data, _, _ := s.Read(ctx, root, "archive/dir/sub/two.txt"); string(data) != "2" {
		t.Fatalf("moved content = %q", data)
	}
	for _, to := range []string{"archive/dir", "archive/dir/sub/x"} {
		if _, err := s.Move(ctx, root, "archive/dir", to, MoveOpts{}); !errors.Is(err, ErrIntoItself) {
			t.Errorf("into itself %q: %v", to, err)
		}
	}
	if _, err := s.Move(ctx, root, "missing.txt", "x.txt", MoveOpts{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := s.Move(ctx, root, "notes/a.md", "../out.md", MoveOpts{}); !errors.Is(err, ErrEscapesRoot) {
		t.Fatalf("escape: %v", err)
	}
	if _, err := s.Move(ctx, root, "notes/a.md", "trebi.yaml", MoveOpts{}); !errors.Is(err, ErrProtectedPath) {
		t.Fatalf("protected target: %v", err)
	}
}

func TestDeleteDir(t *testing.T) {
	t.Parallel()
	s := New(1 << 20)
	ctx := context.Background()
	root := t.TempDir()
	for _, rel := range []string{"r/a.txt", "r/b/c.txt", "r/node_modules/m.js", "keep.txt"} {
		if _, _, err := s.Write(ctx, root, rel, []byte("x"), ""); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := s.DirFiles(ctx, root, "r")
	if err != nil || strings.Join(paths, ",") != "r/a.txt,r/b/c.txt,r/node_modules/m.js" {
		t.Fatalf("dir files: %v %v", paths, err)
	}
	n, err := s.DeleteDir(ctx, root, "r")
	if err != nil || n != 3 {
		t.Fatalf("delete dir: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(root, "r")); !os.IsNotExist(err) {
		t.Fatal("folder still there")
	}
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Fatalf("sibling gone: %v", err)
	}
	if _, err := s.DeleteDir(ctx, root, "keep.txt"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("file: %v", err)
	}
	if _, err := s.DeleteDir(ctx, root, "r"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	for _, rel := range []string{".", "", "../"} {
		if _, err := s.DeleteDir(ctx, root, rel); !errors.Is(err, ErrEscapesRoot) {
			t.Errorf("root %q: %v", rel, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "repo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "repo", ".git", "HEAD"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteDir(ctx, root, "repo"); !errors.Is(err, ErrProtectedPath) {
		t.Fatalf("protected: %v", err)
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
