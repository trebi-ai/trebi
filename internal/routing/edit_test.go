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
