package connectors

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/job"
)

// A caller looks up the MCP command in its own PATH, so a command in the bin
// folder gets its full path.
func TestMCPServerBinCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "tool"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	for cmd, want := range map[string]string{
		"tool":       filepath.Join(dir, "bin", "tool"),
		"npx":        "npx",
		"/usr/bin/x": "/usr/bin/x",
	} {
		r := Resolved{Slug: "demo", Dir: dir, Manifest: manifest.Manifest{Actions: &manifest.Actions{MCP: &manifest.MCPAction{Command: cmd}}}}
		srv, _, err := r.MCPServer()
		if err != nil {
			t.Fatal(err)
		}
		if srv.Command != want {
			t.Errorf("%s: command = %q, want %q", cmd, srv.Command, want)
		}
	}
}

// TestTriggerIssues checks the trigger of a job against the event schema of
// a manifest.
func TestTriggerIssues(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	schema := `{"type":"object","required":["subject"],"properties":{"subject":{"type":"string"},"body":{"type":"string"}}}`
	if err := os.WriteFile(filepath.Join(dir, "mail.json"), []byte(schema), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Resolved{Title: "Gmail", Dir: dir, Manifest: manifest.Manifest{Events: &manifest.Events{
		Types: []manifest.EventType{{Type: "mail.received", Schema: "mail.json"}},
	}}}
	md := "---\nharness: fake\ninputs:\n  body: {type: string, required: true}\n  topic: {type: string, required: true}\n" +
		"  note: {type: string, required: true}\n" +
		"triggers:\n  - event: gmail\n    types: [mail.sent]\n    inputs: {note: \"{{data.missing}}\"}\n---\n\n{{body}} {{topic}} {{note}}\n"
	j, err := job.ParseBytes("a/JOB.md", []byte(md))
	if err != nil || j.ParseError != "" {
		t.Fatalf("parse: %v %s", err, j.ParseError)
	}
	got := map[string]string{}
	for _, is := range r.triggerIssues(j, 0, j.Triggers[0]) {
		got[is.Path] = is.Code
	}
	if got["triggers[0].types"] != job.CodeEventTypeUnknown || len(got) != 1 {
		t.Fatalf("unknown type: %v", got)
	}

	j.Triggers[0].Types = []string{"mail.received"}
	got = map[string]string{}
	for _, is := range r.triggerIssues(j, 0, j.Triggers[0]) {
		got[is.Path] = is.Code
	}
	want := map[string]string{
		"triggers[0].inputs.body":  job.CodeMayBeEmpty,
		"triggers[0].inputs.topic": job.CodeRequiredUnfilled,
		"triggers[0].inputs.note":  job.CodeFieldUnknown,
	}
	if len(got) != len(want) {
		t.Fatalf("issues = %v", got)
	}
	for p, c := range want {
		if got[p] != c {
			t.Errorf("%s = %q, want %q", p, got[p], c)
		}
	}
}

// TestTriggerAccess checks the access of the runs that an event trigger
// starts, and the issue it gives.
func TestTriggerAccess(t *testing.T) {
	t.Parallel()
	read := manifest.Operation{Name: "search", Label: "Search mail", ReadOnly: true}
	write := manifest.Operation{Name: "label", Label: "Add a label"}
	conn := func(ops []manifest.Operation, channel bool) Resolved {
		r := Resolved{ID: "con_1", Slug: "gmail-3", Title: "Gmail (a@b.c)", Manifest: manifest.Manifest{Actions: &manifest.Actions{Operations: ops}}}
		if channel {
			r.Manifest.Channel = &manifest.Channel{}
		}
		return r
	}
	cases := []struct {
		name  string
		r     Resolved
		names []string
		want  TriggerAccess
		code  string
	}{
		{"named by slug", conn([]manifest.Operation{write}, false), []string{"gmail-3"}, AccessFull, ""},
		{"named by id", conn(nil, false), []string{"con_1"}, AccessFull, ""},
		{"read only", conn([]manifest.Operation{read, write}, false), []string{"other"}, AccessReadOnly, job.CodeTriggerAccessReadOnly},
		{"none", conn([]manifest.Operation{write}, false), nil, AccessNone, job.CodeTriggerAccessNone},
		{"channel", conn(nil, true), nil, AccessChannel, ""},
	}
	for _, tc := range cases {
		if got := tc.r.TriggerAccess(tc.r.Named(tc.names)); got != tc.want {
			t.Errorf("%s: access = %s, want %s", tc.name, got, tc.want)
		}
		issues := tc.r.accessIssues(0, tc.names)
		if tc.code == "" && len(issues) != 0 || tc.code != "" && (len(issues) != 1 || issues[0].Code != tc.code || issues[0].Path != "triggers[0].event") {
			t.Errorf("%s: issues = %+v", tc.name, issues)
		}
	}
	msg := conn([]manifest.Operation{read, write}, false).accessIssues(0, nil)[0].Message
	if msg != "Runs that Gmail (a@b.c) starts can read it: Search mail. They cannot change it." {
		t.Errorf("message = %q", msg)
	}
}
