package manifest_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
)

func TestLoadValid(t *testing.T) {
	t.Parallel()
	m, err := manifest.Load("testdata/whatsapp")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Capabilities(); !slices.Equal(got, []string{"actions", "events", "channel"}) {
		t.Fatalf("capabilities %v", got)
	}
	if got := m.Modes(); !slices.Equal(got, []string{"reply", "push"}) {
		t.Fatalf("modes %v", got)
	}
	r, err := m.SkillCompat()
	if err != nil || !r.Contains(manifest.MustVersion("1.9.0")) || r.Contains(manifest.MustVersion("2.0.0")) {
		t.Fatalf("compat %v %v", r, err)
	}
	for _, f := range []string{"testdata/lines.yaml", "testdata/mcp.yaml"} {
		if err := load(t, f).Validate(); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

func TestParseUnknownKey(t *testing.T) {
	t.Parallel()
	_, err := manifest.Parse([]byte("schema: trebi-connector/1\nname: a\nsend: x\n"))
	if err == nil {
		t.Fatal("want an unknown-key error")
	}
}

func TestValidateRules(t *testing.T) {
	t.Parallel()
	base := func() manifest.Manifest { return load(t, "testdata/whatsapp/trebi-connector.yaml") }
	cases := []struct {
		name   string
		edit   func(m *manifest.Manifest)
		fields []string
	}{
		{"bad schema", func(m *manifest.Manifest) { m.Schema = "trebi-connector/2" }, []string{"schema"}},
		{"reserved", func(m *manifest.Manifest) { m.Name = "email" }, []string{"name"}},
		{"bad name", func(m *manifest.Manifest) { m.Name = "A_b" }, []string{"name"}},
		{"bad version", func(m *manifest.Manifest) { m.Version = "1.3" }, []string{"version"}},
		{"no title", func(m *manifest.Manifest) { m.Title = "" }, []string{"title"}},
		{"no blocks", func(m *manifest.Manifest) { m.Actions, m.Events, m.Channel, m.Setup = nil, nil, nil, nil }, []string{""}},
		{"channel on lines", func(m *manifest.Manifest) { m.Events.Protocol = "lines"; m.Setup = nil }, []string{"channel"}},
		{"login on lines", func(m *manifest.Manifest) { m.Events.Protocol = "lines"; m.Channel = nil }, []string{"setup.login"}},
		{"lines field on protocol", func(m *manifest.Manifest) { m.Events.ID = "d.id" }, []string{"events.id"}},
		{"message path on protocol", func(m *manifest.Manifest) { m.Events.Room = "chat.id" }, []string{"events.room"}},
		{"message paths on lines", func(m *manifest.Manifest) {
			m.Events.Protocol, m.Channel, m.Setup, m.Events.Format = "lines", nil, nil, manifest.FormatJSON
			m.Events.Room, m.Events.RoomName, m.Events.Thread, m.Events.Sender = "channel.id", "channel.name", "thread_id", "user.id"
			m.Events.SenderName, m.Events.Type, m.Events.Text = "user.name", "kind", "items.0.text"
		}, nil},
		{"message path needs json", func(m *manifest.Manifest) {
			m.Events.Protocol, m.Channel, m.Setup, m.Events.Format = "lines", nil, nil, manifest.FormatLine
			m.Events.Text = "text"
		}, []string{"events.text"}},
		{"message path no format", func(m *manifest.Manifest) {
			m.Events.Protocol, m.Channel, m.Setup = "lines", nil, nil
			m.Events.SenderName = "user.name"
		}, []string{"events.sender_name"}},
		{"bad message path", func(m *manifest.Manifest) {
			m.Events.Protocol, m.Channel, m.Setup, m.Events.Format = "lines", nil, nil, manifest.FormatJSON
			m.Events.Thread = "a..b"
		}, []string{"events.thread"}},
		{"unknown feature", func(m *manifest.Manifest) { m.Channel.Features = append(m.Channel.Features, "fly") }, []string{"channel.features"}},
		{"replies feature", func(m *manifest.Manifest) { m.Channel.Features = append(m.Channel.Features, manifest.FeatureReplies) }, nil},
		{"unknown login", func(m *manifest.Manifest) { m.Setup.Login = []string{"sms"} }, []string{"setup.login"}},
		{"bad compat", func(m *manifest.Manifest) { m.Skill.Compat = ">=x" }, []string{"skill.compat"}},
		{"skill escapes", func(m *manifest.Manifest) { m.Skill.Path = "../other" }, []string{"skill.path"}},
		{"bad template", func(m *manifest.Manifest) {
			m.Install.GitHubRelease = &manifest.GitHubRelease{Repo: "a/b", Tag: "v{version}", Asset: "x_{goos}.tgz"}
		}, []string{"install.github_release.asset"}},
		{"install needs bin", func(m *manifest.Manifest) { m.Install.Bin = "" }, []string{"install.bin"}},
		{"build valid", func(m *manifest.Manifest) { m.Install.Build = &manifest.Build{Go: "./cmd/x"} }, nil},
		{"build root", func(m *manifest.Manifest) { m.Install.Build = &manifest.Build{Go: "."} }, nil},
		{"build escapes", func(m *manifest.Manifest) { m.Install.Build = &manifest.Build{Go: "../x"} }, []string{"install.build.go"}},
		{"build empty", func(m *manifest.Manifest) { m.Install.Build = &manifest.Build{} }, []string{"install.build.go"}},
		{"build needs bin", func(m *manifest.Manifest) {
			m.Install.Catalog, m.Install.Bin, m.Install.Build = false, "", &manifest.Build{Go: "."}
		}, []string{"install.bin"}},
		{"mcp two forms", func(m *manifest.Manifest) {
			m.Actions.MCP = &manifest.MCPAction{Command: "x", URL: "https://x"}
		}, []string{"actions.mcp"}},
		{"mcp registry ref", func(m *manifest.Manifest) {
			m.Actions.MCP = &manifest.MCPAction{Registry: "official", Name: "io.github.acme/tool", Version: "1.2.0"}
		}, nil},
		{"schema platform", func(m *manifest.Manifest) { m.Platforms = []string{"Darwin ARM"} }, []string{"platforms"}},
		{"schema format", func(m *manifest.Manifest) { m.Channel.Limits.Formats = []string{"rtf"} }, []string{"channel.limits.formats"}},
		{"input name", func(m *manifest.Manifest) {
			m.Setup.Inputs = []manifest.Input{{Name: "A-B"}}
		}, []string{"setup.inputs[0].name"}},
		{"subscriptions valid", func(m *manifest.Manifest) { m.Events.Subscriptions = watch() }, nil},
		{"subscriptions on lines", func(m *manifest.Manifest) {
			m.Events.Protocol, m.Channel, m.Setup = "lines", nil, nil
			m.Events.Subscriptions = watch()
		}, []string{"events.subscriptions"}},
		{"subscriptions no action", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Action = ""
		}, []string{"events.subscriptions.action"}},
		{"subscriptions long action", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Action = "Watch every repository of every organization you own"
		}, []string{"events.subscriptions.action"}},
		{"subscriptions no modes", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Modes = nil
		}, []string{"events.subscriptions.modes"}},
		{"subscriptions duplicate mode", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Modes = []string{"poll", "poll"}
		}, []string{"events.subscriptions.modes"}},
		{"subscriptions api needs webhook", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Webhook = nil
		}, []string{"events.subscriptions.webhook"}},
		{"subscriptions poll needs no webhook", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Modes, m.Events.Subscriptions.Webhook = []string{"poll", "stream"}, nil
		}, nil},
		{"subscriptions bad field name", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Fields[0].Name = "Team"
		}, []string{"events.subscriptions.fields[0].name"}},
		{"subscriptions duplicate field", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Fields[1].Name = "team"
		}, []string{"events.subscriptions.fields[1].name"}},
		{"subscriptions no label", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Fields[0].Label = ""
		}, []string{"events.subscriptions.fields[0].label"}},
		{"subscriptions choices and dynamic", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Fields[1].Dynamic = true
		}, []string{"events.subscriptions.fields[1].dynamic"}},
		{"subscriptions bad default", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Fields[1].Default = "Project"
		}, []string{"events.subscriptions.fields[1].default"}},
		{"subscriptions two rooms", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Fields[1].Room, m.Events.Subscriptions.Fields[1].Multiple = true, false
		}, []string{"events.subscriptions.fields"}},
		{"subscriptions multiple room", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Fields[0].Multiple = true
		}, []string{"events.subscriptions.fields[0].multiple"}},
		{"subscriptions denied header", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Webhook.Headers = []string{"x-ok", "authorization", "cf-ray", "x-forwarded-for"}
		}, []string{"events.subscriptions.webhook.headers[1]", "events.subscriptions.webhook.headers[2]", "events.subscriptions.webhook.headers[3]"}},
		{"subscriptions bad header", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Webhook.Headers = []string{"X-Upper"}
		}, []string{"events.subscriptions.webhook.headers[0]"}},
		{"subscriptions bad rule", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Webhook.Handshake = []string{"ok:POST"}
		}, []string{"events.subscriptions.webhook.handshake[0]"}},
		{"subscriptions too many rules", func(m *manifest.Manifest) {
			m.Events.Subscriptions = watch()
			m.Events.Subscriptions.Webhook.Handshake = []string{"ok:GET", "ok:HEAD", "json_echo:a", "json_echo:b", "json_echo:c"}
		}, []string{"events.subscriptions.webhook.handshake"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.edit(&m)
			err := m.Validate()
			if len(tc.fields) == 0 {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			var ve *manifest.ValidationError
			if !errors.As(err, &ve) || !errors.Is(err, manifest.ErrInvalid) {
				t.Fatalf("want ValidationError, got %v", err)
			}
			got := map[string]bool{}
			for _, f := range ve.Fields {
				got[f.Field] = true
			}
			for _, want := range tc.fields {
				if !got[want] {
					t.Fatalf("missing field %q in %v", want, ve.Fields)
				}
			}
		})
	}
}

