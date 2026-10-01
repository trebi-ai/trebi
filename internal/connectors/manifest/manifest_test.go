package manifest_test

import (
	"encoding/json"
	"errors"
	"os"
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
		{"unknown feature", func(m *manifest.Manifest) { m.Channel.Features = append(m.Channel.Features, "fly") }, []string{"channel.features"}},
		{"replies feature", func(m *manifest.Manifest) { m.Channel.Features = append(m.Channel.Features, manifest.FeatureReplies) }, nil},
		{"unknown login", func(m *manifest.Manifest) { m.Setup.Login = []string{"sms"} }, []string{"setup.login"}},
		{"bad compat", func(m *manifest.Manifest) { m.Skill.Compat = ">=x" }, []string{"skill.compat"}},
		{"skill escapes", func(m *manifest.Manifest) { m.Skill.Path = "../other" }, []string{"skill.path"}},
		{"bad template", func(m *manifest.Manifest) {
			m.Install.GitHubRelease = &manifest.GitHubRelease{Repo: "a/b", Tag: "v{version}", Asset: "x_{goos}.tgz"}
		}, []string{"install.github_release.asset"}},
		{"install needs bin", func(m *manifest.Manifest) { m.Install.Bin = "" }, []string{"install.bin"}},
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
