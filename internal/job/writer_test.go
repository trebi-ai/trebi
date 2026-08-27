package job_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

func TestRewriteSentinel(t *testing.T) {
	t.Parallel()
	in := []byte("finish with JOBI_TASK_COMPLETE please")
	out, changed := job.RewriteSentinel(in)
	if !changed || !strings.Contains(string(out), "TREBI_RUN_COMPLETE") {
		t.Fatalf("%s changed=%v", out, changed)
	}
	out2, changed2 := job.RewriteSentinel(out)
	if changed2 {
		t.Fatal("second pass should be no-op")
	}
	_ = out2
}

func TestSetDisabledPreservesComment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "JOB.md")
	content := "---\n# keep me\nharness: claude\nworking_dir: .\ndisabled: false\n---\nbody\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := job.SetDisabled(path, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "# keep me") {
		t.Fatal("comment lost")
	}
	if !strings.Contains(s, "disabled: true") {
		t.Fatalf("%s", s)
	}
}
