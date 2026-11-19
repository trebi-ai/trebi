package routing_test

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
)

const discordFile = `mode: off
routes:
  - agent: sling-coder
    gets: every_message
    rooms: ["C1"]
  - job: sling-cmo/seo-writer
    rooms: ["C2"]
    batch: 2m
`

const gmailFile = `mode: on
notes: "Mail about bugs goes to sling-coder."
routes:
  - id: rt_3f9a0c1b2d
    agent: sling-coder
    gets: chosen
    use_for: "bugs and failures"
  - id: rt_4f9a0c1b2d
    job: sling-cs/email-support
    gets: chosen
    allowlist: ["a@b.co"]
rules:
  - sender_ends: "@stripe.com"
    to: sling-cs/email-support
  - sender_is: "spam@x.io"
    to: nobody
unsure: ask
no_fit: drop
sure_at: 0.7
`

func TestParse(t *testing.T) {
	t.Parallel()
	f := routing.Parse("discord", "/ws/routes/discord.yaml", nil)
	if f.Mode != routing.ModeOff || len(f.Routes) != 0 || len(f.Problems) != 0 || f.SureAt != routing.DefaultSureAt || f.Unsure != routing.UnsureAsk || f.NoFit != routing.NoFitDrop {
		t.Fatalf("empty: %+v", f)
	}
	f = routing.Parse("discord", "", []byte(discordFile))
	if len(f.Problems) != 0 || len(f.Routes) != 2 || f.Routes[0].Gets != routing.GetsEveryMessage || f.Routes[1].Gets != routing.GetsEveryMessage {
		t.Fatalf("discord: %+v", f)
	}
	if got := f.Routes[1].TargetName(); got != "sling-cmo/seo-writer" {
		t.Fatalf("target name = %q", got)
	}
	f = routing.Parse("gmail", "", []byte(gmailFile))
	if len(f.Problems) != 0 {
		t.Fatalf("gmail problems: %+v", f.Problems)
	}
	if f.Mode != routing.ModeOn || f.Notes == "" || len(f.Routes) != 2 || len(f.Rules) != 2 || f.Unsure != "ask" || f.NoFit != "drop" || f.SureAt != 0.7 {
		t.Fatalf("gmail: %+v", f)
	}
	if r := f.Routes[0]; r.ID != "rt_3f9a0c1b2d" || r.Agent != "sling-coder" || r.UseFor == "" || r.Gets != routing.GetsChosen {
		t.Fatalf("route 0: %+v", r)
	}
	if r := f.Rules[0]; r.Field != routing.RuleSenderEnds || r.Value != "@stripe.com" || r.To != "sling-cs/email-support" {
		t.Fatalf("rule 0: %+v", r)
	}
	if f.Rules[1].To != routing.RuleNobody {
		t.Fatalf("rule 1: %+v", f.Rules[1])
	}
	f = routing.Parse("x", "", []byte("routes:\n  - agent: sling-cmo\n  - job: sling-coder/platform-health-watcher\n"))
	if f.Routes[0].Agent != "sling-cmo" || f.Routes[1].TargetName() != "sling-coder/platform-health-watcher" || f.Routes[1].Job != "sling-coder/platform-health-watcher" {
		t.Fatalf("prefix: %+v", f.Routes)
	}
	if f.Routes[0].TargetName() != "sling-cmo" || (routing.Route{Assistant: true}).TargetName() != "assistant" {
		t.Fatal("target names")
	}
}

func TestParseProblems(t *testing.T) {
	t.Parallel()
	dup := "routes:\n  - id: rt_aaaaaaaaaa\n    agent: a\n  - id: rt_aaaaaaaaaa\n    agent: b\n"
	cases := []struct {
		name, src, path, msg string
		routes               int
	}{
		{"syntax", "routes: [\n", "", "not valid YAML", 0},
		{"two targets", "routes:\n  - agent: a\n    job: a/b\n", "routes[0]", "one target", 1},
		{"no target", "routes:\n  - gets: off\n", "routes[0]", "needs one target", 1},
		{"bad gets", "routes:\n  - agent: a\n    gets: maybe\n", "routes[0].gets", "gets", 1},
		{"duplicate id", dup, "routes[rt_aaaaaaaaaa].id", "earlier route", 2},
		{"bad id", "routes:\n  - id: x\n    agent: a\n", "routes[0].id", "rt_", 1},
		{"old mode", "mode: choose_one\n", "mode", "on or off", 0},
		{"bad sure_at", "sure_at: 1.5\n", "sure_at", "sure_at", 0},
		{"unknown top key", "extra: 1\n", "extra", "unknown key", 0},
		{"two conditions", "rules:\n  - sender_is: a\n    text_has: b\n    to: x\n", "rules[0]", "one condition", 0},
		{"rule without to", "rules:\n  - sender_is: a\n", "rules[0]", "to key", 0},
		{"assistant chat", "routes:\n  - assistant: true\n    rooms: [r]\n    chat: reply\n", "routes[0].chat", "applies only to a job", 1},
		{"assistant continue", "routes:\n  - assistant: true\n    replies: continue\n", "routes[0].replies", "applies only to a job", 1},
		{"assistant batch", "routes:\n  - assistant: true\n    batch: 5m\n", "routes[0].batch", "job or agent", 1},
		{"agent chat", "routes:\n  - agent: a\n    chat: listen\n", "routes[0].chat", "applies only to a job", 1},
		{"chat without rooms", "routes:\n  - job: a/b\n    chat: reply\n", "routes[0]", `"chat: reply" needs "rooms"`, 1},
		{"debounce", "routes:\n  - agent: a\n    debounce: 5s\n", "routes[0].debounce", "unknown route key", 1},
		{"nobody agent", "routes:\n  - agent: nobody\n", "routes[0].agent", "not a valid agent name", 1},
		{"job form", "routes:\n  - job: solo\n", "routes[0].job", "<agent>/<job>", 1},
		{"not a map", "- a\n", "", "must be a map", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := routing.Parse("c", "", []byte(tc.src))
			if len(f.Problems) == 0 {
				t.Fatalf("no problem: %+v", f)
			}
			p := f.Problems[len(f.Problems)-1]
			if p.Code != routing.CodeInvalidRoute || p.Path != tc.path || !strings.Contains(p.Message, tc.msg) {
				t.Fatalf("problem = %+v, want path %q msg %q", p, tc.path, tc.msg)
			}
			if len(f.Routes) != tc.routes {
				t.Fatalf("routes = %d, want %d", len(f.Routes), tc.routes)
			}
			if tc.routes > 0 && !f.Routes[len(f.Routes)-1].Broken() {
				t.Fatalf("route is not broken: %+v", f.Routes)
			}
		})
	}
}

