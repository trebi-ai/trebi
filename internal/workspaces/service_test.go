package workspaces_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

const jobMD = "---\nharness: claude\n---\n\nDo the task.\n"

func TestRebuildIndexIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "alpha", "JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "beta", "JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "TREBI.md"), "---\nharness: claude\n---\n\nAssist.\n")

	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	seq := mustSeq(t, st)

	// Second rebuild with no changes: no events, no row churn.
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if got := mustSeq(t, st); got != seq {
		t.Fatalf("bus grew on no-op rebuild: %v", busTopics(t, st, seq))
	}
}

func TestRebuildIndexEmitsDiffEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "alpha", "JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "beta", "JOB.md"), jobMD)

	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}

	// Edit one job file: exactly one job.updated.
	seq := mustSeq(t, st)
	writeFile(t, filepath.Join(root, "alpha", "JOB.md"), jobMD+"\nMore work.\n")
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	topics := busTopics(t, st, seq)
	if len(topics) != 1 || topics[0] != "job.updated:alpha" {
		t.Fatalf("topics=%v want [job.updated:alpha]", topics)
	}

	// Remove one job file: exactly one job.deleted.
	seq = mustSeq(t, st)
	if err := os.RemoveAll(filepath.Join(root, "beta")); err != nil {
		t.Fatal(err)
	}
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	topics = busTopics(t, st, seq)
	if len(topics) != 1 || topics[0] != "job.deleted:beta" {
		t.Fatalf("topics=%v want [job.deleted:beta]", topics)
	}

	// New job file: exactly one job.created.
	seq = mustSeq(t, st)
	writeFile(t, filepath.Join(root, "gamma", "JOB.md"), jobMD)
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	topics = busTopics(t, st, seq)
	if len(topics) != 1 || topics[0] != "job.created:gamma" {
		t.Fatalf("topics=%v want [job.created:gamma]", topics)
	}

	// New assistant file: assistant.updated.
	seq = mustSeq(t, st)
	writeFile(t, filepath.Join(root, "TREBI.md"), "---\nharness: claude\n---\n\nAssist.\n")
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatal(err)
	}
	topics = busTopics(t, st, seq)
	if len(topics) != 1 || !strings.HasPrefix(topics[0], "assistant.updated:") {
		t.Fatalf("topics=%v want [assistant.updated:*]", topics)
	}
}

func TestRebuildIndexSkipsNestedWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "alpha", "JOB.md"), jobMD)
	writeFile(t, filepath.Join(root, "sub", "nested", "JOB.md"), jobMD)

	wsA, err := svc.Add(ctx, root, "A")
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id, name, path, git_enabled, created_at, updated_at)
		VALUES (?,?,?,?,?,?)`, "ws_nested", "B", sub, 0, now, now); err != nil {
		t.Fatal(err)
	}
	wsB, err := svc.Get(ctx, "ws_nested")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RebuildIndex(ctx, wsB); err != nil {
		t.Fatal(err)
	}

	// Remove B's trebi.yaml marker if present: the registry boundary must still hold.
	if err := os.Remove(filepath.Join(root, "sub", "trebi.yaml")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := svc.RebuildIndex(ctx, wsA); err != nil {
		t.Fatal(err)
	}

	rowsA, err := svc.ListJobs(ctx, wsA.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rowsA {
		if strings.HasPrefix(r.Name, "sub/") {
			t.Fatalf("A's index contains B's job: %s", r.Name)
		}
	}
	rowsB, err := svc.ListJobs(ctx, wsB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsB) != 1 || rowsB[0].Name != "nested" {
		t.Fatalf("B rows=%v want [nested]", rowsB)
	}
}

func TestAddIDCollision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	dir1 := t.TempDir()
	dir2 := t.TempDir()
	if _, err := svc.Add(ctx, dir1, "one"); err != nil {
		t.Fatal(err)
	}

	// Re-add same path: idempotent no-op, no new events.
	seq := mustSeq(t, st)
	if _, err := svc.Add(ctx, dir1, "one"); err != nil {
		t.Fatal(err)
	}
	if got := mustSeq(t, st); got != seq {
		t.Fatalf("bus grew on idempotent re-add: %v", busTopics(t, st, seq))
	}

	// Copied trebi.yaml (same id, different path): error names existing path.
	data, err := os.ReadFile(filepath.Join(dir1, "trebi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir2, "trebi.yaml"), string(data))
	_, err = svc.Add(ctx, dir2, "two")
	if err == nil {
		t.Fatal("want id-collision error")
	}
	if !strings.Contains(err.Error(), dir1) {
		t.Fatalf("error %q does not name existing path %s", err, dir1)
	}
}

func TestRemoveEmitsWorkspaceDeleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "alpha", "JOB.md"), jobMD)
	ws, err := svc.Add(ctx, root, "demo")
	if err != nil {
		t.Fatal(err)
	}

	seq := mustSeq(t, st)
	if err := svc.Remove(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	topics := busTopics(t, st, seq)
	if len(topics) != 1 || topics[0] != "workspace.deleted:"+ws.ID {
		t.Fatalf("topics=%v want [workspace.deleted:%s]", topics, ws.ID)
	}
	if _, err := svc.Get(ctx, ws.ID); err == nil {
		t.Fatal("workspace still present after Remove")
	}
	rows, err := svc.ListJobs(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("job_index rows remain: %v", rows)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("workspace folder still present: %v", err)
	}
}

func TestRemoveDeletesCreatedFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	root := t.TempDir()
	svc := workspaces.New(st).WithRoot(root)

	ws, err := svc.AddWith(ctx, workspaces.AddOpts{Name: "test", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "trebi.yaml")); err != nil {
		t.Fatalf("trebi.yaml: %v", err)
	}
	if err := svc.Remove(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Path); !os.IsNotExist(err) {
		t.Fatalf("created folder still present: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("workspaces.root should remain: %v", err)
	}
}

func TestRemoveMissingFolderStillUnregisters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)
	root := t.TempDir()
	ws, err := svc.Add(ctx, root, "gone")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := svc.Remove(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, ws.ID); err == nil {
		t.Fatal("workspace still present after Remove")
	}
}

func TestRenameBumpsRevisionAndBusFrame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	root := t.TempDir()
	ws, err := svc.Add(ctx, root, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Revision != 1 {
		t.Fatalf("create revision=%d want 1", ws.Revision)
	}

	seq := mustSeq(t, st)
	got, err := svc.Rename(ctx, ws.ID, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "beta" || got.Revision != 2 {
		t.Fatalf("first rename %+v", got)
	}
	evs, err := st.ListBus(ctx, seq, []string{"workspace.updated"}, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("bus %d %v", len(evs), err)
	}
	if evs[0].Revision != 2 || evs[0].ID != ws.ID {
		t.Fatalf("frame %+v", evs[0])
	}

	got, err = svc.Rename(ctx, ws.ID, "gamma")
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 3 {
		t.Fatalf("second rename revision=%d want 3", got.Revision)
	}

	seq = mustSeq(t, st)
	if _, err := svc.Rename(ctx, "ws_nope", "x"); !errors.Is(err, workspaces.ErrNotFound) {
		t.Fatalf("missing id: %v", err)
	}
	n, err := st.ListBus(ctx, seq, nil, 10)
	if err != nil || len(n) != 0 {
		t.Fatalf("not-found must not emit: %d %v", len(n), err)
	}
}

func TestAddCreateAndGuards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	root := t.TempDir()
	svc := workspaces.New(st).WithRoot(root)

	missing := filepath.Join(root, "fresh")
	if _, err := svc.AddWith(ctx, workspaces.AddOpts{Path: missing, Name: "fresh", Create: true}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(missing); err != nil || !st.IsDir() {
		t.Fatalf("mkdir: %v %v", st, err)
	}
	seq := mustSeq(t, st)
	if _, err := svc.AddWith(ctx, workspaces.AddOpts{Path: missing, Name: "fresh", Create: true}); err != nil {
		t.Fatal(err)
	}
	if got := mustSeq(t, st); got != seq {
		t.Fatalf("bus grew on idempotent create: %v", busTopics(t, st, seq))
	}

	inside := filepath.Join(missing, "nested")
	if _, err := svc.AddWith(ctx, workspaces.AddOpts{Path: inside, Name: "nested", Create: true}); !errors.Is(err, workspaces.ErrNested) {
		t.Fatalf("nested: %v", err)
	}

	pathless, err := svc.AddWith(ctx, workspaces.AddOpts{Name: "reports", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if pathless.Path != filepath.Join(root, "reports") {
		t.Fatalf("pathless path %s", pathless.Path)
	}

	bare := workspaces.New(st)
	if _, err := bare.AddWith(ctx, workspaces.AddOpts{Name: "x", Create: true}); !errors.Is(err, workspaces.ErrNoRoot) {
		t.Fatalf("no root: %v", err)
	}

	filePath := filepath.Join(root, "not-a-dir")
	writeFile(t, filePath, "x")
	if _, err := svc.AddWith(ctx, workspaces.AddOpts{Path: filepath.Join(filePath, "sub"), Name: "bad", Create: true}); !errors.Is(err, workspaces.ErrAncestorFile) {
		t.Fatalf("ancestor file: %v", err)
	}

	child := filepath.Join(root, "child")
	if _, err := svc.AddWith(ctx, workspaces.AddOpts{Path: child, Name: "child", Create: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddWith(ctx, workspaces.AddOpts{Path: root, Name: "parent"}); !errors.Is(err, workspaces.ErrContains) {
		t.Fatalf("contains: %v", err)
	}
}

func TestPatchWritesTrebiYAML(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)
	root := t.TempDir()
	ws, err := svc.Add(ctx, root, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	on := true
	got, err := svc.Patch(ctx, ws.ID, workspaces.Patch{GitEnabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	if !got.GitEnabled || got.Revision != 2 {
		t.Fatalf("%+v", got)
	}
	data, err := os.ReadFile(filepath.Join(root, "trebi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "git_enabled: true") || !strings.Contains(string(data), "name: alpha") {
		t.Fatalf("yaml: %s", data)
	}

	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	name := "blocked"
	if _, err := svc.Patch(ctx, ws.ID, workspaces.Patch{Name: &name}); err == nil {
		t.Fatal("want file-write error")
	}
	cur, err := svc.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Name != "alpha" {
		t.Fatalf("registry changed after file failure: %s", cur.Name)
	}
}

func TestRemoveNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTest(t)
	svc := workspaces.New(st)

	seq := mustSeq(t, st)
	err := svc.Remove(ctx, "ws_nope")
	if !errors.Is(err, workspaces.ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
	if got := mustSeq(t, st); got != seq {
		t.Fatalf("bus grew on not-found remove: %v", busTopics(t, st, seq))
	}
}

func openTest(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func busTopics(t *testing.T, st *store.Store, after int64) []string {
	t.Helper()
	evs, err := st.ListBus(context.Background(), after, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	var topics []string
	for _, ev := range evs {
		topics = append(topics, ev.Topic+":"+ev.ID)
	}
	return topics
}

func mustSeq(t *testing.T, st *store.Store) int64 {
	t.Helper()
	seq, err := st.NewestSeq(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return seq
}
