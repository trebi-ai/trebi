package routing_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/routing"
)

func TestMarshalRoundTrip(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{"discord": discordFile, "gmail": gmailFile} {
		f := routing.Parse(name, "", []byte(src))
		out, err := routing.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		g := routing.Parse(name, "", out)
		if len(g.Problems) != 0 {
			t.Fatalf("%s problems: %+v\n%s", name, g.Problems, out)
		}
		strip := func(f routing.File) routing.File {
			f.Hash, f.Problems = "", nil
			for i := range f.Routes {
				f.Routes[i].Line = 0
			}
			for i := range f.Rules {
				f.Rules[i].Line = 0
			}
			return f
		}
		if !reflect.DeepEqual(strip(f), strip(g)) {
			t.Fatalf("%s round trip:\n%+v\n%+v\n%s", name, strip(f), strip(g), out)
		}
	}
}

func TestMarshalOrder(t *testing.T) {
	t.Parallel()
	f := routing.Parse("c", "", []byte(gmailFile))
	out, _ := routing.Marshal(f)
	text := string(out)
	last := -1
	for _, key := range []string{"mode:", "notes:", "routes:", "rules:"} {
		i := strings.Index(text, key)
		if i <= last {
			t.Fatalf("%s out of order:\n%s", key, text)
		}
		last = i
	}
	if strings.Contains(text, "sure_at") || strings.Contains(text, "unsure") {
		t.Fatalf("default keys written:\n%s", text)
	}
}

