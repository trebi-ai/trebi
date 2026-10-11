package tools

import (
	"encoding/json"
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
