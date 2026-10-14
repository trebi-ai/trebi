package uploads

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
)

// PNG is the smallest valid PNG header the type check accepts.
var PNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00") //nolint:gochecknoglobals // test fixture

func TestPutGetOpen(t *testing.T) {
	t.Parallel()
	s := newStore(t, 0)
	ctx := context.Background()
	u, err := s.Put(ctx, "ws1", bytes.NewReader(PNG))
	if err != nil {
		t.Fatal(err)
	}
	if u.MIME != "image/png" || u.Size != int64(len(PNG)) || u.SHA256 == "" {
		t.Fatalf("upload = %+v", u)
	}
	got, err := s.Get(ctx, "ws1", u.ID)
	if err != nil || got.MIME != "image/png" || got.Size != u.Size {
		t.Fatalf("get = %+v, %v", got, err)
	}
	f, _, err := s.Open(ctx, "ws1", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	_ = f.Close()
	if !bytes.Equal(b, PNG) {
		t.Fatalf("bytes = %q", b)
	}
	if _, err := s.Get(ctx, "ws2", u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other workspace err = %v", err)
	}
}

func TestPutRejects(t *testing.T) {
	t.Parallel()
	s := newStore(t, 64)
	ctx := context.Background()
	if _, err := s.Put(ctx, "ws1", bytes.NewReader([]byte("hello, plain text"))); !errors.Is(err, ErrType) {
		t.Fatalf("text err = %v", err)
	}
	big := append(append([]byte{}, PNG...), make([]byte, 100)...)
	if _, err := s.Put(ctx, "ws1", bytes.NewReader(big)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("large err = %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(s.dir, "ws1"))
	if len(entries) != 0 {
		t.Fatalf("a rejected upload left %d files", len(entries))
	}
}

func TestPruneKeepsAttached(t *testing.T) {
	t.Parallel()
	s := newStore(t, 0)
	ctx := context.Background()
	used, err := s.Put(ctx, "ws1", bytes.NewReader(PNG))
	if err != nil {
		t.Fatal(err)
	}
	unused, err := s.Put(ctx, "ws1", bytes.NewReader(PNG))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attach(ctx, "ws1", []string{used.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attach(ctx, "ws2", []string{unused.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attach across workspaces err = %v", err)
	}
	if n, err := s.Prune(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("fresh prune = %d, %v", n, err)
	}
	n, err := s.Prune(ctx, time.Now().Add(Keep+time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	if _, err := os.Stat(s.Path(unused)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unused file stays: %v", err)
	}
	if _, err := s.Get(ctx, "ws1", used.ID); err != nil {
		t.Fatalf("attached upload went: %v", err)
	}
}

func TestInline(t *testing.T) {
	t.Parallel()
	a := Attachments{{ID: "upl_1", MIME: "image/png", Path: "/h/uploads/ws/upl_1.png"}}
	if got := a.Inline("look\n"); got != "look\n[Image: /h/uploads/ws/upl_1.png]" {
		t.Fatalf("inline = %q", got)
	}
	if got := a.Inline(""); got != "[Image: /h/uploads/ws/upl_1.png]" {
		t.Fatalf("inline without text = %q", got)
	}
	if got := Attachments(nil).Inline("x"); got != "x" {
		t.Fatalf("no images = %q", got)
	}
}

func newStore(t *testing.T, maxBytes int64) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewStore(st, filepath.Join(dir, "uploads"), maxBytes)
}
