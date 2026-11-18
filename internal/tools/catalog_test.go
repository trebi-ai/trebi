package tools

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestCatalogSamplesBuild(t *testing.T) {
	t.Parallel()
	for tool, actions := range NewCatalog() {
		for name, entry := range actions {
			samples := append([]Input{entry.Sample}, entry.Samples...)
			for i, sample := range samples {
				if _, err := entry.Build(sample); err != nil {
					t.Errorf("%s.%s sample %d: build: %v", tool, name, i, err)
				}
			}
		}
	}
}

func TestBusPollTransform(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		body       string
		wantEvents int
		wantCursor int64
	}{
		{"empty dump", "", 0, 0},
		{"keepalive only", ": ping\n\n", 0, 0},
		{"events in order", "data: {\"seq\":5,\"topic\":\"job.created\"}\n\ndata: {\"seq\":7,\"topic\":\"job.updated\"}\n\n", 2, 7},
		{"cursor takes the max seq", "data: {\"seq\":9}\n\ndata: {\"seq\":2}\n\n", 2, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := busPollTransform(nil, 200, []byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			var res struct {
				Events     []map[string]any `json:"events"`
				NextCursor int64            `json:"next_cursor"`
			}
			if err := json.Unmarshal(got, &res); err != nil {
				t.Fatalf("decode %s: %v", got, err)
			}
			if len(res.Events) != c.wantEvents || res.NextCursor != c.wantCursor {
				t.Fatalf("got %d events, cursor %d; want %d events, cursor %d",
					len(res.Events), res.NextCursor, c.wantEvents, c.wantCursor)
			}
		})
	}
}

func TestConnectorSetPatch(t *testing.T) {
	t.Parallel()
	set := NewCatalog()["connector"]["set"]
	for _, c := range []struct {
		in   Input
		want string
	}{
		{Input{"connector": "x", "version": ""}, `{"version":""}`},
		{Input{"connector": "x", "patch": nil}, `{"patch":null}`},
		{Input{"connector": "x", "manifest": map[string]any{"name": "x"}}, `{"manifest":{"name":"x"}}`},
		{Input{"connector": "x", "inputs": map[string]any{"A": "1"}}, `{"inputs":{"A":"1"}}`},
		{Input{"connector": "x", "slug": "x-work", "title": "Work"}, `{"slug":"x-work","title":"Work"}`},
		{Input{"connector": "x"}, `{}`},
	} {
		req, err := set.Build(c.in)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(req.Body)
		if string(b) != c.want {
			t.Fatalf("%v: body %s, want %s", c.in, b, c.want)
		}
	}
}