func TestBuildDecode(t *testing.T) {
	t.Parallel()
	m, err := manifest.Parse([]byte("install:\n  build: {go: ./cmd/x}\n  bin: x\n"))
	if err != nil || m.Install.Build == nil || m.Install.Build.Go != "./cmd/x" {
		t.Fatalf("build %+v, %v", m.Install, err)
	}
	if _, err := manifest.Parse([]byte("install:\n  build: {go: ., npm: x}\n")); err == nil {
		t.Fatal("want an unknown-key error for build.npm")
	}
}

func TestBuildModuleDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cmd", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := (manifest.Build{Go: "cmd/x"}).ModuleDir(root); err == nil {
		t.Fatal("want a missing go.mod error")
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{".", "cmd/x", "./cmd/x"} {
		got, err := (manifest.Build{Go: pkg}).ModuleDir(root)
		if err != nil || got != filepath.Clean(root) {
			t.Fatalf("%s: %q, %v", pkg, got, err)
		}
	}
}

func TestPatch(t *testing.T) {
	t.Parallel()
	m := load(t, "testdata/whatsapp/trebi-connector.yaml")
	out, err := manifest.Patch(m, json.RawMessage(`{"channel":{"limits":{"max_text":1800}},"warning":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.Channel.Limits.MaxText != 1800 || !slices.Equal(out.Channel.Limits.Formats, []string{"text"}) {
		t.Fatalf("limits %+v", out.Channel.Limits)
	}
	if out.Warning != "" || m.Warning == "" {
		t.Fatalf("warning: patched %q base %q", out.Warning, m.Warning)
	}
	if _, err := manifest.Patch(m, json.RawMessage(`{"channel":{"bogus":1}}`)); err == nil {
		t.Fatal("want an unknown-field error")
	}
}

func TestMergePatchRFC7386(t *testing.T) {
	t.Parallel()
	// Cases from RFC 7386 Appendix A.
	cases := []struct{ target, patch, want string }{
		{`{"a":"b"}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"b"}`, `{"b":"c"}`, `{"a":"b","b":"c"}`},
		{`{"a":"b"}`, `{"a":null}`, `{}`},
		{`{"a":["b"]}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":{"b":"c"}}`, `{"a":{"b":"d","c":null}}`, `{"a":{"b":"d"}}`},
		{`["a","b"]`, `["c","d"]`, `["c","d"]`},
		{`{"e":null}`, `{"a":1}`, `{"a":1,"e":null}`},
		{`{}`, `{"a":{"bb":{"ccc":null}}}`, `{"a":{"bb":{}}}`},
	}
	for _, tc := range cases {
		got, err := manifest.MergePatch([]byte(tc.target), []byte(tc.patch))
		if err != nil {
			t.Fatal(err)
		}
		var g, w any
		_ = json.Unmarshal(got, &g)
		_ = json.Unmarshal([]byte(tc.want), &w)
		gb, _ := json.Marshal(g)
		wb, _ := json.Marshal(w)
		if string(gb) != string(wb) {
			t.Fatalf("%s + %s = %s, want %s", tc.target, tc.patch, gb, wb)
		}
	}
}

