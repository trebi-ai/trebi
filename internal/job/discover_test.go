package job_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

const discoverJobMD = "---\nharness: claude\n---\n\nDo the task.\n"

func TestDiscoverSkipsOtherWorkspaceRoots(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeJob(t, filepath.Join(root, "a"))
	writeJob(t, filepath.Join(root, "sub", "b"))

	// Without other roots, both jobs appear.
	jobs, _, err := job.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs=%v want 2", jobNames(jobs))
	}

	// With sub registered as a workspace root, only "a" appears.
	jobs, _, err = job.Discover(root, filepath.Join(root, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Name != "a" {
		t.Fatalf("jobs=%v want [a]", jobNames(jobs))
	}
}

func TestDiscoverSkipsNestedTrebiYAML(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeJob(t, filepath.Join(root, "a"))
	writeJob(t, filepath.Join(root, "sub", "b"))
	if err := os.WriteFile(filepath.Join(root, "sub", "trebi.yaml"), []byte("id: ws_x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	jobs, _, err := job.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Name != "a" {
		t.Fatalf("jobs=%v want [a]", jobNames(jobs))
	}
}

func writeJob(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "JOB.md"), []byte(discoverJobMD), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jobNames(jobs []*job.Job) []string {
	var out []string
	for _, j := range jobs {
		out = append(out, j.Name)
	}
	return out
}
