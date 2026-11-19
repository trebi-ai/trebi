package job_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

const typedJob = `---
harness: claude
inputs:
  from:    {type: string, label: From, required: true}
  subject: {type: string, label: Subject}
  body:    {type: string, label: Email text, format: multiline, required: true}
  tone:    {type: string, label: Tone, enum: [friendly, formal, short], default: friendly}
  count:   {type: integer, min: 1, max: 5, default: 2}
---

Reply to {{from}} about {{subject}}: {{body}}. Tone {{tone}}, {{count}} points. {{note}}
`

func TestInputsParseInOrder(t *testing.T) {
	t.Parallel()
	j := parseTyped(t, typedJob)
	manual, mapped := typedRoutes(t)
	var names []string
	for _, in := range j.InputList(true) {
		names = append(names, in.Name)
	}
	if got := strings.Join(names, ","); got != "from,subject,body,tone,count,note" {
		t.Fatalf("input order = %s", got)
	}
	tone, _ := j.Inputs.Get("tone")
	if tone.Default != "friendly" || !reflect.DeepEqual(tone.Enum, []string{"friendly", "formal", "short"}) {
		t.Fatalf("tone = %+v", tone)
	}
	count, _ := j.Inputs.Get("count")
	if count.Type != job.InputInteger || *count.Min != 1 || *count.Max != 5 || count.Default != int64(2) {
		t.Fatalf("count = %+v", count)
	}
	note := j.InputList(true)[5]
	if note.Declared || note.Type != job.InputString {
		t.Fatalf("note = %+v, want an undeclared string", note)
	}
	if !manual.Manual || !reflect.DeepEqual(manual.Types, []string{"mail.received"}) {
		t.Fatalf("trigger 0 = %+v", manual)
	}
	m := mapped.Inputs
	if len(m) != 4 || m[0].Name != "from" || m[3].Value != "short" {
		t.Fatalf("trigger 1 inputs = %+v", m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"from":"{{sender}}","subject":"Slack message in {{room_name}}","body":"{{text}}","tone":"short"}`; string(raw) != want {
		t.Fatalf("json = %s", raw)
	}
}

func TestInputCoerce(t *testing.T) {
	t.Parallel()
	one, five := 1.0, 5.0
	cases := []struct {
		name    string
		in      job.Input
		v       any
		want    any
		wantErr string
	}{
		{"string", job.Input{Type: job.InputString}, "hi", "hi", ""},
		{"empty is nil", job.Input{Type: job.InputString}, "  ", nil, ""},
		{"list joins", job.Input{Type: job.InputString}, []any{"a", "b"}, "a, b", ""},
		{"object json", job.Input{Type: job.InputString}, map[string]any{"a": 1.0}, `{"a":1}`, ""},
		{"enum ok", job.Input{Type: job.InputString, Enum: []string{"x"}}, "x", "x", ""},
		{"enum bad", job.Input{Name: "tone", Type: job.InputString, Enum: []string{"x"}}, "y", nil, "tone: must be one of x"},
		{"number text", job.Input{Type: job.InputNumber}, "2.5", 2.5, ""},
		{"number bad", job.Input{Label: "Size", Type: job.InputNumber}, "big", nil, "Size: must be a number"},
		{"integer", job.Input{Type: job.InputInteger}, 3.0, int64(3), ""},
		{"integer frac", job.Input{Name: "n", Type: job.InputInteger}, 3.5, nil, "n: must be a whole number"},
		{"min", job.Input{Name: "n", Type: job.InputInteger, Min: &one}, "0", nil, "n: must be at least 1"},
		{"max", job.Input{Name: "n", Type: job.InputNumber, Max: &five}, 6.0, nil, "n: must be at most 5"},
		{"bool text", job.Input{Type: job.InputBoolean}, "yes", true, ""},
		{"bool bad", job.Input{Name: "b", Type: job.InputBoolean}, "maybe", nil, "b: must be true or false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.in.Coerce(tc.v)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, %v; want %#v", got, err, tc.want)
			}
		})
	}
}

func TestFillOrder(t *testing.T) {
	t.Parallel()
	j := parseTyped(t, typedJob)
	manual, mapped := typedRoutes(t)
	ev := &job.EventVars{
		Vars: map[string]string{"sender": "ana", "room_name": "general", "text": "hello"},
		Payload: map[string]any{"data": map[string]any{
			"from": "ana@x.io", "subject": "Hi", "body": "Mail text", "note": []any{"a", "b"},
		}},
	}
	// Manual trigger: no map, so each input takes the event field of its name.
	f := j.Fill(&manual, ev, map[string]any{"tone": "formal"})
	want := map[string]any{"from": "ana@x.io", "subject": "Hi", "body": "Mail text", "tone": "formal", "count": int64(2), "note": "a, b"}
	if !reflect.DeepEqual(f.Values, want) {
		t.Fatalf("values = %#v", f.Values)
	}
	wantSrc := map[string]string{"from": job.SourceName, "subject": job.SourceName, "body": job.SourceName, "tone": job.SourceGiven, "count": job.SourceDefault, "note": job.SourceName}
	if !reflect.DeepEqual(f.Sources, wantSrc) {
		t.Fatalf("sources = %#v", f.Sources)
	}
	// Map trigger: templates and a literal win over the event field.
	f = j.Fill(&mapped, ev, nil)
	if f.Values["from"] != "ana" || f.Values["subject"] != "Slack message in general" || f.Values["tone"] != "short" {
		t.Fatalf("values = %#v", f.Values)
	}
	if f.Sources["from"] != job.SourceMap || f.Sources["tone"] != job.SourceLiteral {
		t.Fatalf("sources = %#v", f.Sources)
	}
	// No event: required inputs without a default are missing.
	f = j.Fill(nil, nil, nil)
	if !reflect.DeepEqual(f.Missing, []string{"from", "body"}) || f.Sources["from"] != job.SourceEmpty {
		t.Fatalf("missing = %v, sources = %v", f.Missing, f.Sources)
	}
	// An empty input renders as "", so its placeholder does not stay.
	if got := job.ExpandVars("to {{from}}: {{body}}", f.Text()); got != "to : " {
		t.Fatalf("prompt = %q", got)
	}
	// A bad given value is an error and counts as empty.
	f = j.Fill(nil, nil, map[string]any{"tone": "loud", "from": "x", "body": "y"})
	if f.Errors["tone"] == "" || f.Values["tone"] != nil {
		t.Fatalf("errors = %v values = %v", f.Errors, f.Values)
	}
}

func TestEventVarsRenderTopLevelData(t *testing.T) {
	t.Parallel()
	// A native event has no data object; data.<path> reads the top level.
	ev := job.EventVars{Payload: map[string]any{"subject": "Hi", "to": []any{"a", "b"}}}
	if got := ev.Render("{{data.subject}} to {{data.to}} {{data.none}}"); got != "Hi to a, b " {
		t.Fatalf("render = %q", got)
	}
}

func TestIssues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		md   string
		want []string // path code/severity
	}{
		{"clean", typedJob, []string{
			"prompt placeholder_undeclared/warning",
		}},
		{"bad input keys", "---\ninputs:\n  a: {type: text}\n  b: {type: number, enum: [x], color: red}\n  c: {type: integer, default: 1.5}\n---\n\n{{a}} {{b}} {{c}}\n", []string{
			"inputs.a.type input_type/error",
			"inputs.b.color input_invalid/error",
			"inputs.b.enum input_invalid/error",
			"inputs.c.default input_invalid/error",
		}},
		{"conflicts", "---\ninputs:\n  a: {type: string}\ntriggers:\n  - every: 1h\n    manual: true\n  - every: 2h\n    inputs: {a: '{{text}}', z: x}\n---\n\n{{a}}\n", []string{
			"triggers[0].manual trigger_conflict/error",
			"triggers[1].inputs.a template_invalid/error",
			"triggers[1].inputs.z input_unknown/error",
		}},
		{"schedule required", "---\ninputs:\n  a: {type: string, required: true}\n  b: {type: integer}\ntriggers:\n  - cron: '0 9 * * *'\n    inputs: {b: two}\n---\n\nHi\n", []string{
			"triggers[0].inputs.b input_invalid/error",
			"triggers[0].inputs.a required_unfilled/error",
			"inputs.a input_unused/warning",
			"inputs.b input_unused/warning",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j := parseTyped(t, tc.md)
			var got []string
			for _, is := range j.Issues(true) {
				got = append(got, is.Path+" "+is.Code+"/"+is.Severity)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("issues =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// TestRouteInputIssues proves the checks of the inputs map of a route.
func TestRouteInputIssues(t *testing.T) {
	t.Parallel()
	j := parseTyped(t, typedJob)
	manual, mapped := typedRoutes(t)
	got := func(tr job.Trigger) []string {
		var out []string
		for _, is := range j.RouteIssues("routes[rt_1]", tr) {
			out = append(out, is.Path+" "+is.Code+"/"+is.Severity)
		}
		return out
	}
	if want := []string{"routes[rt_1].inputs.from required_unfilled/info", "routes[rt_1].inputs.body required_unfilled/info"}; !reflect.DeepEqual(got(manual), want) {
		t.Fatalf("manual = %v", got(manual))
	}
	if g := got(mapped); len(g) != 0 {
		t.Fatalf("mapped = %v", g)
	}
	bad := mapped
	bad.Inputs = job.InputMap{{Name: "from", Value: "{{text"}, {Name: "zzz", Value: "x"}}
	bad.Batch, bad.BatchMS, bad.Manual = "5m", 300000, true
	want := []string{
		"routes[rt_1].manual trigger_conflict/error",
		"routes[rt_1].inputs.from template_invalid/error",
		"routes[rt_1].inputs.zzz input_unknown/error",
		"routes[rt_1].inputs.body required_unfilled/info",
	}
	if g := got(bad); !reflect.DeepEqual(g, want) {
		t.Fatalf("bad = %v", g)
	}
}

// TestIssuesRouted proves the event vars of the prompt need a route.
func TestIssuesRouted(t *testing.T) {
	t.Parallel()
	j := parseTyped(t, "---\ninputs:\n  topic: {type: string}\n---\n\n{{topic}}: {{text}}\n")
	has := func(routed bool) bool {
		for _, is := range j.Issues(routed) {
			if is.Code == job.CodePlaceholderUndeclared {
				return true
			}
		}
		return false
	}
	if !has(false) || has(true) {
		t.Fatalf("undeclared: unrouted=%v routed=%v", has(false), has(true))
	}
}

func TestPatchInputsAndTriggers(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "reply", "JOB.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nharness: claude\ntitle: Reply\n---\n\nReply to {{body}} in a {{tone}} way.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := job.MergePatchFrontmatter(path, map[string]any{
		"inputs": []any{
			map[string]any{"name": "body", "type": "string", "label": "Email text", "format": "multiline", "required": true},
			map[string]any{"name": "tone", "type": "string", "enum": []any{"friendly", "short"}, "default": "friendly"},
		},
		"triggers": []any{
			map[string]any{"cron": "0 9 * * *", "inputs": map[string]any{"body": "Morning"}},
			map[string]any{"every": "1h", "inputs": map[string]any{"body": "Daily check"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	j, err := job.ParseFile(path)
	if err != nil || j.ParseError != "" {
		t.Fatalf("parse: %v %s", err, j.ParseError)
	}
	if len(j.Inputs) != 2 || j.Inputs[0].Name != "body" || j.Inputs[1].Default != "friendly" {
		t.Fatalf("inputs = %+v", j.Inputs)
	}
	if len(j.Triggers) != 2 || j.Triggers[0].Cron == "" || j.Triggers[1].Inputs[0].Value != "Daily check" {
		t.Fatalf("triggers = %+v", j.Triggers)
	}
	if j.Title != "Reply" || j.Issues(false).Err() != nil {
		t.Fatalf("title = %q issues = %+v", j.Title, j.Issues(false))
	}
	// A bad entry is an error before any write; null removes the block.
	before, _ := os.ReadFile(path)
	if err := job.MergePatchFrontmatter(path, map[string]any{"inputs": []any{map[string]any{"name": "x", "type": "date"}}}); err == nil {
		t.Fatal("want an error for an unknown type")
	}
	if err := job.MergePatchFrontmatter(path, map[string]any{"triggers": []any{map[string]any{"cron": "x", "every": "1h"}}}); err == nil {
		t.Fatal("want an error for a bad trigger")
	}
	var ee *job.EventTriggerError
	if err := job.MergePatchFrontmatter(path, map[string]any{"triggers": []any{map[string]any{"event": "gmail"}}}); !errors.As(err, &ee) {
		t.Fatalf("event entry: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("a failed patch wrote the file:\n%s", after)
	}
	if err := job.MergePatchFrontmatter(path, map[string]any{"inputs": nil}); err != nil {
		t.Fatal(err)
	}
	if j, _ = job.ParseFile(path); len(j.Inputs) != 0 || len(j.Triggers) != 2 {
		t.Fatalf("after remove: inputs = %+v triggers = %d", j.Inputs, len(j.Triggers))
	}
}

func parseTyped(t *testing.T, md string) *job.Job {
	t.Helper()
	j, err := job.ParseBytes("reply/JOB.md", []byte(md))
	if err != nil {
		t.Fatal(err)
	}
	if j.ParseError != "" {
		t.Fatalf("parse error: %s", j.ParseError)
	}
	return j
}

// typedRoutes are the two event triggers that routes give to typedJob.
func typedRoutes(t *testing.T) (manual, mapped job.Trigger) {
	t.Helper()
	manual, err := job.NormalizeEventTrigger(job.TriggerYAML{Types: []string{"mail.received"}})
	if err != nil {
		t.Fatal(err)
	}
	manual.Manual = true
	mapped, err = job.NormalizeEventTrigger(job.TriggerYAML{Types: []string{"mention"}, Inputs: job.InputMap{
		{Name: "from", Value: "{{sender}}"}, {Name: "subject", Value: "Slack message in {{room_name}}"},
		{Name: "body", Value: "{{text}}"}, {Name: "tone", Value: "short"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mapped.Inputs = job.InputMap{
		{Name: "from", Value: "{{sender}}"}, {Name: "subject", Value: "Slack message in {{room_name}}"},
		{Name: "body", Value: "{{text}}"}, {Name: "tone", Value: "short"},
	}
	return manual, mapped
}