func TestEdit(t *testing.T) {
	t.Parallel()
	on, off := routing.ModeOn, routing.ModeOff
	src := "# keep\nmode: off\nroutes:\n  - id: rt_aaaaaaaaaa   # first\n    agent: a\n    gets: every_message\n    rooms: [r1]\n"
	// A new route in a mode-off file gets off, and a comment stays.
	out, err := routing.Edit([]byte(src), routing.Patch{Routes: []map[string]any{{"job": "b/c", "rooms": []string{"x"}}}})
	if err != nil {
		t.Fatal(err)
	}
	f := routing.Parse("c", "", out)
	if len(f.Routes) != 2 || f.Routes[1].Gets != routing.GetsOff || !routing.ValidID(f.Routes[1].ID) || f.Routes[1].Job != "b/c" {
		t.Fatalf("new route: %+v\n%s", f.Routes, out)
	}
	if !strings.Contains(string(out), "# keep") || !strings.Contains(string(out), "# first") {
		t.Fatalf("comments lost:\n%s", out)
	}
	// Mode on in the same patch: a new route gets chosen. A merge sets and removes keys.
	out, err = routing.Edit([]byte(src), routing.Patch{Mode: &on, Routes: []map[string]any{
		{"id": "rt_aaaaaaaaaa", "gets": "chosen", "rooms": nil, "use_for": "bugs"},
		{"agent": "z"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f = routing.Parse("c", "", out)
	if f.Mode != routing.ModeOn || f.Routes[0].Gets != routing.GetsChosen || len(f.Routes[0].Filter.Rooms) != 0 || f.Routes[0].UseFor != "bugs" || f.Routes[1].Gets != routing.GetsChosen {
		t.Fatalf("merge: %+v\n%s", f, out)
	}
	// Retarget, remove, rules, and settings.
	rules := []routing.Rule{{Field: routing.RuleSenderIs, Value: "a@b.co", To: routing.RuleNobody}}
	sure := 0.9
	out, err = routing.Edit(out, routing.Patch{Mode: &off, SureAt: &sure, Rules: &rules, RemoveRoutes: []string{f.Routes[1].ID},
		Routes: []map[string]any{{"id": "rt_aaaaaaaaaa", "job": "b/d"}}})
	if err != nil {
		t.Fatal(err)
	}
	f = routing.Parse("c", "", out)
	if len(f.Routes) != 1 || f.Routes[0].Job != "b/d" || f.Routes[0].Agent != "" || len(f.Rules) != 1 || f.SureAt != 0.9 || f.Mode != routing.ModeOff {
		t.Fatalf("second edit: %+v\n%s", f, out)
	}
	// An empty file gets keys in canonical order.
	out, err = routing.Edit(nil, routing.Patch{Routes: []map[string]any{{"assistant": true}}, Mode: &on})
	if err != nil || strings.Index(string(out), "mode") > strings.Index(string(out), "routes") {
		t.Fatalf("empty file: %v\n%s", err, out)
	}
}

func TestEditValidation(t *testing.T) {
	t.Parallel()
	bad := "nope"
	var ve *routing.ValidationError
	if _, err := routing.Edit(nil, routing.Patch{Mode: &bad}); !errors.As(err, &ve) || ve.Problems[0].Path != "mode" {
		t.Fatalf("bad mode: %v", err)
	}
	if _, err := routing.Edit(nil, routing.Patch{Routes: []map[string]any{{"id": "rt_aaaaaaaaaa", "gets": "off"}}}); !errors.As(err, &ve) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := routing.Edit(nil, routing.Patch{Routes: []map[string]any{{"agent": "a", "gets": "maybe"}}}); !errors.As(err, &ve) {
		t.Fatalf("bad gets: %v", err)
	}
}

func TestRenameTarget(t *testing.T) {
	t.Parallel()
	src := "# keep\nroutes:\n  - id: rt_aaaaaaaaaa\n    agent: ops\n  - id: rt_bbbbbbbbbb\n    job: ops/digest\n  - id: rt_cccccccccc\n    agent: cmo\nrules:\n  - sender_is: x\n    to: ops/digest\n"
	out, changed, err := routing.RenameTarget([]byte(src), "ops", "ops2")
	if err != nil || !changed {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}
	for _, want := range []string{"# keep", "agent: ops2", "job: ops2/digest", "to: ops2/digest", "agent: cmo"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if _, changed, _ := routing.RenameTarget([]byte(src), "none", "x"); changed {
		t.Fatal("a rename of an unused name changed the file")
	}
}

func TestRefsTo(t *testing.T) {
	t.Parallel()
	f := routing.Parse("gmail", "", []byte("routes:\n  - id: rt_aaaaaaaaaa\n    agent: ops\n  - id: rt_bbbbbbbbbb\n    job: ops/digest\n  - id: rt_cccccccccc\n    agent: cmo\nrules:\n  - sender_is: x\n    to: ops/digest\n  - sender_is: y\n    to: cmo\n"))
	refs := f.RefsTo([]string{"ops"})
	if len(refs.Routes) != 2 || len(refs.Rules) != 1 || refs.Rules[0].Index != 0 {
		t.Fatalf("agent refs = %+v", refs)
	}
	if exact := refs.Exact("ops"); len(exact.Routes) != 1 || len(exact.Rules) != 0 {
		t.Fatalf("exact = %+v", exact)
	}
	if refs := f.RefsTo([]string{"ops/digest"}); len(refs.Routes) != 1 || len(refs.Rules) != 1 {
		t.Fatalf("job refs = %+v", refs)
	}
	if !f.RefsTo([]string{"none"}).Empty() {
		t.Fatal("refs of an unused name are not empty")
	}
	if got := refs.Describe("ops"); !strings.Contains(got, "2 routes") || !strings.Contains(got, "1 rule") {
		t.Fatalf("describe = %q", got)
	}
}

func TestFixCLI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		fix  routing.Fix
		want string
	}{
		{routing.Fix{Input: map[string]any{"connector": "gmail", "remove_routes": []string{"rt_a", "rt_b"}}}, "trebi connector routing remove gmail rt_a rt_b --yes --workspace W"},
		{routing.Fix{Input: map[string]any{"connector": "gmail", "mode": "off"}}, "trebi connector routing set gmail --mode off --workspace W"},
		{routing.Fix{Input: map[string]any{"connector": "gmail", "routes": []map[string]any{{"id": "rt_a", "gets": "chosen"}}}}, "trebi connector routing set gmail --gets rt_a=chosen --workspace W"},
	}
	for _, tc := range cases {
		if got := tc.fix.CLI("W"); got != tc.want {
			t.Errorf("CLI = %q, want %q", got, tc.want)
		}
	}
	if got := (routing.Fix{Input: map[string]any{"connector": "gmail", "rules": []map[string]string{}}}).CLI("W"); !strings.Contains(got, "--patch") {
		t.Errorf("fallback = %q", got)
	}
}