func TestVersionsAndTracks(t *testing.T) {
	t.Parallel()
	v := manifest.MustVersion
	if v("1.3.0+2").CompareFull(v("1.3.0")) <= 0 || v("1.3.0+2").Compare(v("1.3.0")) != 0 {
		t.Fatal("build metadata order")
	}
	if v("1.0.0-rc.1").Compare(v("1.0.0")) >= 0 || v("1.0.0-rc.2").Compare(v("1.0.0-rc.10")) >= 0 {
		t.Fatal("prerelease order")
	}
	tracks := []struct {
		track     manifest.Track
		cur, next string
		want      bool
	}{
		{manifest.TrackExact, "1.3.0", "1.3.1", false},
		{manifest.TrackPatch, "1.3.0", "1.3.1", true},
		{manifest.TrackPatch, "1.3.0", "1.3.0+1", true},
		{manifest.TrackPatch, "1.3.0", "1.4.0", false},
		{manifest.TrackMinor, "1.3.0", "1.4.0", true},
		{manifest.TrackMinor, "1.3.0", "2.0.0", false},
		{manifest.TrackLatest, "1.3.0", "2.0.0", true},
		{manifest.TrackLatest, "1.3.0", "1.2.0", false},
	}
	for _, tc := range tracks {
		if got := tc.track.Follows(v(tc.cur), v(tc.next)); got != tc.want {
			t.Fatalf("%s %s→%s = %v", tc.track, tc.cur, tc.next, got)
		}
	}
	ranges := []struct {
		r, v string
		want bool
	}{
		{">=1.3 <2", "1.3.0", true}, {">=1.3 <2", "2.0.0", false}, {"^1.2", "1.9.9", true},
		{"~1.2.3", "1.3.0", false}, {"1.x", "1.7.0", true}, {"*", "9.0.0", true},
		{"<1 || >=3", "3.1.0", true}, {"<1 || >=3", "2.0.0", false}, {"1.3.0", "1.3.0+4", true},
	}
	for _, tc := range ranges {
		r, err := manifest.ParseRange(tc.r)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Contains(v(tc.v)); got != tc.want {
			t.Fatalf("%q contains %s = %v", tc.r, tc.v, got)
		}
	}
}

