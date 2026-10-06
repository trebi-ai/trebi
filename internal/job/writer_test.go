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

func TestMergePatchHarnessModel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "JOB.md")
	content := "---\n# keep me\nharness: claude\nworking_dir: .\n---\nbody\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := job.MergePatchFrontmatter(path, map[string]any{"model": "opus"}); err != nil {
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
	if !strings.Contains(s, "harness:\n  name: claude\n  model: opus\n") {
		t.Fatalf("harness block: %s", s)
	}
	if strings.Contains(s, "effort:") {
		t.Fatalf("effort must stay unset: %s", s)
	}
	parsed, err := job.ParseBytes(path, data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Harness != "claude" || parsed.Model != "opus" || parsed.Effort != "" {
		t.Fatalf("parsed: %+v", parsed)
	}

	// Clearing model collapses the node back to the scalar form.
	if err := job.MergePatchFrontmatter(path, map[string]any{"model": ""}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	s = string(data)
	if !strings.Contains(s, "harness: claude\n") || strings.Contains(s, "model:") {
		t.Fatalf("collapse: %s", s)
	}
}

func TestMergePatchHarnessEffortKeepsModel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "JOB.md")
	content := "---\nharness:\n  name: codex\n  model: gpt-5.4\n---\nbody\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := job.MergePatchFrontmatter(path, map[string]any{"effort": "high"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, "  model: gpt-5.4\n") || !strings.Contains(s, "  effort: high\n") {
		t.Fatalf("effort merge: %s", s)
	}
	if strings.Count(s, "harness:") != 1 {
		t.Fatalf("harness duplicated: %s", s)
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

func TestSpecRenderOmitsUnsetFields(t *testing.T) {
	t.Parallel()
	md, err := job.Spec{ID: "agt_0123456789", Prompt: "Go.", Description: "Watch usage."}.Render()
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nid: agt_0123456789\nharness: claude\ndescription: Watch usage.\n---\n\nGo.\n"
	if md != want {
		t.Fatalf("got:\n%s\nwant:\n%s", md, want)
	}
}

func TestSpecRenderNestedHarness(t *testing.T) {
	t.Parallel()
	md, err := job.Spec{Prompt: "Run.", Harness: "claude", Model: "opus", Effort: "xhigh"}.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "harness:\n    name: claude\n    model: opus\n    effort: xhigh\n") {
		t.Fatalf("nested harness: %s", md)
	}
	parsed, err := job.ParseBytes("sync/JOB.md", []byte(md))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Harness != "claude" || parsed.Model != "opus" || parsed.Effort != "xhigh" {
		t.Fatalf("%+v", parsed)
	}
}
