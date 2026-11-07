package job_test

import (
	"os"
	"path/filepath"
	"reflect"
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
		"session":  map[string]any{"if_running": "replace", "idle_timeout": "10m", "continue": "7d"},
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
	if j.Harness != "claude" || !j.Disabled || j.Session.IfRunning != "replace" || j.Session.Continue != "7d" {
		t.Fatalf("%+v", j)
	}
}

func TestMergePatchFrontmatterConnectors(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "AGENT.md")
	src := "---\nharness: fake\nkit:\n  connectors: [kitone]\nconnectors:\n  - old\n  - older\ntitle: T\n---\n\nBody.\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := job.MergePatchFrontmatter(path, map[string]any{"connectors": []any{"github", "slack"}}); err != nil {
		t.Fatal(err)
	}
	j, err := job.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(j.Connectors, []string{"github", "slack"}) {
		t.Fatalf("connectors = %v", j.Connectors)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "  connectors: [kitone]\n") || strings.Contains(string(data), "old") {
		t.Fatalf("patch touched the kit list or kept old items: %s", data)
	}
	if j.Title != "T" {
		t.Fatalf("title = %q", j.Title)
	}

	// An empty list clears the job list, so the kit list applies.
	if err := job.MergePatchFrontmatter(path, map[string]any{"connectors": []any{}}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "\nconnectors: []\n") {
		t.Fatalf("clear: %s", data)
	}
}

func TestMergePatchFrontmatterEmailAccept(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, src, want string }{
		{"block list", "---\nemail:\n  accept:\n    - admins\n    - \"@acme.com\"\n  style:\n    accent: \"#123456\"\ntitle: T\n---\n",
			"email:\n  accept: [\"admins\", \"a@b.com\"]\n  style:\n    accent: \"#123456\"\ntitle: T\n"},
		{"flow list", "---\nemail:\n  accept: [admins]\n---\n", "email:\n  accept: [\"admins\", \"a@b.com\"]\n"},
		{"no email", "---\ntitle: T\n---\n", "title: T\nemail:\n  accept: [\"admins\", \"a@b.com\"]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "AGENT.md")
			if err := os.WriteFile(path, []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := job.MergePatchFrontmatter(path, map[string]any{"email_accept": []string{"admins", "a@b.com"}}); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(path)
			if got := strings.SplitN(string(data), "---\n", 3)[1]; got != tc.want {
				t.Fatalf("got\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "AGENT.md")
	_ = os.WriteFile(path, []byte("---\nemail: {accept: [admins]}\n---\n"), 0o644)
	if err := job.MergePatchFrontmatter(path, map[string]any{"email_accept": []any{"x@y.com"}}); err == nil {
		t.Fatal("a one-line email map changed")
	}
	if err := job.MergePatchFrontmatter(path, map[string]any{"email_accept": []any{"not an address"}}); err == nil {
		t.Fatal("a bad entry passed")
	}
}

func TestMergePatchFolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "JOB.md")
	if err := os.WriteFile(path, []byte("---\nharness: claude\n---\n\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	folder := func() string {
		t.Helper()
		j, err := job.ParseFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return j.Folder
	}
	steps := []struct {
		value any
		want  string
	}{
		{"SEO", "SEO"},
		{"Growth: Q4", "Growth: Q4"},
		{"", ""},
	}
	for _, s := range steps {
		if err := job.MergePatchFrontmatter(path, map[string]any{"folder": s.value}); err != nil {
			t.Fatalf("patch %v: %v", s.value, err)
		}
		if got := folder(); got != s.want {
			t.Fatalf("after %v: folder=%q want %q", s.value, got, s.want)
		}
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "folder") {
		t.Fatalf("empty folder left the key:\n%s", data)
	}
	before := data
	if err := job.MergePatchFrontmatter(path, map[string]any{"folder": "a/b", "title": "X"}); err == nil {
		t.Fatal("folder a/b accepted")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("a bad folder changed the file:\n%s", after)
	}
}

func TestMergePatchFolderOnAgent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agents", "cmo", "AGENT.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nharness: claude\n---\n\nLead.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := job.MergePatchFrontmatter(path, map[string]any{"folder": "SEO"})
	if err == nil || !strings.Contains(err.Error(), "applies only to a job") {
		t.Fatalf("err=%v", err)
	}
}

func TestMergePatchEnvFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "JOB.md")
	if err := os.WriteFile(path, []byte("---\nharness: fake\n---\n\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	read := func() *job.Job {
		t.Helper()
		j, err := job.ParseFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	for _, tc := range []struct {
		v    any
		want *bool
	}{{false, new(false)}, {"true", new(true)}, {nil, nil}} {
		if err := job.MergePatchFrontmatter(path, map[string]any{"env_file": tc.v}); err != nil {
			t.Fatal(err)
		}
		if got := read().EnvFile; !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("env_file %v: got %v, want %v", tc.v, got, tc.want)
		}
	}
	if err := job.MergePatchFrontmatter(path, map[string]any{"env_file": "maybe"}); err == nil {
		t.Fatal("bad env_file value accepted")
	}
}
