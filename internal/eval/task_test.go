package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTaskAndPlaceholders(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "t.yaml")
	body := []byte(`id: demo
title: Demo
tier: onboarding
clients: [codex]
prompt: |
  hello
checks:
  - name: workspace exists
    cmd: trebi workspace list --json
    jq: '[.workspaces[].name] | index("ops") != null'
  - name: out
    file_exists: "{workspace}/hello/out.txt"
`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	task, err := LoadTask(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	if task.Trials != defaultTrials || task.ServerHarness != defaultHarness {
		t.Fatalf("defaults: %+v", task)
	}
	if len(task.Surfaces) != 1 || task.Surfaces[0] != surfaceCLI {
		t.Fatalf("surfaces default: %v", task.Surfaces)
	}
	p := Placeholders{Workspace: "/ws/ops"}
	got := p.Expand(task.Checks[1].FileExists)
	if got != "/ws/ops/hello/out.txt" {
		t.Fatalf("expand %s", got)
	}
}

func TestCheckValidate(t *testing.T) {
	t.Parallel()
	bad := Check{Name: "x", Cmd: "a", FileExists: "b"}
	if err := bad.validate(); err == nil {
		t.Fatal("expected error")
	}
	ok := Check{Name: "x", Cmd: "trebi doctor --json"}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	jq := Check{Name: "x", CommandsJQ: "any(true)"}
	if err := jq.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTaskRejectsUnknownTier(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "t.yaml")
	if err := os.WriteFile(path, []byte("id: x\ntier: nope\nprompt: p\nchecks:\n  - name: a\n    cmd: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTask(path, dir); err == nil {
		t.Fatal("expected error")
	}
}
