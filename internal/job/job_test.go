package job_test

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

const canonicalJob = `---
working_dir: .
harness: claude
envset: claude-zai
session:
  idle_timeout: 30m
  if_running: coalesce
triggers:
  - cron: "0 9 * * *"
    when:
      run: echo ok
      match: "true"
  - event: whatsapp
    match: "event.chat === 'x'"
    extract: "({from: event.from})"
memory: true
---
Do the thing. Emit TREBI_RUN_COMPLETE when done.
`

func TestParseCanonical(t *testing.T) {
	t.Parallel()
	j, err := job.ParseBytes("/ws/demo/JOB.md", []byte(canonicalJob))
	if err != nil {
		t.Fatal(err)
	}
	if j.Name != "demo" {
		t.Fatalf("name=%s", j.Name)
	}
	if j.Harness != "claude" || j.Envset != "claude-zai" {
		t.Fatalf("harness/envset: %s / %s", j.Harness, j.Envset)
	}
	if j.Session.IdleTimeout != "30m" {
		t.Fatalf("idle_timeout=%s", j.Session.IdleTimeout)
	}
	if len(j.Triggers) != 2 {
		t.Fatalf("triggers=%d", len(j.Triggers))
	}
	if j.Triggers[0].Family != "cron" || j.Triggers[0].Index != 0 {
		t.Fatalf("cron: %+v", j.Triggers[0])
	}
	if j.Triggers[1].Family != "event" || j.Triggers[1].Event != "whatsapp" || j.Triggers[1].Index != 0 {
		t.Fatalf("event: %+v", j.Triggers[1])
	}
	if !j.Memory {
		t.Fatal("memory")
	}
	if !strings.Contains(j.Prompt, "TREBI_RUN_COMPLETE") {
		t.Fatalf("prompt=%q", j.Prompt)
	}
	if j.ParseError != "" {
		t.Fatal(j.ParseError)
	}
}

func TestRejectLegacyCommand(t *testing.T) {
	t.Parallel()
	raw := "---\ncommand: claude\nworking_dir: .\n---\nbody\n"
	_, err := job.ParseBytes("/ws/x/JOB.md", []byte(raw))
	if err == nil || !strings.Contains(err.Error(), "legacy key") {
		t.Fatalf("want legacy key error, got %v", err)
	}
}

func TestRejectLegacyScheduleOn(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"schedule: '0 * * * *'", "on: []", "env: foo"} {
		raw := "---\n" + key + "\nworking_dir: .\nharness: claude\n---\nbody\n"
		_, err := job.ParseBytes("/ws/x/JOB.md", []byte(raw))
		if err == nil {
			t.Fatalf("expected reject for %s", key)
		}
	}
}

func TestRejectSessionTimeout(t *testing.T) {
	t.Parallel()
	raw := "---\nharness: claude\nworking_dir: .\nsession:\n  timeout: 30m\n---\nbody\n"
	_, err := job.ParseBytes("/ws/x/JOB.md", []byte(raw))
	if err == nil || !strings.Contains(err.Error(), "idle_timeout") {
		t.Fatalf("got %v", err)
	}
}

func TestRejectTopLevelWhen(t *testing.T) {
	t.Parallel()
	raw := "---\nharness: claude\nworking_dir: .\nwhen:\n  match: true\n---\nbody\n"
	_, err := job.ParseBytes("/ws/x/JOB.md", []byte(raw))
	if err == nil || !strings.Contains(err.Error(), "when") {
		t.Fatalf("got %v", err)
	}
}

func TestPerFamilyIndexSpaces(t *testing.T) {
	t.Parallel()
	raw := `---
working_dir: .
harness: claude
triggers:
  - event: a
    match: "true"
  - cron: "0 * * * *"
  - event: b
    match: "true"
  - every: 45m
    anchor: "2026-01-02T15:04:05Z"
---
hi
`
	j, err := job.ParseBytes("/ws/x/JOB.md", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	// event family: 0,1 ; schedule family (cron+every): 0,1
	var eventIdx, schedIdx []int
	for _, tr := range j.Triggers {
		switch tr.Family {
		case "event":
			eventIdx = append(eventIdx, tr.Index)
		case "cron", "every":
			schedIdx = append(schedIdx, tr.Index)
		}
	}
	if len(eventIdx) != 2 || eventIdx[0] != 0 || eventIdx[1] != 1 {
		t.Fatalf("event indices: %v", eventIdx)
	}
	if len(schedIdx) != 2 || schedIdx[0] != 0 || schedIdx[1] != 1 {
		t.Fatalf("schedule indices: %v", schedIdx)
	}
}

func TestEnvMapAcceptedEnvStringRejected(t *testing.T) {
	t.Parallel()
	ok := `---
working_dir: .
harness: claude
env:
  FOO: bar
---
hi
`
	if _, err := job.ParseBytes("/ws/x/JOB.md", []byte(ok)); err != nil {
		t.Fatal(err)
	}
	bad := `---
working_dir: .
harness: claude
env: claude-zai
---
hi
`
	if _, err := job.ParseBytes("/ws/x/JOB.md", []byte(bad)); err == nil {
		t.Fatal("expected reject string env")
	}
}

func TestContentHashStable(t *testing.T) {
	t.Parallel()
	h1 := job.ContentHash([]byte(canonicalJob))
	h2 := job.ContentHash([]byte(canonicalJob))
	if h1 != h2 || h1 == "" {
		t.Fatalf("%s vs %s", h1, h2)
	}
}