// TestConnectorActionsUseConnectionRefs proves the connector actions take a
// slug or id in "connector", install takes slug and title, and replace is
// gone.
func TestConnectorActionsUseConnectionRefs(t *testing.T) {
	t.Parallel()
	cat := NewCatalog()["connector"]
	if _, ok := cat["replace"]; ok {
		t.Fatal("replace action must be gone; set covers every field")
	}
	req, err := cat["restart"].Build(Input{"connector": "con_01k6abcdefghjkmnpqrstvwxyz"})
	if err != nil || req.Path != "/v1/connectors/con_01k6abcdefghjkmnpqrstvwxyz/restart" {
		t.Fatalf("restart by id: %+v %v", req, err)
	}
	if _, err := cat["restart"].Build(Input{"name": "mail"}); err == nil {
		t.Fatal("the old name key must not name a connection")
	}
	req, err = cat["install"].Build(Input{"registry": "trebi", "name": "whatsapp", "slug": "wa-work", "title": "Work phone"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(req.Body)
	if !strings.Contains(string(b), `"slug":"wa-work"`) || !strings.Contains(string(b), `"title":"Work phone"`) || strings.Contains(string(b), `"as"`) {
		t.Fatalf("install body %s", b)
	}
	if _, err := cat["create"].Build(Input{}); err == nil {
		t.Fatal("create without a connection must fail")
	}
	link := "https://github.com/flarco/cli-tools/tree/main/vitally-cli"
	req, err = cat["check_link"].Build(Input{"url": link})
	if err != nil || req.Method != "POST" || req.Path != "/v1/connectors/sources/check" {
		t.Fatalf("check_link: %+v %v", req, err)
	}
	if _, err := cat["check_link"].Build(Input{}); err == nil {
		t.Fatal("check_link without url must fail")
	}
	req, err = cat["install"].Build(Input{"source": map[string]any{"url": link, "check_id": "chk_1"}, "force": true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(req.Body)
	if string(b) != `{"force":true,"source":{"url":"`+link+`","check_id":"chk_1"}}` {
		t.Fatalf("install from a link body %s", b)
	}
	for _, in := range []Input{{}, {"name": "whatsapp", "source": map[string]any{"url": link}}} {
		if _, err := cat["install"].Build(in); err == nil {
			t.Fatalf("install %v must need one of name or source", in)
		}
	}
}

// TestJobFolderFields proves job.create, job.validate_text, and job.patch
// send folder, and agent.patch does not take it.
func TestJobFolderFields(t *testing.T) {
	t.Parallel()
	cat := NewCatalog()
	for _, action := range []string{"create", "validate_text"} {
		req, err := cat["job"][action].Build(Input{"name": "a", "workspace": "w", "prompt": "Go.", "agent": "cmo", "folder": "SEO"})
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := json.Marshal(req.Body); !strings.Contains(string(b), `"folder":"SEO"`) {
			t.Fatalf("%s body %s", action, b)
		}
	}
	req, err := cat["job"]["patch"].Build(Input{"name": "a", "folder": ""})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(req.Body); string(b) != `{"folder":""}` {
		t.Fatalf("patch body %s", b)
	}
	if slices.Contains(agentPatchFields, "folder") {
		t.Fatal("agent.patch must not take folder")
	}
}

// TestArchiveActionsBuild proves the archive actions send the method, the
// path, the If-Match header, and the archived list filter.
func TestArchiveActionsBuild(t *testing.T) {
	t.Parallel()
	cat := NewCatalog()
	cases := []struct {
		tool, action string
		in           Input
		path         string
		ifMatch      string
	}{
		{"job", "archive", Input{"name": "nightly", "workspace": "ws1", "if_match": "h1"}, "/v1/jobs/nightly/archive?workspace=ws1", "h1"},
		{"job", "unarchive", Input{"name": "nightly"}, "/v1/jobs/nightly/unarchive", ""},
		{"agent", "archive", Input{"workspace": "ws1", "name": "cmo", "if_match": "h2"}, "/v1/workspaces/ws1/agents/cmo/archive", "h2"},
		{"agent", "unarchive", Input{"workspace": "ws1", "name": "cmo"}, "/v1/workspaces/ws1/agents/cmo/unarchive", ""},
		{"job", "list", Input{"archived": "all"}, "/v1/jobs?archived=all", ""},
		{"agent", "list", Input{"archived": "true"}, "/v1/agents?archived=true", ""},
		{"skill", "archive", Input{"name": "deploy", "scope": "agent", "owner": "ops", "if_match": "h3"}, "/v1/skills/deploy/archive?owner=ops&scope=agent", "h3"},
		{"skill", "unarchive", Input{"name": "deploy"}, "/v1/skills/deploy/unarchive", ""},
		{"skill", "list", Input{"archived": "all"}, "/v1/skills?archived=all", ""},
	}
	for _, c := range cases {
		req, err := cat[c.tool][c.action].Build(c.in)
		if err != nil {
			t.Fatalf("%s.%s: %v", c.tool, c.action, err)
		}
		if req.Path != c.path || req.IfMatch != c.ifMatch {
			t.Errorf("%s.%s: path %q if-match %q, want %q %q", c.tool, c.action, req.Path, req.IfMatch, c.path, c.ifMatch)
		}
		if c.action != "list" && req.Method != "POST" {
			t.Errorf("%s.%s: method %s", c.tool, c.action, req.Method)
		}
	}
}

// TestJobOwnerAgent proves job.create needs an agent, job.move refuses an
// empty agent, and agent.delete sends move_jobs_to.
func TestJobOwnerAgent(t *testing.T) {
	t.Parallel()
	cat := NewCatalog()
	if _, err := cat["job"]["create"].Build(Input{"name": "digest", "workspace": "w", "prompt": "Go."}); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("create without agent: err=%v", err)
	}
	for _, in := range []Input{
		{"name": "digest", "workspace": "w", "prompt": "Go.", "agent": "cmo"},
		{"name": "agents/cmo/digest", "workspace": "w", "prompt": "Go."},
	} {
		if _, err := cat["job"]["create"].Build(in); err != nil {
			t.Fatalf("create %v: %v", in, err)
		}
	}
	if _, err := cat["job"]["move"].Build(Input{"workspace": "w", "name": "agents/cmo/digest", "agent": ""}); err == nil {
		t.Fatal(`move with agent "" must fail`)
	}
	req, err := cat["agent"]["delete"].Build(Input{"workspace": "w", "name": "cmo", "move_jobs_to": "ops", "confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(req.Path)
	if err != nil || req.Method != "DELETE" || u.Query().Get("move_jobs_to") != "ops" || u.Query().Get("confirm") == "" {
		t.Fatalf("agent.delete: %+v %v", req, err)
	}
}

func TestEmailShowTransform(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		dir, key, absent string
	}{
		{"in", "untrusted_text", "text"},
		{"out", "text", "untrusted_text"},
	} {
		body := `{"id":"01A","direction":"` + tc.dir + `","content":{"text":"hi","html":"<p>hi</p>"}}`
		out, err := emailShowTransform(nil, 200, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Content map[string]any `json:"content"`
		}
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		if m.Content[tc.key] != "hi" || m.Content[tc.absent] != nil || m.Content["html"] != nil {
			t.Errorf("%s: content = %v", tc.dir, m.Content)
		}
	}
}

// TestHarnessEnvRefusesSecretValue covers the rule that a secret value never
// passes through the chat.
func TestHarnessEnvRefusesSecretValue(t *testing.T) {
	t.Parallel()
	harness := NewCatalog()["harness"]
	secret := map[string]any{"ANTHROPIC_AUTH_TOKEN": map[string]any{"value": "tok", "secret": true}}
	for action, in := range map[string]Input{
		"patch":  {"name": "claude-zai", "env": secret},
		"create": {"name": "claude-zai", "base": "claude", "env": secret},
	} {
		_, err := harness[action].Build(in)
		if err == nil || !strings.Contains(err.Error(), "/settings/harnesses?") {
			t.Fatalf("%s: err=%v", action, err)
		}
	}
	ok := Input{"name": "claude-zai", "env": map[string]any{"ANTHROPIC_AUTH_TOKEN": map[string]any{"secret": true}, "ANTHROPIC_MODEL": map[string]any{"value": "glm-5"}}}
	if _, err := harness["patch"].Build(ok); err != nil {
		t.Fatalf("patch with no secret value: %v", err)
	}
}
