package job_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

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

func TestSpecRender(t *testing.T) {
	t.Parallel()
	md, err := job.Spec{Prompt: "Run the sync.", Cron: "0 3 * * *", Harness: "codex"}.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "harness: codex") || !strings.Contains(md, "cron: 0 3 * * *") {
		t.Fatalf("front matter: %s", md)
	}
	if !strings.HasSuffix(md, "Run the sync.\n") {
		t.Fatalf("body: %q", md)
	}
	parsed, err := job.ParseBytes("sync/JOB.md", []byte(md))
	if err != nil {
		t.Fatalf("compose must produce a parseable job: %v", err)
	}
	if parsed.Harness != "codex" || len(parsed.Triggers) != 1 {
		t.Fatalf("%+v", parsed)
	}

	if _, err := (job.Spec{}).Render(); err == nil {
		t.Fatal("an empty spec must fail")
	}
	if md, _ := (job.Spec{Content: "---\nharness: claude\n---\n\nhi\n", Prompt: "ignored"}).Render(); !strings.Contains(md, "hi") {
		t.Fatalf("content must win: %s", md)
	}
}
