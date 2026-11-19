package tools

import (
	"encoding/json"
	"testing"
)

func TestConnectorRoutingActions(t *testing.T) {
	t.Parallel()
	cat := NewCatalog()["connector"]
	list, err := cat["routing_list"].Build(Input{"workspace": "sling"})
	if err != nil || list.Method != "GET" || list.Path != "/v1/workspaces/sling/routing" {
		t.Fatalf("list = %+v %v", list, err)
	}
	get, err := cat["routing_get"].Build(Input{"workspace": "sling", "connector": "gmail-2"})
	if err != nil || get.Path != "/v1/workspaces/sling/connectors/gmail-2/routing" {
		t.Fatalf("get = %+v %v", get, err)
	}
	set, err := cat["routing_set"].Build(Input{
		"workspace": "sling", "connector": "gmail-2", "if_match": `"h"`, "mode": "on", "sure_at": 0.8,
		"routes":        []any{map[string]any{"id": "rt_a", "gets": "chosen"}},
		"remove_routes": []any{"rt_b"}, "rules": []any{},
	})
	if err != nil || set.Method != "PATCH" || set.IfMatch != `"h"` {
		t.Fatalf("set = %+v %v", set, err)
	}
	raw, _ := json.Marshal(set.Body)
	if want := `{"mode":"on","remove_routes":["rt_b"],"routes":[{"gets":"chosen","id":"rt_a"}],"rules":[],"sure_at":0.8}`; string(raw) != want {
		t.Fatalf("body = %s, want %s", raw, want)
	}
	for name, in := range map[string]Input{
		"no workspace": {"connector": "gmail"},
		"bad routes":   {"workspace": "w", "connector": "gmail", "routes": []any{"x"}},
		"bad sure_at":  {"workspace": "w", "connector": "gmail", "sure_at": "x"},
	} {
		if _, err := cat["routing_set"].Build(in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	empty, err := cat["routing_set"].Build(Input{"workspace": "w", "connector": "gmail"})
	if err != nil || len(empty.Body.(map[string]any)) != 0 {
		t.Fatalf("empty patch = %+v %v", empty, err)
	}
}
