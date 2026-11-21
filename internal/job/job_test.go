package job_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/secrets"
)

const canonicalJob = `---
working_dir: .
harness: claude-zai
session:
  idle_timeout: 30m
  if_running: queue
triggers:
  - cron: "0 9 * * *"
    when:
      run: echo ok
      match: "true"
  - every: 45m
memory: true
---
Do the thing. Emit TREBI_RUN_COMPLETE when done.
`

func TestParseIgnoresOldKeys(t *testing.T) {
	t.Parallel()
	for path, raw := range map[string]string{
		"/ws/x/JOB.md":          "---\nharness: claude\nschedule: '0 * * * *'\ncommand: claude\nallowlist: [Bash]\n---\nbody\n",
		"/ws/agents/a/AGENT.md": "---\nharness: claude\nkit:\n  mcp: [slack]\n---\nbody\n",
	} {
		j, err := job.ParseBytes(path, []byte(raw))
		if err != nil || j.ParseError != "" {
			t.Fatalf("%s: err=%v parse_error=%q", path, err, j.ParseError)
		}
	}
}

func TestParseCanonical(t *testing.T) {
	t.Parallel()
	j, err := job.ParseBytes("/ws/demo/JOB.md", []byte(canonicalJob))
	if err != nil {
		t.Fatal(err)
	}
	if j.Name != "demo" {
		t.Fatalf("name=%s", j.Name)
	}
	if j.Harness != "claude-zai" {
		t.Fatalf("harness: %s", j.Harness)
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
	if j.Triggers[1].Family != "every" || j.Triggers[1].Index != 1 {
		t.Fatalf("every: %+v", j.Triggers[1])
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
	if j.ResolvedHarness(nil) != "codex" {
		t.Fatalf("resolved=%q", j.ResolvedHarness(nil))
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

func TestScheduleIndexSpace(t *testing.T) {
	t.Parallel()
	raw := `---
working_dir: .
harness: claude
triggers:
  - cron: "0 * * * *"
  - every: 45m
  - cron: "5 * * * *"
---
hi
`
	j, err := job.ParseBytes("/ws/x/JOB.md", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var idx []int
	for _, tr := range j.Triggers {
		idx = append(idx, tr.Index)
	}
	if !slices.Equal(idx, []int{0, 1, 2}) {
		t.Fatalf("schedule indices: %v", idx)
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
		bytes   *int
	}{
		{"true", "/ws/a/JOB.md", "memory: true", true, nil, nil, nil, nil},
		{"false", "/ws/a/JOB.md", "memory: false", false, nil, nil, nil, nil},
		{"absent job", "/ws/a/JOB.md", "", false, nil, nil, nil, nil},
		{"legacy string", "/ws/a/JOB.md", "memory: ledger", true, nil, nil, nil, nil},
		{"empty string", "/ws/a/JOB.md", `memory: ""`, false, nil, nil, nil, nil},
		{
			"object", "/ws/a/JOB.md",
			"memory:\n  max_logs: 60\n  require_update: false\n  inject_logs: 30\n  max_bytes: 4096",
			true, ptrInt(60), ptrBool(false), ptrInt(30), ptrInt(4096),
		},
		{"object empty", "/ws/a/JOB.md", "memory: {}", true, nil, nil, nil, nil},
		{"max_bytes off", "/ws/a/JOB.md", "memory:\n  max_bytes: 0", true, nil, nil, nil, ptrInt(0)},
		{"agent default", "/ws/agents/bot/AGENT.md", "", true, nil, nil, nil, nil},
		{"agent false", "/ws/agents/bot/AGENT.md", "memory: false", false, nil, nil, nil, nil},
		{"agent object", "/ws/agents/bot/AGENT.md", "memory:\n  max_logs: 5", true, ptrInt(5), nil, nil, nil},
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
		if !ptrEqInt(j.MemoryOpts.MaxBytes, tc.bytes) {
			t.Errorf("%s: max_bytes = %v, want %v", tc.name, j.MemoryOpts.MaxBytes, tc.bytes)
		}
	}
}

func TestParseMemoryErrors(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"unknown key":        "memory:\n  cap: 5",
		"max_logs string":    "memory:\n  max_logs: many",
		"inject float":       "memory:\n  inject_logs: 1.5",
		"max_bytes negative": "memory:\n  max_bytes: -1",
		"require as int":     "memory:\n  require_update: 1",
		"memory as list":     "memory: [1, 2]",
		"memory as int":      "memory: 3",
		"require as string":  "memory:\n  require_update: yes!",
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

func TestParseState(t *testing.T) {
	t.Parallel()
	doc := "---\nstate:\n  reported:\n    description: Items already sent.\n    ttl: 2y\n    schema:\n      type: object\n      required: [title]\n  sources: {}\n---\nbody\n"
	j, err := job.ParseBytes("/ws/a/JOB.md", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(j.State) != 2 || j.State["reported"].TTL != "2y" || j.State["reported"].Description != "Items already sent." {
		t.Fatalf("State = %+v", j.State)
	}
	none, err := job.ParseBytes("/ws/a/JOB.md", []byte("---\ntitle: x\n---\nbody\n"))
	if err != nil || none.State != nil {
		t.Fatalf("no state: %+v %v", none.State, err)
	}
	bad, err := job.ParseBytes("/ws/a/JOB.md", []byte("---\nstate:\n  Reported: {}\n---\nbody\n"))
	if err == nil || !strings.Contains(bad.ParseError, `state: collection "Reported"`) {
		t.Fatalf("bad name: %v %q", err, parseErrOf(bad))
	}
}

func TestMemoryConfigResolve(t *testing.T) {
	t.Parallel()
	def := config.MemoryDefaults{MaxLogs: 100, RequireUpdate: true, InjectLogs: 20, MaxBytes: 16384}

	// Nil pointers take config defaults.
	opt := job.MemoryConfig{}.Resolve(def)
	if opt.MaxLogs != 100 || !opt.RequireUpdate || opt.InjectLogs != 20 || opt.MaxBytes != 16384 {
		t.Fatalf("defaults not filled: %+v", opt)
	}
	if got := (job.MemoryConfig{}).Resolve(config.DefaultMemory()); got.MaxBytes != 16384 {
		t.Fatalf("built-in max_bytes = %d", got.MaxBytes)
	}

	// Explicit values win, including zero (no cap) and false.
	zero, noUpdate, none, off := 0, false, 0, 0
	opt = job.MemoryConfig{MaxLogs: &zero, RequireUpdate: &noUpdate, InjectLogs: &none, MaxBytes: &off}.Resolve(def)
	if opt.MaxLogs != 0 || opt.RequireUpdate || opt.InjectLogs != 0 || opt.MaxBytes != 0 {
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

// TestParseRefusesEventTrigger proves a job file refuses an event entry and
// names the routes file that takes it.
func TestParseRefusesEventTrigger(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, path, fm string
		index          int
		conn           string
	}{
		{"job", "/ws/x/JOB.md", "triggers:\n  - event: gmail-2\n", 0, "gmail-2"},
		{"agent", "/ws/agents/a/AGENT.md", "triggers:\n  - cron: \"0 9 * * *\"\n  - event: chat\n    rooms: [r1]\n", 1, "chat"},
	}
	for _, tc := range cases {
		j, err := job.ParseBytes(tc.path, []byte("---\n"+tc.fm+"---\nhi\n"))
		var ee *job.EventTriggerError
		if !errors.As(err, &ee) || ee.Index != tc.index || ee.Connection != tc.conn {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
		if !strings.Contains(err.Error(), "routes/"+tc.conn+".yaml") || !strings.Contains(parseErrOf(j), "routes/"+tc.conn+".yaml") {
			t.Fatalf("%s: message %q", tc.name, err)
		}
	}
	if _, err := job.ParseBytes("/ws/x/JOB.md", []byte("---\ntriggers:\n  - cron: \"0 9 * * *\"\n  - every: 1h\n---\nhi\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := job.ParseBytes("/ws/agents/nobody/AGENT.md", []byte("---\n---\nhi\n")); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("agent nobody: %v", err)
	}
	j, err := job.ParseBytes("/ws/agents/ops/AGENT.md", []byte("---\ndescription: Ops.\n---\nhi\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := job.ValidateAgent(j, "nobody"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("ValidateAgent nobody: %v", err)
	}
}

// TestScheduleKeys proves each event-only key fails on a schedule.
func TestScheduleKeys(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]string{
		"contains: x":     `"contains" applies only to an event trigger`,
		"from: bots":      `"from" applies only to an event trigger`,
		"replies: off":    `"replies" applies only to an event trigger`,
		"rooms: [r1]":     `"rooms" applies only to an event trigger`,
		"allowlist: [a]":  `"allowlist" applies only to an event trigger`,
		"batch: 10m":      `"batch" applies only to an event trigger`,
		"chat: listen":    `"chat" applies only to an event trigger`,
		"limit: 1/1h":     `"limit" applies only to an event trigger`,
		"debounce: 30s":   `use "limit: N/<duration>"`,
		"enabled: false":  `use "disabled: true"`,
		"anchor: x":       "use cron",
		"rate_limit: 1/1": `use "limit: N/<duration>"`,
	} {
		j, err := job.ParseBytes("/ws/x/JOB.md", []byte("---\ntriggers:\n  - cron: \"0 9 * * *\"\n    "+key+"\n---\nhi\n"))
		if err == nil || !strings.Contains(parseErrOf(j), want) {
			t.Errorf("%q: err=%v parse_error=%q, want %q", key, err, parseErrOf(j), want)
		}
	}
}

// TestNormalizeEventTrigger proves the event filter keys of a route.
func TestNormalizeEventTrigger(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  job.TriggerYAML
		want string
		chk  func(t *testing.T, tr job.Trigger)
	}{
		{name: "scalar contains", raw: job.TriggerYAML{Contains: []string{" a ", "b", "a", ""}, From: "bots", Replies: "continue"}, chk: func(t *testing.T, tr job.Trigger) {
			if !slices.Equal(tr.Contains, []string{"a", "b"}) || tr.From != job.FromBots || !strings.HasPrefix(tr.Replies, "continue") || tr.Family != "event" {
				t.Fatalf("%+v", tr)
			}
		}},
		{name: "rooms", raw: job.TriggerYAML{Rooms: []string{"r1", "", "r1", "r2"}, Allowlist: []string{"alice", "alice"}}, chk: func(t *testing.T, tr job.Trigger) {
			if !reflect.DeepEqual(tr.Rooms, []string{"r1", "r2"}) || !reflect.DeepEqual(tr.Allowlist, []string{"alice"}) {
				t.Fatalf("%+v", tr)
			}
		}},
		{name: "default limit", raw: job.TriggerYAML{}, chk: func(t *testing.T, tr job.Trigger) {
			if tr.Limit != "" || tr.EffectiveLimit != "15/10m" || tr.RateLimit() != nil && tr.RateLimit().Per != 10*time.Minute {
				t.Fatalf("%+v", tr)
			}
		}},
		{name: "limit off", raw: job.TriggerYAML{Limit: "off"}, chk: func(t *testing.T, tr job.Trigger) {
			if tr.EffectiveLimit != "off" {
				t.Fatalf("%+v", tr)
			}
		}},
		{name: "bad limit", raw: job.TriggerYAML{Limit: "fast"}, want: `limit "fast"`},
		{name: "bad from", raw: job.TriggerYAML{From: "robots"}, want: `from "robots"`},
		{name: "bad replies", raw: job.TriggerYAML{Replies: "always"}, want: `replies "always"`},
		{name: "batch", raw: job.TriggerYAML{Batch: "10m"}, chk: func(t *testing.T, tr job.Trigger) {
			if tr.BatchMS != 600000 || !tr.Batched() {
				t.Fatalf("%+v", tr)
			}
		}},
		{name: "batch off", raw: job.TriggerYAML{Batch: "off"}, chk: func(t *testing.T, tr job.Trigger) {
			if tr.BatchMS != 0 {
				t.Fatalf("%+v", tr)
			}
		}},
		{name: "batch short", raw: job.TriggerYAML{Batch: "30s"}, want: `batch "30s": must be at least 1m`},
		{name: "batch long", raw: job.TriggerYAML{Batch: "25h"}, want: `batch "25h": must be at most 24h`},
		{name: "batch word", raw: job.TriggerYAML{Batch: "abc"}, want: `batch "abc": not a duration`},
		{name: "batch continue", raw: job.TriggerYAML{Batch: "10m", Replies: "continue"}, want: `"batch" cannot be used with "replies: continue"`},
		{name: "batch chat", raw: job.TriggerYAML{Batch: "5m", Rooms: []string{"r"}, Chat: "reply"}, want: `"batch" cannot be used with "chat: reply"`},
		{name: "chat reply", raw: job.TriggerYAML{Rooms: []string{"r"}, Chat: "reply"}, chk: func(t *testing.T, tr job.Trigger) {
			if !tr.RepliesInChat() {
				t.Fatalf("%+v", tr)
			}
		}},
		{name: "chat listen", raw: job.TriggerYAML{Chat: "listen"}},
		{name: "chat no rooms", raw: job.TriggerYAML{Chat: "reply"}, want: `"chat: reply" needs "rooms"`},
		{name: "chat continue", raw: job.TriggerYAML{Rooms: []string{"r"}, Chat: "reply", Replies: "continue"}, want: `cannot be used with "replies: continue"`},
		{name: "chat bad", raw: job.TriggerYAML{Rooms: []string{"r"}, Chat: "talk"}, want: `chat "talk": use listen or reply`},
		{name: "removed key", raw: job.TriggerYAML{Debounce: "30s"}, want: `use "limit: N/<duration>"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr, err := job.NormalizeEventTrigger(tc.raw)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.chk != nil {
				tc.chk(t, tr)
			}
		})
	}
}

// TestBatchRouteIssues proves the prompt warnings of a batch route.
func TestBatchRouteIssues(t *testing.T) {
	t.Parallel()
	parse := func(prompt string) *job.Job {
		j, err := job.ParseBytes("/ws/x/JOB.md", []byte("---\nharness: claude\n---\n"+prompt+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	batch := func(extract string) job.Trigger {
		tr, err := job.NormalizeEventTrigger(job.TriggerYAML{Batch: "10m", Extract: extract})
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
	for prompt, want := range map[string]int{
		"Reply to {{text}} from {{sender}}":                                     2,
		"Summarize {{event_count}} messages:\n{{events}}\nAll: {{events_file}}": 0,
	} {
		is := parse(prompt).RouteIssues("routes[0]", batch(""))
		if len(is) != want || (want > 0 && (is[0].Code != job.CodeMayBeEmpty || is[0].Message != "{{text}} is empty in a batch run; use {{events}} or {{events_file}}")) {
			t.Errorf("%q: issues = %+v", prompt, is)
		}
	}
	if is := parse("Thank {{who}}").RouteIssues("routes[0]", batch("({who: event.sender})")); len(is) != 1 {
		t.Fatalf("extract var issues = %+v", is)
	}
	single, _ := job.NormalizeEventTrigger(job.TriggerYAML{})
	if is := parse("Reply to {{text}}").RouteIssues("routes[0]", single); len(is) != 0 {
		t.Fatalf("no batch, issues = %+v", is)
	}
}

func TestIfRunningValues(t *testing.T) {
	t.Parallel()
	for v, ok := range map[string]bool{"queue": true, "skip": true, "replace": true, "allow": false, "coalesce": false} {
		_, err := job.ParseBytes("/ws/x/JOB.md", []byte("---\nsession:\n  if_running: "+v+"\n---\nhi\n"))
		if (err == nil) != ok {
			t.Errorf("%s: err=%v", v, err)
		}
	}
}

func TestSessionContinue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		{value: "", want: 0},
		{value: "12h", want: 12 * time.Hour},
		{value: "7d", want: 7 * 24 * time.Hour},
		{value: "soon", wantErr: true},
		{value: "-1h", wantErr: true},
	}
	for _, tc := range cases {
		raw := "---\nharness: claude\nsession:\n  continue: \"" + tc.value + "\"\n---\nbody\n"
		j, err := job.ParseBytes("/ws/x/JOB.md", []byte(raw))
		if tc.wantErr {
			if err == nil && j.ParseError == "" {
				t.Fatalf("continue %q: want a parse error", tc.value)
			}
			continue
		}
		if err != nil {
			t.Fatalf("continue %q: %v", tc.value, err)
		}
		if got := j.LaunchDefaults().ContinueMS; got != tc.want.Milliseconds() {
			t.Fatalf("continue %q: got %dms want %s", tc.value, got, tc.want)
		}
	}
}

func TestParseArtifacts(t *testing.T) {
	t.Parallel()
	ok := "---\nharness: claude\nartifacts:\n  - path: out/Weekly Report.html\n  - {path: data.csv, name: sales, title: Sales}\n---\nbody\n"
	j, err := job.ParseBytes("/ws/x/JOB.md", []byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	want := []job.ArtifactYAML{
		{Path: "out/Weekly Report.html", Name: "weekly-report", Title: "weekly-report"},
		{Path: "data.csv", Name: "sales", Title: "Sales"},
	}
	if len(j.Artifacts) != 2 || j.Artifacts[0] != want[0] || j.Artifacts[1] != want[1] {
		t.Fatalf("artifacts %+v", j.Artifacts)
	}
	bad := []struct{ entries, want string }{
		{"  - {path: ../x.html}", `artifacts: entry 1: path "../x.html" must be inside the working dir`},
		{"  - {path: /etc/x.html}", `artifacts: entry 1: path "/etc/x.html" must be inside the working dir`},
		{"  - {name: x}", "artifacts: entry 1: path is required"},
		{"  - {path: a.html, name: Bad_Name}", `artifacts: entry 1: name "Bad_Name" must match ^[a-z0-9][a-z0-9-]{0,62}$`},
		{"  - {path: a.html}\n  - {path: b/a.md}", `artifacts: entry 2: duplicate name "a"`},
	}
	for _, tc := range bad {
		raw := "---\nharness: claude\nartifacts:\n" + tc.entries + "\n---\nbody\n"
		_, err := job.ParseBytes("/ws/x/JOB.md", []byte(raw))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want %q", tc.entries, err, tc.want)
		}
	}
}

func TestParseFolder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, path, fm, want, err string
	}{
		{name: "none", path: "/ws/agents/cmo/a/JOB.md", fm: "", want: ""},
		{name: "plain", path: "/ws/agents/cmo/a/JOB.md", fm: "folder: SEO\n", want: "SEO"},
		{name: "trim", path: "/ws/agents/cmo/a/JOB.md", fm: "folder: \"  Competitor watch  \"\n", want: "Competitor watch"},
		{name: "empty", path: "/ws/agents/cmo/a/JOB.md", fm: "folder: \"\"\n", want: ""},
		{name: "slash", path: "/ws/agents/cmo/a/JOB.md", fm: "folder: a/b\n", err: "nested folders are not supported"},
		{name: "backslash", path: "/ws/agents/cmo/a/JOB.md", fm: "folder: 'a\\b'\n", err: "nested folders are not supported"},
		{name: "long", path: "/ws/agents/cmo/a/JOB.md", fm: "folder: " + strings.Repeat("x", 65) + "\n", err: "at most 64 characters"},
		{name: "control", path: "/ws/agents/cmo/a/JOB.md", fm: "folder: \"a\\tb\"\n", err: "remove control characters"},
		{name: "agent", path: "/ws/agents/cmo/AGENT.md", fm: "folder: SEO\n", err: "applies only to a job"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j, err := job.ParseBytes(tc.path, []byte("---\nharness: claude\n"+tc.fm+"---\nBody\n"))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err=%v want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if j.Folder != tc.want {
				t.Fatalf("folder=%q want %q", j.Folder, tc.want)
			}
		})
	}
}
func TestChoiceOrder(t *testing.T) {
	writeConfig(t, "defaults:\n  command: codex\ntools:\n  codex:\n    model: gpt-5\n    effort: low\n  claude:\n    model: sonnet\n")
	agent := &job.KitYAML{Harness: "claude", Model: "opus", Effort: "high"}
	for _, tc := range []struct {
		name string
		j    job.Job
		kit  *job.KitYAML
		want job.HarnessChoice
	}{
		{"default", job.Job{}, nil, job.HarnessChoice{
			Harness: "codex", HarnessSource: "default", Model: "gpt-5", ModelSource: "default", Effort: "low", EffortSource: "default"}},
		{"agent", job.Job{}, agent, job.HarnessChoice{
			Harness: "claude", HarnessSource: "agent", Model: "opus", ModelSource: "agent", Effort: "high", EffortSource: "agent"}},
		{"job model wins", job.Job{Model: "haiku"}, agent, job.HarnessChoice{
			Harness: "claude", HarnessSource: "agent", Model: "haiku", ModelSource: "job", Effort: "high", EffortSource: "agent"}},
		// The agent model belongs to the agent harness, so another job harness skips it.
		{"other harness", job.Job{Harness: "codex"}, agent, job.HarnessChoice{
			Harness: "codex", HarnessSource: "job", Model: "gpt-5", ModelSource: "default", Effort: "low", EffortSource: "default"}},
		{"same harness on job", job.Job{Harness: "claude"}, agent, job.HarnessChoice{
			Harness: "claude", HarnessSource: "job", Model: "opus", ModelSource: "agent", Effort: "high", EffortSource: "agent"}},
		// An agent with no harness has the default harness, so its model applies to a default job.
		{"agent model only", job.Job{}, &job.KitYAML{Model: "gpt-5-pro"}, job.HarnessChoice{
			Harness: "codex", HarnessSource: "default", Model: "gpt-5-pro", ModelSource: "agent", Effort: "low", EffortSource: "default"}},
	} {
		if got := tc.j.Choice(tc.kit); got != tc.want {
			t.Errorf("%s: got %+v want %+v", tc.name, got, tc.want)
		}
	}
	if d := (&job.Job{Kind: "job"}).LaunchDefaultsWithKit(agent); d.Harness != "claude" || d.Model != "opus" || d.Effort != "high" {
		t.Fatalf("launch defaults with kit: %+v", d)
	}
}

func TestChoiceNoConfig(t *testing.T) {
	_ = os.Remove(home.ConfigPath())
	if got := (&job.Job{}).ResolvedHarness(nil); got != "claude" {
		t.Fatalf("no config: %q want claude", got)
	}
}

func TestOwnerKitCarriesHarness(t *testing.T) {
	t.Parallel()
	a, err := job.ParseBytes("agents/cmo/AGENT.md", []byte("---\ndescription: Growth.\nharness:\n  name: codex\n  model: gpt-5\n---\n\nA.\n"))
	if err != nil {
		t.Fatal(err)
	}
	kit := a.OwnerKit()
	if kit == nil || kit.Harness != "codex" || kit.Model != "gpt-5" || kit.Effort != "" {
		t.Fatalf("kit=%+v", kit)
	}
}

func TestMergePatchHarnessNullRemovesNode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "JOB.md")
	if err := os.WriteFile(path, []byte("---\nharness:\n  name: codex\n  model: gpt-5\ntitle: T\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := job.MergePatchFrontmatter(path, map[string]any{"harness": nil, "model": nil, "effort": nil}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if s := string(data); strings.Contains(s, "harness") || strings.Contains(s, "model") || !strings.Contains(s, "title: T") {
		t.Fatalf("harness node left: %s", s)
	}
	parsed, err := job.ParseBytes(path, data)
	if err != nil || parsed.Harness != "" {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	// A model with no name keeps the node, without a name line.
	if err := job.MergePatchFrontmatter(path, map[string]any{"model": "opus"}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if s := string(data); !strings.Contains(s, "harness:\n  model: opus\n") || strings.Contains(s, "name:") {
		t.Fatalf("model only: %s", s)
	}
	if parsed, _ := job.ParseBytes(path, data); parsed.Harness != "" || parsed.Model != "opus" {
		t.Fatalf("parsed model only: %+v", parsed)
	}
}

// writeConfig sets config.yaml for one test. The test must not be parallel.
func writeConfig(t *testing.T, body string) {
	t.Helper()
	path := home.ConfigPath()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}

func TestParseArchivedAt(t *testing.T) {
	t.Parallel()
	j, err := job.ParseBytes("/ws/agents/a/demo/JOB.md", []byte("---\nharness: fake\narchived_at: 2026-11-18T10:00:00Z\n---\n\nRun.\n"))
	if err != nil || j.ArchivedAt != "2026-11-18T10:00:00Z" || !j.IsArchived() {
		t.Fatalf("archived_at = %q, %v", j.ArchivedAt, err)
	}
	if len(j.Issues(false)) != 0 {
		t.Fatalf("valid time raised issues: %v", j.Issues(false))
	}
	j, err = job.ParseBytes("/ws/agents/a/demo/JOB.md", []byte("---\nharness: fake\narchived_at: yesterday\n---\n\nRun.\n"))
	if err != nil || !j.IsArchived() {
		t.Fatalf("bad value: archived=%v err=%v", j.IsArchived(), err)
	}
	if is := j.Issues(false).Err(); is == nil || is.Code != job.CodeArchivedAtInvalid {
		t.Fatalf("issue = %v", is)
	}
	// A parse failure still reads the key.
	j, err = job.ParseBytes("/ws/agents/a/demo/JOB.md", []byte("---\nharness: fake\narchived_at: 2026-11-18T10:00:00Z\nsession: [\n---\n"))
	if err == nil || !j.IsArchived() {
		t.Fatalf("parse failure: archived=%v err=%v", j.IsArchived(), err)
	}
	owned := &job.Job{OwnerArchivedAt: "2026-11-18T10:00:00Z"}
	if !owned.IsArchived() {
		t.Fatal("owner archive not counted")
	}
}