func TestRouteTrigger(t *testing.T) {
	t.Parallel()
	f := routing.Parse("gmail-2", "", []byte("routes:\n  - id: rt_0123456789\n    agent: a\n    gets: by_hand\n    batch: 2m\n  - agent: b\n    gets: off\n    inputs: {topic: \"{{text}}\"}\n"))
	if len(f.Problems) != 0 {
		t.Fatal(f.Problems)
	}
	tr := f.Routes[0].Trigger("gmail-2")
	if !tr.Manual || tr.Disabled || tr.Event != "gmail-2" || tr.Route != "rt_0123456789" || tr.Family != "event" || tr.BatchMS != 120000 {
		t.Fatalf("trigger 0: %+v", tr)
	}
	tr = f.Routes[1].Trigger("gmail-2")
	if tr.Manual || !tr.Disabled || len(tr.Inputs) != 1 {
		t.Fatalf("trigger 1: %+v", tr)
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()
	declared := &job.Job{Name: "j", Inputs: job.Inputs{{Name: "topic", Type: job.InputString}}}
	targets := map[string]routing.Target{
		"a":         {Kind: "agent", Name: "a", Job: declared},
		"b":         {Kind: "agent", Name: "b"},
		"gone":      {Kind: "agent", Name: "gone", Archived: true},
		"assistant": {Kind: "assistant", Name: "assistant"},
	}
	parse := func(src string) routing.File { return routing.Parse("c", "", []byte(src)) }
	cases := []struct {
		name, src, code, path string
		fixKey                string
	}{
		{"missing target", "routes:\n  - id: rt_aaaaaaaaaa\n    agent: zzz\n", routing.CodeRouteTargetMissing, "routes[rt_aaaaaaaaaa]", "remove_routes"},
		{"missing rule target", "rules:\n  - sender_is: x\n    to: zzz\n", routing.CodeRuleTargetMissing, "rules[0].to", "rules"},
		{"no choices", "mode: on\nroutes:\n  - agent: a\n", routing.CodeNoChoices, "mode", "mode"},
		{"rule not chosen", "mode: on\nroutes:\n  - id: rt_aaaaaaaaaa\n    agent: a\n    gets: chosen\n  - id: rt_bbbbbbbbbb\n    agent: b\n    gets: every_message\nrules:\n  - sender_is: x\n    to: b\n", routing.CodeRuleTargetNotChosen, "rules[0].to", "routes"},
		{"unknown input", "routes:\n  - id: rt_0000000001\n    agent: a\n    inputs: {name: v}\n", job.CodeInputUnknown, "routes[rt_0000000001].inputs.name", "routes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := parse(tc.src)
			if tc.name == "unknown input" {
				f.Routes[0].ID = "rt_0000000001"
			}
			got := routing.Check(f, targets)
			if len(got) != 1 || got[0].Code != tc.code || got[0].Path != tc.path {
				t.Fatalf("problems = %+v", got)
			}
			if got[0].Fix == nil || got[0].Fix.Action != routing.FixAction {
				t.Fatalf("fix = %+v", got[0].Fix)
			}
			if _, ok := got[0].Fix.Input[tc.fixKey]; !ok {
				t.Fatalf("fix input = %+v", got[0].Fix.Input)
			}
		})
	}
	if got := routing.Check(parse("routes:\n  - agent: gone\n  - assistant: true\nrules:\n  - sender_is: x\n    to: nobody\n"), targets); len(got) != 0 {
		t.Fatalf("archived or nobody: %+v", got)
	}
}
