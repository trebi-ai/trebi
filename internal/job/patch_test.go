package job_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

func TestMergePatchFrontmatter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "JOB.md")
	src := "---\nharness: fake\ntitle: Old\n# important comment\nsession:\n  idle_timeout: 5m\n---\n\nBody.\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	err := job.MergePatchFrontmatter(path, map[string]any{
		"harness":  "claude",
		"title":    "New",
		"session":  map[string]any{"if_running": "replace", "idle_timeout": "10m"},
		"disabled": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, "harness: claude") {
		t.Fatalf("harness: %s", s)
	}
	if !strings.Contains(s, "title: New") {
		t.Fatalf("title: %s", s)
	}
	if !strings.Contains(s, "if_running: replace") {
		t.Fatalf("if_running: %s", s)
	}
	if !strings.Contains(s, "idle_timeout: 10m") {
		t.Fatalf("idle_timeout: %s", s)
	}
	if !strings.Contains(s, "# important comment") {
		t.Fatalf("comment lost: %s", s)
	}
	if !strings.Contains(s, "disabled: true") {
		t.Fatalf("disabled: %s", s)
	}
	j, err := job.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if j.Harness != "claude" || !j.Disabled || j.Session.IfRunning != "replace" {
		t.Fatalf("%+v", j)
	}
}
