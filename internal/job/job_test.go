package job_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/secrets"
	"gopkg.in/yaml.v3"
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
	if j.Harness != "claude" || j.Envset.First() != "claude-zai" || len(j.Envset) != 1 {
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

// TestParseNotify pins the route forms: unset is nil (inherit), an empty
// list is set (no notification), and the old channel list is an error.
func TestParseNotify(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		fm      string
		want    []notify.Step
		set     bool
		contact []string
		err     string
	}{
		{name: "unset is nil"},
		{name: "empty is set", fm: "notify: []\n", want: []notify.Step{}, set: true},
		{
			name: "steps and contact", fm: "contact: Ops@Acme.com\nnotify:\n  - {via: whatsapp, to: '+1555'}\n  - via: email\n",
			want: []notify.Step{{Via: "whatsapp", To: "+1555"}, {Via: "email"}}, set: true, contact: []string{"ops@acme.com"},
		},
		{name: "contact list", fm: "contact: [a@acme.com, admins]\n", contact: []string{"a@acme.com", "admins"}},
		{name: "old list", fm: "notify: [email, whatsapp]\n", err: "the channel list is removed"},
		{name: "bad contact", fm: "contact: nobody\n", err: "want an address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j, err := job.ParseBytes("/ws/demo/JOB.md", []byte("---\n"+tc.fm+"---\nBody\n"))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if j.NotifySet != tc.set || !reflect.DeepEqual(j.Notify, tc.want) || !reflect.DeepEqual(j.Contact, tc.contact) {
				t.Fatalf("notify %#v set=%v contact %#v", j.Notify, j.NotifySet, j.Contact)
			}
		})
	}
}

func TestPatchRoute(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "demo", "JOB.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ntitle: Demo\n---\nBody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := map[string]any{
		"contact": []any{"ops@acme.com"},
		"notify":  []any{map[string]any{"via": "whatsapp", "to": "+1555"}, map[string]any{"via": "email"}},
	}
	if err := job.MergePatchFrontmatter(path, patch); err != nil {
		t.Fatal(err)
	}
	j, err := job.ParseFile(path)
	if err != nil || j.ParseError != "" {
		t.Fatal(err, j.ParseError)
	}
	if len(j.Notify) != 2 || j.Notify[0].To != "+1555" || !reflect.DeepEqual(j.Contact, []string{"ops@acme.com"}) {
		t.Fatalf("notify %#v contact %#v", j.Notify, j.Contact)
	}
	if err := job.MergePatchFrontmatter(path, map[string]any{"notify": nil}); err != nil {
		t.Fatal(err)
	}
	if j, _ = job.ParseFile(path); j.NotifySet || j.Title != "Demo" {
		t.Fatalf("notify null must remove the key: %#v", j)
	}
	if err := job.MergePatchFrontmatter(path, map[string]any{"notify": []any{"email"}}); err == nil {
		t.Fatal("old list form must fail")
	}
	body, err := job.Spec{Prompt: "Go.", Contact: []string{"a@acme.com"}, Notify: []notify.Step{}}.Render()
	if err != nil || !strings.Contains(body, "notify: []") || !strings.Contains(body, "a@acme.com") {
		t.Fatalf("render %v\n%s", err, body)
	}
	if body, _ = (job.Spec{Prompt: "Go."}).Render(); strings.Contains(body, "notify") || strings.Contains(body, "contact") {
		t.Fatalf("an unset route must not render:\n%s", body)
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

func TestParseNestedHarness(t *testing.T) {
	t.Parallel()
	raw := "---\nharness:\n  name: codex\n  model: gpt-5.4\n  effort: high\n---\nbody\n"
	j, err := job.ParseBytes("/ws/x/JOB.md", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if j.Harness != "codex" || j.Model != "gpt-5.4" || j.Effort != "high" {
		t.Fatalf("harness=%q model=%q effort=%q", j.Harness, j.Model, j.Effort)
	}
	if j.ResolvedHarness() != "codex" {
		t.Fatalf("resolved=%q", j.ResolvedHarness())
	}
}

func TestParseHarnessBadShape(t *testing.T) {
	t.Parallel()
	raw := "---\nharness: [claude]\n---\nbody\n"
	if _, err := job.ParseBytes("/ws/x/JOB.md", []byte(raw)); err == nil {
		t.Fatal("want an error for a sequence harness")
	}
}

func TestLaunchDefaultsHarnessPin(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	yaml := "tools:\n  claude:\n    model: pin-model\n    effort: high\n    env:\n      FROM_PIN: pin\n"
	if err := os.WriteFile(config.ConfigPath(), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	raw := "---\nharness: claude\nenv:\n  FROM_PIN: job\n  JOB_ONLY: \"1\"\n---\nbody\n"
	j, err := job.ParseBytes("/ws/x/JOB.md", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	d := j.LaunchDefaults()
	if d.Model != "pin-model" || d.Effort != "high" {
		t.Fatalf("pin defaults: model=%q effort=%q", d.Model, d.Effort)
	}
	if d.Env["FROM_PIN"] != "job" || d.Env["JOB_ONLY"] != "1" {
		t.Fatalf("job env must win: %v", d.Env)
	}

	nested := "---\nharness:\n  name: claude\n  model: job-model\n  effort: xhigh\n---\nbody\n"
	nj, err := job.ParseBytes("/ws/x/JOB.md", []byte(nested))
	if err != nil {
		t.Fatal(err)
	}
	nd := nj.LaunchDefaults()
	if nd.Model != "job-model" || nd.Effort != "xhigh" {
		t.Fatalf("job overrides: model=%q effort=%q", nd.Model, nd.Effort)
	}
	if nd.Env["FROM_PIN"] != "pin" {
		t.Fatalf("harness env: %v", nd.Env)
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

// TestTriggerYAML proves a trigger read alone (the assistant block of
// trebi.yaml) gets its family and canonical keys, and writes back the same.
func TestTriggerYAML(t *testing.T) {
	var list []job.Trigger
	src := "- event: chat\n  match: kind = 'message'\n  rate_limit: 5/m\n- cron: '0 9 * * *'\n- event: mail\n"
	if err := yaml.Unmarshal([]byte(src), &list); err != nil {
		t.Fatal(err)
	}
	job.NumberTriggers(list)
	if list[0].Family != "event" || list[0].RateLimit != "5/m" || list[0].Match == "" || list[1].Family != "cron" || list[2].Index != 1 {
		t.Fatalf("triggers = %+v", list)
	}
	out, err := yaml.Marshal(list)
	if err != nil || string(out) != "- event: chat\n  match: kind = 'message'\n  rate_limit: 5/m\n- cron: 0 9 * * *\n- event: mail\n" {
		t.Fatalf("marshal = %q %v", out, err)
	}
	if err := yaml.Unmarshal([]byte("- match: x\n"), &list); err == nil {
		t.Fatal("a trigger with no family must fail")
	}
	// A trigger from a JSON body has no raw form.
	out, err = yaml.Marshal([]job.Trigger{{Family: "event", Event: "chat", Match: "x"}})
	if err != nil || string(out) != "- event: chat\n  match: x\n" {
		t.Fatalf("json-made marshal = %q %v", out, err)
	}
}
