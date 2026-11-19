package routing_test

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/routing"
)

func TestAssignIDs(t *testing.T) {
	t.Parallel()
	src := "# my routes\nmode: off\nroutes:\n  - agent: a   # first\n    gets: off\n  - { job: b/c, rooms: [r1] }\n  - id: rt_aaaaaaaaaa\n    agent: d\n"
	out, changed, err := routing.AssignIDs([]byte(src))
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	f := routing.Parse("c", "", out)
	if len(f.Problems) != 0 || len(f.Routes) != 3 {
		t.Fatalf("parse: %+v", f)
	}
	seen := map[string]bool{}
	for _, r := range f.Routes {
		if !routing.ValidID(r.ID) || seen[r.ID] {
			t.Fatalf("id %q", r.ID)
		}
		seen[r.ID] = true
	}
	if f.Routes[2].ID != "rt_aaaaaaaaaa" {
		t.Fatal("old id changed")
	}
	for _, keep := range []string{"# my routes\n", "agent: a   # first\n    gets: off", "rooms: [r1] }"} {
		if !strings.Contains(string(out), keep) {
			t.Fatalf("lost %q in\n%s", keep, out)
		}
	}
	if !strings.Contains(string(out), "  - id: "+f.Routes[0].ID+"\n    agent: a") {
		t.Fatalf("id is not the first key:\n%s", out)
	}
	again, changed, err := routing.AssignIDs(out)
	if err != nil || changed || string(again) != string(out) {
		t.Fatalf("second call changed=%v err=%v", changed, err)
	}
	if _, _, err := routing.AssignIDs([]byte("routes: [")); err == nil {
		t.Fatal("syntax error was not returned")
	}
}
