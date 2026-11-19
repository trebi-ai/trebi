package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
)

func TestRoutingGet(t *testing.T) {
	base, ws, wsPath := routingFixture(t)
	routingtest.Write(t, wsPath, "gmail", routing.File{Mode: routing.ModeOn, Routes: []routing.Route{{Agent: "ops", Gets: routing.GetsChosen}}})
	url := base + "/v1/workspaces/" + ws + "/connectors/"
	code, view, h := doJSON(t, "GET", url+"gmail/routing", nil)
	if code != http.StatusOK || view["exists"] != true || view["mode"] != "on" || !strings.HasSuffix(view["path"].(string), "routes/gmail.yaml") {
		t.Fatalf("by slug = %d %v", code, view)
	}
	if h.Get("ETag") != `"`+view["hash"].(string)+`"` {
		t.Fatalf("etag = %q hash = %v", h.Get("ETag"), view["hash"])
	}
	conn := view["connection"].(map[string]any)
	if code, byID, _ := doJSON(t, "GET", url+conn["id"].(string)+"/routing", nil); code != http.StatusOK || byID["hash"] != view["hash"] {
		t.Fatalf("by id = %d %v", code, byID)
	}
	target := view["routes"].([]any)[0].(map[string]any)["target"].(map[string]any)
	if target["kind"] != "agent" || target["exists"] != true || target["index"] != "agents/ops" {
		t.Fatalf("target = %v", target)
	}
	models := view["models"].([]any)
	if len(models) == 0 || models[0].(map[string]any)["label"] != "Default (assistant model)" {
		t.Fatalf("models = %v", models)
	}
	if code, _, _ := doJSON(t, "GET", url+"nope/routing", nil); code != http.StatusNotFound {
		t.Fatalf("unknown connection = %d", code)
	}
	code, empty, _ := doJSON(t, "GET", url+"slack/routing", nil)
	if code != http.StatusOK || empty["exists"] != false || empty["mode"] != "off" || len(empty["routes"].([]any)) != 0 {
		t.Fatalf("missing file = %d %v", code, empty)
	}
}

func TestRoutingSet(t *testing.T) {
	base, ws, _ := routingFixture(t)
	url := base + "/v1/workspaces/" + ws + "/connectors/gmail/routing"
	code, view, _ := doJSON(t, "PATCH", url, map[string]any{"routes": []any{map[string]any{"agent": "ops"}}})
	if code != http.StatusOK || view["exists"] != true {
		t.Fatalf("add = %d %v", code, view)
	}
	ids := routeIDs(view)
	if len(ids) != 1 || view["routes"].([]any)[0].(map[string]any)["gets"] != "off" {
		t.Fatalf("new route in a file with mode off = %v", view["routes"])
	}
	code, view, _ = doJSON(t, "PATCH", url, map[string]any{"mode": "on", "routes": []any{map[string]any{"id": ids[0], "gets": "chosen", "use_for": "Billing"}}})
	if code != http.StatusOK || view["mode"] != "on" || view["routes"].([]any)[0].(map[string]any)["gets"] != "chosen" {
		t.Fatalf("merge = %d %v", code, view)
	}
	// A new route in a file with mode on starts as chosen.
	code, view, _ = doJSON(t, "PATCH", url, map[string]any{"routes": []any{map[string]any{"agent": "cmo"}}})
	if code != http.StatusOK || len(routeIDs(view)) != 2 || view["routes"].([]any)[1].(map[string]any)["gets"] != "chosen" {
		t.Fatalf("add on = %d %v", code, view)
	}
	// A missing target gives 400 with a fix that names the action.
	code, body, _ := doJSON(t, "PATCH", url, map[string]any{"routes": []any{map[string]any{"agent": "opz", "gets": "chosen"}}})
	if code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("missing target = %d %v", code, body)
	}
	details := body["error"].(map[string]any)["details"].(map[string]any)
	prob := details["problems"].([]any)[0].(map[string]any)
	fix := prob["fix"].(map[string]any)
	if prob["code"] != "route_target_missing" || fix["action"] != "connector.routing_set" || fix["label"] == "" ||
		fix["input"].(map[string]any)["connector"] != "gmail" || !strings.Contains(prob["message"].(string), "agent opz does not exist") {
		t.Fatalf("problem = %v", prob)
	}
	// A stale If-Match gives 412.
	code, body, _ = doJSONHeaders(t, "PATCH", url, map[string]any{"notes": "x"}, map[string]string{"If-Match": `"stale"`})
	if code != http.StatusPreconditionFailed || errCode(body) != "precondition_failed" {
		t.Fatalf("stale = %d %v", code, body)
	}
	// The current hash passes.
	_, cur, _ := doJSON(t, "GET", url, nil)
	code, _, _ = doJSONHeaders(t, "PATCH", url, map[string]any{"notes": "x"}, map[string]string{"If-Match": `"` + cur["hash"].(string) + `"`})
	if code != http.StatusOK {
		t.Fatalf("fresh if-match = %d", code)
	}
	code, view, _ = doJSON(t, "PATCH", url, map[string]any{"mode": "off", "remove_routes": routeIDs(view)})
	if code != http.StatusOK || len(routeIDs(view)) != 0 {
		t.Fatalf("remove = %d %v", code, view)
	}
}

func TestRoutingList(t *testing.T) {
	base, ws, wsPath := routingFixture(t)
	routingtest.Write(t, wsPath, "gmail", routing.File{Routes: []routing.Route{{Agent: "ops", Gets: routing.GetsOff}}})
	code, body, _ := doJSON(t, "GET", base+"/v1/workspaces/"+ws+"/routing", nil)
	list, _ := body["routing"].([]any)
	if code != http.StatusOK || len(list) != 2 {
		t.Fatalf("list = %d %v", code, body)
	}
	exists := map[string]any{}
	for _, v := range list {
		m := v.(map[string]any)
		exists[m["connection"].(map[string]any)["slug"].(string)] = m["exists"]
	}
	if exists["gmail"] != true || exists["slack"] != false {
		t.Fatalf("exists = %v", exists)
	}
	if code, _, _ := doJSON(t, "GET", base+"/v1/workspaces/ws_none/routing", nil); code != http.StatusNotFound {
		t.Fatalf("unknown workspace = %d", code)
	}
}

// routingFixture starts the API with a workspace that has the agents ops and
// cmo and the connections gmail and slack.
func routingFixture(t *testing.T) (base, wsID, wsPath string) {
	t.Helper()
	_, base, _ = startAPI(t)
	wsPath = t.TempDir()
	writeAgentFile(t, wsPath, "ops")
	writeAgentFile(t, wsPath, "cmo")
	for _, name := range []string{"gmail", "slack"} {
		def := inlineEntry(name, map[string]any{"actions": map[string]any{"mcp": map[string]any{"command": "npx"}}})
		if code, body, _ := doJSON(t, "POST", base+"/v1/connectors", def); code != http.StatusCreated {
			t.Fatalf("create %s = %d %v", name, code, body)
		}
	}
	return base, addWorkspace(t, base, wsPath, "rt"), wsPath
}

func routeIDs(view map[string]any) []string {
	var out []string
	for _, r := range view["routes"].([]any) {
		out = append(out, r.(map[string]any)["id"].(string))
	}
	return out
}
