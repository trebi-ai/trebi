package notify_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/notify"
	"gopkg.in/yaml.v3"
)

type layer struct {
	route   []notify.Step
	set     bool
	contact []string
	owner   string
}

type fakeLayers struct {
	job, agent, ws, global layer
}

func (f fakeLayers) JobLayer(context.Context, string, string) ([]notify.Step, bool, []string, string) {
	return f.job.route, f.job.set, f.job.contact, f.job.owner
}

func (f fakeLayers) AgentLayer(context.Context, string, string) ([]notify.Step, bool, []string) {
	return f.agent.route, f.agent.set, f.agent.contact
}

func (f fakeLayers) WorkspaceLayer(context.Context, string) ([]notify.Step, bool, []string) {
	return f.ws.route, f.ws.set, f.ws.contact
}

func (f fakeLayers) Global() ([]notify.Step, bool, []string) {
	return f.global.route, f.global.set, f.global.contact
}

type fakeChannels map[string]bool

func (f fakeChannels) HasChannel(name string) (bool, bool) {
	ch, ok := f[name]
	return ok, ch
}

func TestResolve(t *testing.T) {
	t.Parallel()
	wa := []notify.Step{{Via: "whatsapp", To: "+1555"}, {Via: "email"}}
	admins := layer{contact: []string{"Root@Example.com"}}
	for _, tc := range []struct {
		name    string
		layers  fakeLayers
		sc      notify.Scope
		emailOn func() bool
		source  notify.Layer
		steps   []notify.ResolvedStep
		contact []string
		csource notify.Layer
	}{
		{
			name: "default route mails the admins", layers: fakeLayers{global: admins},
			sc: notify.Scope{WorkspaceID: "ws"}, emailOn: on, source: notify.LayerDefault,
			steps:   []notify.ResolvedStep{{Step: notify.Step{Via: "email", To: "contact"}, Targets: []string{"root@example.com"}}},
			contact: []string{"root@example.com"}, csource: notify.LayerGlobal,
		},
		{
			name: "job wins and names its owner",
			layers: fakeLayers{
				job:    layer{route: wa, set: true, owner: "cmo"},
				agent:  layer{route: []notify.Step{{Via: "email"}}, set: true, contact: []string{"cmo@example.com", "admins"}},
				global: admins,
			},
			sc: notify.Scope{WorkspaceID: "ws", Job: "deploy"}, emailOn: on, source: notify.LayerJob,
			steps: []notify.ResolvedStep{
				{Step: wa[0], Targets: []string{"+1555"}},
				{Step: wa[1], Targets: []string{"cmo@example.com", "root@example.com"}},
			},
			contact: []string{"cmo@example.com", "root@example.com"}, csource: notify.LayerAgent,
		},
		{
			name:   "an empty list is set and sends nothing",
			layers: fakeLayers{ws: layer{route: []notify.Step{}, set: true}, global: layer{route: wa, set: true}},
			sc:     notify.Scope{WorkspaceID: "ws"}, emailOn: on, source: notify.LayerWorkspace,
			steps: []notify.ResolvedStep{}, contact: []string{}, csource: notify.LayerGlobal,
		},
		{
			name:   "problems",
			layers: fakeLayers{global: layer{route: []notify.Step{{Via: "slack", To: "#ops"}, {Via: "discord", To: "c1"}, {Via: "email", To: "admins"}}, set: true}},
			sc:     notify.Scope{}, emailOn: off, source: notify.LayerGlobal,
			steps: []notify.ResolvedStep{
				{Step: notify.Step{Via: "slack", To: "#ops"}, Targets: []string{"#ops"}, Problem: notify.ProblemNoConn},
				{Step: notify.Step{Via: "discord", To: "c1"}, Targets: []string{"c1"}, Problem: notify.ProblemNoChan},
				{Step: notify.Step{Via: "email", To: "admins"}, Targets: []string{}, Problem: notify.ProblemEmail},
			},
			contact: []string{}, csource: notify.LayerGlobal,
		},
		{
			name:   "no contact",
			layers: fakeLayers{},
			sc:     notify.Scope{WorkspaceID: "ws"}, emailOn: on, source: notify.LayerDefault,
			steps:   []notify.ResolvedStep{{Step: notify.Step{Via: "email", To: "contact"}, Targets: []string{}, Problem: notify.ProblemNoAddr}},
			contact: []string{}, csource: notify.LayerGlobal,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := notify.NewResolver(tc.layers, fakeChannels{"whatsapp": true, "discord": false}, tc.emailOn)
			got := r.Resolve(context.Background(), tc.sc)
			if got.Source != tc.source || got.ContactSource != tc.csource {
				t.Errorf("source %s/%s, want %s/%s", got.Source, got.ContactSource, tc.source, tc.csource)
			}
			if !reflect.DeepEqual(got.Steps, tc.steps) {
				t.Errorf("steps %+v\nwant  %+v", got.Steps, tc.steps)
			}
			if !reflect.DeepEqual(got.Contact, tc.contact) {
				t.Errorf("contact %v, want %v", got.Contact, tc.contact)
			}
		})
	}
}

func TestParseSteps(t *testing.T) {
	t.Parallel()
	node := func(src string) *yaml.Node {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Content[0]
	}
	steps, err := notify.ParseSteps(node("- {via: whatsapp, to: '+1555'}\n- via: email\n"))
	if err != nil || len(steps) != 2 || steps[0].To != "+1555" || steps[1].Via != "email" {
		t.Fatalf("steps %+v %v", steps, err)
	}
	if steps, err := notify.ParseSteps(node("[]")); err != nil || steps == nil || len(steps) != 0 {
		t.Fatalf("empty list %v %v", steps, err)
	}
	for src, want := range map[string]string{
		"[email, whatsapp]":                   "the channel list is removed",
		"- {to: a@b.co}":                      "via is required",
		"- {via: Bad_Name, to: x}":            "want email or a connector name",
		"- {via: whatsapp}":                   "to is required",
		"- {via: email, to: nobody}":          "want an address",
		"- {via: email, cc: a@b.co}":          "unknown key",
		strings.Repeat("- {via: email}\n", 9): "at most 8",
	} {
		if _, err := notify.ParseSteps(node(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want %q", src, err, want)
		}
	}
}

func TestParseContact(t *testing.T) {
	t.Parallel()
	if got, err := notify.ParseContact("Ops@Example.com"); err != nil || !reflect.DeepEqual(got, []string{"ops@example.com"}) {
		t.Fatalf("string %v %v", got, err)
	}
	if got, err := notify.ParseContact([]any{"a@b.co", "Admins"}); err != nil || !reflect.DeepEqual(got, []string{"a@b.co", "admins"}) {
		t.Fatalf("list %v %v", got, err)
	}
	if _, err := notify.ParseContact([]string{"nobody"}); err == nil {
		t.Fatal("bad address must fail")
	}
}

func on() bool { return true }

func off() bool { return false }