func TestExpandTemplate(t *testing.T) {
	t.Parallel()
	got := manifest.ExpandTemplate("whatsapp-cli_{version}_{os}_{arch}.tar.gz", "1.3.0", "darwin", "arm64")
	if got != "whatsapp-cli_1.3.0_darwin_arm64.tar.gz" {
		t.Fatal(got)
	}
}

func load(t *testing.T, file string) manifest.Manifest {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEventSchemas(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/message.json", []byte(`{"type":"object","required":["chat"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := manifest.Manifest{Events: &manifest.Events{Types: []manifest.EventType{{Type: "message", Schema: "message.json"}, {Type: "plain"}}}}
	got, err := m.EventSchemas(dir)
	if err != nil || len(got) != 1 {
		t.Fatalf("schemas = %v %v", got, err)
	}
	if got["message"].Validate(map[string]any{}) == nil || got["message"].Validate(map[string]any{"chat": "x"}) != nil {
		t.Fatal("the schema does not validate")
	}
	m.Events.Types[0].Schema = "missing.json"
	if _, err := m.EventSchemas(dir); err == nil {
		t.Fatal("a missing schema file must fail")
	}
}

// watch is a valid subscriptions block with a room field and a multiple
// choice field.
func watch() *manifest.Subscriptions {
	return &manifest.Subscriptions{
		Action: "Watch a team",
		Modes:  []string{manifest.ModeAPI, manifest.ModePoll},
		Fields: []manifest.SubscriptionField{
			{Name: "team", Label: "Team", Dynamic: true, Room: true},
			{Name: "resources", Label: "Changes to", Choices: []string{"Issue", "Comment"}, Multiple: true, Default: "Issue"},
		},
		Webhook: &manifest.Webhook{Headers: []string{"linear-signature"}, Handshake: []string{"json_echo:challenge"}},
	}
}

func TestSubscriptionsCheck(t *testing.T) {
	t.Parallel()
	s := *watch()
	s.Fields[0].Required = true
	types := []manifest.EventType{{Type: "issue"}, {Type: "comment"}}
	cases := []struct {
		name   string
		values map[string][]string
		types  []string
		fields []string
	}{
		{"valid", map[string][]string{"team": {"ENG"}, "resources": {"Issue", "Comment"}}, []string{"issue"}, nil},
		{"required", map[string][]string{"resources": {"Issue"}}, nil, []string{"values.team"}},
		{"unknown field", map[string][]string{"team": {"ENG"}, "label": {"x"}}, nil, []string{"values.label"}},
		{"one value", map[string][]string{"team": {"ENG", "OPS"}}, nil, []string{"values.team"}},
		{"bad choice", map[string][]string{"team": {"ENG"}, "resources": {"Cycle"}}, nil, []string{"values.resources"}},
		{"long value", map[string][]string{"team": {string(make([]byte, manifest.MaxValueBytes+1))}}, nil, []string{"values.team"}},
		{"unknown type", map[string][]string{"team": {"ENG"}}, []string{"push"}, []string{"types"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, e := range s.Check(tc.values, tc.types, types) {
				got = append(got, e.Field)
			}
			if !slices.Equal(got, tc.fields) {
				t.Fatalf("fields %v, want %v", got, tc.fields)
			}
		})
	}
	if got := s.WithDefaults(map[string][]string{"team": {"ENG"}}); !slices.Equal(got["resources"], []string{"Issue"}) {
		t.Fatalf("defaults %v", got)
	}
}

func TestFeaturesWithSubscriptions(t *testing.T) {
	t.Parallel()
	m := load(t, "testdata/whatsapp/trebi-connector.yaml")
	m.Events.Subscriptions = watch()
	got := m.Features()
	if !slices.Contains(got, manifest.FeatureSubscriptions) || got[len(got)-1] != manifest.FeatureWebhooks {
		t.Fatalf("features %v", got)
	}
	m.Events.Subscriptions.Webhook = nil
	if slices.Contains(m.Features(), manifest.FeatureWebhooks) {
		t.Fatal("webhooks without a webhook block")
	}
}

// TestWebhookAnswer runs the shared handshake cases. The cloud runs the
// same file.
func TestWebhookAnswer(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "cloudlink", "testdata", "contract", "hook-handshake.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string            `json:"name"`
		Rules   []string          `json:"rules"`
		Method  string            `json:"method"`
		Query   string            `json:"query"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
		Answer  *struct {
			Status      int               `json:"status"`
			Headers     map[string]string `json:"headers"`
			ContentType string            `json:"content_type"`
			Body        string            `json:"body"`
		} `json:"answer"`
		Forward bool `json:"forward"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 8 {
		t.Fatalf("only %d cases", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			q, err := url.ParseQuery(tc.Query)
			if err != nil {
				t.Fatal(err)
			}
			h := http.Header{}
			for k, v := range tc.Headers {
				h.Set(k, v)
			}
			got := manifest.Webhook{Handshake: tc.Rules}.Answer(manifest.HandshakeRequest{Method: tc.Method, Query: q, Headers: h, Body: []byte(tc.Body)})
			if tc.Answer == nil {
				if got != nil {
					t.Fatalf("answer %+v, want a normal delivery", got)
				}
				if !tc.Forward {
					t.Fatal("a normal delivery is always forwarded")
				}
				return
			}
			if got == nil {
				t.Fatal("no answer")
			}
			if got.Status != tc.Answer.Status || got.ContentType != tc.Answer.ContentType || got.Body != tc.Answer.Body || got.Forward != tc.Forward {
				t.Fatalf("answer %+v, want %+v forward %v", got, tc.Answer, tc.Forward)
			}
			if len(got.Headers) != len(tc.Answer.Headers) {
				t.Fatalf("headers %v, want %v", got.Headers, tc.Answer.Headers)
			}
			for k, v := range tc.Answer.Headers {
				if got.Headers[k] != v {
					t.Fatalf("header %s = %q, want %q", k, got.Headers[k], v)
				}
			}
		})
	}
}
