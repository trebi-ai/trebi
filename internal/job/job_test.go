package job_test

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/secrets"
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

func TestLaunchEnvExpandsSecretNames(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	if err := secrets.Set("eval_api_key", "sk-secret"); err != nil {
		t.Fatal(err)
	}
	raw := "---\nharness: claude\nenv:\n  EVAL_API_KEY: eval_api_key\n  PLAIN: hello\n---\nbody\n"
	j, err := job.ParseBytes("/ws/x/JOB.md", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	env := j.LaunchEnv()
	if env["EVAL_API_KEY"] != "sk-secret" || env["PLAIN"] != "hello" {
		t.Fatalf("%v", env)
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

func TestParseMemoryFrontmatter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		path    string
		fm      string
		enabled bool
		maxLogs *int
		require *bool
		inject  *int
	}{
		{"true", "/ws/a/JOB.md", "memory: true", true, nil, nil, nil},
		{"false", "/ws/a/JOB.md", "memory: false", false, nil, nil, nil},
		{"absent job", "/ws/a/JOB.md", "", false, nil, nil, nil},
		{"legacy string", "/ws/a/JOB.md", "memory: ledger", true, nil, nil, nil},
		{"empty string", "/ws/a/JOB.md", `memory: ""`, false, nil, nil, nil},
		{
			"object", "/ws/a/JOB.md",
			"memory:\n  max_logs: 60\n  require_update: false\n  inject_logs: 30",
			true, ptrInt(60), ptrBool(false), ptrInt(30),
		},
		{"object empty", "/ws/a/JOB.md", "memory: {}", true, nil, nil, nil},
		{"agent default", "/ws/agents/bot/AGENT.md", "", true, nil, nil, nil},
		{"agent false", "/ws/agents/bot/AGENT.md", "memory: false", false, nil, nil, nil},
		{"agent object", "/ws/agents/bot/AGENT.md", "memory:\n  max_logs: 5", true, ptrInt(5), nil, nil},
	}
	for _, tc := range cases {
		doc := "---\n" + tc.fm + "\n---\nbody\n"
		j, err := job.ParseBytes(tc.path, []byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if j.Memory != tc.enabled || j.MemoryOpts.Enabled != tc.enabled {
			t.Errorf("%s: enabled = %v / %+v", tc.name, j.Memory, j.MemoryOpts)
		}
		if !ptrEqInt(j.MemoryOpts.MaxLogs, tc.maxLogs) {
			t.Errorf("%s: max_logs = %v, want %v", tc.name, j.MemoryOpts.MaxLogs, tc.maxLogs)
		}
		if !ptrEqBool(j.MemoryOpts.RequireUpdate, tc.require) {
			t.Errorf("%s: require_update = %v, want %v", tc.name, j.MemoryOpts.RequireUpdate, tc.require)
		}
		if !ptrEqInt(j.MemoryOpts.InjectLogs, tc.inject) {
			t.Errorf("%s: inject_logs = %v, want %v", tc.name, j.MemoryOpts.InjectLogs, tc.inject)
		}
	}
}

func TestParseMemoryErrors(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"unknown key":       "memory:\n  cap: 5",
		"max_logs string":   "memory:\n  max_logs: many",
		"inject float":      "memory:\n  inject_logs: 1.5",
		"require as int":    "memory:\n  require_update: 1",
		"memory as list":    "memory: [1, 2]",
		"memory as int":     "memory: 3",
		"require as string": "memory:\n  require_update: yes!",
	}
	for name, fm := range cases {
		doc := "---\n" + fm + "\n---\nbody\n"
		j, err := job.ParseBytes("/ws/a/JOB.md", []byte(doc))
		if err == nil {
			t.Errorf("%s: expected error", name)
			continue
		}
		if j == nil || !strings.Contains(j.ParseError, "memory") {
			t.Errorf("%s: ParseError = %q", name, parseErrOf(j))
		}
	}
}

func TestMemoryConfigResolve(t *testing.T) {
	t.Parallel()
	def := config.MemoryDefaults{MaxLogs: 100, RequireUpdate: true, InjectLogs: 20}

	// Nil pointers take config defaults.
	opt := job.MemoryConfig{}.Resolve(def)
	if opt.MaxLogs != 100 || !opt.RequireUpdate || opt.InjectLogs != 20 {
		t.Fatalf("defaults not filled: %+v", opt)
	}

	// Explicit values win, including zero (no cap) and false.
	zero, noUpdate, none := 0, false, 0
	opt = job.MemoryConfig{MaxLogs: &zero, RequireUpdate: &noUpdate, InjectLogs: &none}.Resolve(def)
	if opt.MaxLogs != 0 || opt.RequireUpdate || opt.InjectLogs != 0 {
		t.Fatalf("explicit zeros not kept: %+v", opt)
	}
}

func ptrInt(n int) *int    { return &n }
func ptrBool(b bool) *bool { return &b }

func ptrEqInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func ptrEqBool(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func parseErrOf(j *job.Job) string {
	if j == nil {
		return "<nil job>"
	}
	return j.ParseError
}
