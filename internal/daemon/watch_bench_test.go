package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

const benchJobMD = "---\nharness: claude\ntriggers:\n  - cron: \"0 9 * * *\"\n---\n\nDo the task.\n"

// BenchmarkRebuildAll runs one index rebuild over 50 jobs and 3000 non-job files.
// Baseline (Apple M1 Max, 2026-09-13): 8.0 ms/op, 2.46 MB/op, 23800 allocs/op.
func BenchmarkRebuildAll(b *testing.B) {
	dir := b.TempDir()
	home.SetBase(dir)
	b.Setenv("TREBI_HOME", dir)
	b.Cleanup(func() { home.SetBase("") })

	wsPath := filepath.Join(dir, "ws")
	for i := 0; i < 50; i++ {
		p := filepath.Join(wsPath, "jobs", fmt.Sprintf("job-%02d", i), "JOB.md")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(benchJobMD), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < 3000; i++ {
		p := filepath.Join(wsPath, "noise", fmt.Sprintf("d%02d", i%50), fmt.Sprintf("f%04d.txt", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			b.Fatal(err)
		}
	}

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = st.Close() })

	cp := New(st, Options{})
	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "bench")
	if err != nil {
		b.Fatal(err)
	}
	if err := cp.Workspaces.RebuildIndex(ctx, ws); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for b.Loop() {
		cp.rebuildAll(ctx)
	}
}
