package connectors

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
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
		"---\n\n{{body}} {{topic}} {{note}}\n"
	j, err := job.ParseBytes("a/JOB.md", []byte(md))
	if err != nil || j.ParseError != "" {
		t.Fatalf("parse: %v %s", err, j.ParseError)
	}
	tr := routing.Route{Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Types: []string{"mail.sent"}},
		Inputs: job.InputMap{{Name: "note", Value: "{{data.missing}}"}}}.Trigger("gmail")
	got := map[string]string{}
	for _, is := range r.triggerIssues(j, "route", tr) {
		got[is.Path] = is.Code
	}
	if got["route.types"] != job.CodeEventTypeUnknown || len(got) != 1 {
		t.Fatalf("unknown type: %v", got)
	}

	tr.Types = []string{"mail.received"}
	got = map[string]string{}
	for _, is := range r.triggerIssues(j, "route", tr) {
		got[is.Path] = is.Code
	}
	want := map[string]string{
		"route.inputs.body":  job.CodeMayBeEmpty,
		"route.inputs.topic": job.CodeRequiredUnfilled,
		"route.inputs.note":  job.CodeFieldUnknown,
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
		issues := tc.r.accessIssues("route", tc.names)
		if tc.code == "" && len(issues) != 0 || tc.code != "" && (len(issues) != 1 || issues[0].Code != tc.code || issues[0].Path != "route") {
			t.Errorf("%s: issues = %+v", tc.name, issues)
		}
	}
	msg := conn([]manifest.Operation{read, write}, false).accessIssues("route", nil)[0].Message
	if msg != "Runs that Gmail (a@b.c) starts can read it: Search mail. They cannot change it." {
		t.Errorf("message = %q", msg)
	}
}

func TestManifestCacheReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, manifest.FileName)
	write := func(desc string) {
		t.Helper()
		body := "schema: " + manifest.SchemaID + "\nname: demo\ntitle: Demo\ndescription: " + desc + "\nversion: 1.0.0\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("One.")
	c := &manifestCache{}
	m, err := c.Read(dir)
	if err != nil || m.Description != "One." {
		t.Fatalf("first read %q %v", m.Description, err)
	}
	// An unchanged file comes from the cache, so a change of the returned
	// copy does not reach it.
	m.Description = "changed"
	m.Title = "changed"
	if again, _ := c.Read(dir); again.Description != "One." || again.Title != "Demo" {
		t.Fatalf("cache entry changed: %+v", again)
	}
	if c.entries[dir].m.Description != "One." {
		t.Fatal("entry holds the caller copy")
	}
	// Plant a stale entry: an unchanged file is not read again.
	hit := c.entries[dir]
	hit.m.Description = "stale"
	c.entries[dir] = hit
	if got, _ := c.Read(dir); got.Description != "stale" {
		t.Fatalf("unchanged file was parsed again: %q", got.Description)
	}
	write("Two, longer.")
	if got, err := c.Read(dir); err != nil || got.Description != "Two, longer." {
		t.Fatalf("changed file: %q %v", got.Description, err)
	}
	// A parse error is not cached.
	if err := os.WriteFile(path, []byte("name: [broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(dir); err == nil {
		t.Fatal("want a parse error")
	}
}
