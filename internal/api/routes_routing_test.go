package api_test

import (
	"net/http"
	"os"
	"path/filepath"
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

// TestJobGetRoutes: job show lists the routes that name the job, with the
// problems of each. Schedules stay in triggers.
func TestJobGetRoutes(t *testing.T) {
	base, ws, wsPath := routingFixture(t)
	routingtest.Write(t, wsPath, "gmail", routing.File{Routes: []routing.Route{
		{ID: "rt_aaaaaaaaaa", Job: "ops/watch", Gets: routing.GetsByHand, UseFor: "Alerts"},
		{ID: "rt_bbbbbbbbbb", Agent: "cmo", Gets: routing.GetsOff},
	}})
	code, got, _ := doJSON(t, "GET", base+"/v1/jobs/agents%2Fops%2Fwatch?workspace="+ws, nil)
	if code != http.StatusOK {
		t.Fatalf("get = %d %v", code, got)
	}
	routes, _ := got["routes"].([]any)
	if len(routes) != 1 {
		t.Fatalf("routes = %v", got["routes"])
	}
	r := routes[0].(map[string]any)
	if r["connection"] != "gmail" || r["id"] != "rt_aaaaaaaaaa" || r["gets"] != "by_hand" || r["use_for"] != "Alerts" || r["filters"] == nil {
		t.Fatalf("route = %v", r)
	}
	if triggers, _ := got["triggers"].([]any); len(triggers) != 0 {
		t.Fatalf("triggers = %v", triggers)
	}
}

func TestAgentGetRoutes(t *testing.T) {
	base, ws, wsPath := routingFixture(t)
	routingtest.Write(t, wsPath, "gmail", routing.File{Routes: []routing.Route{
		{ID: "rt_aaaaaaaaaa", Agent: "ops", Gets: routing.GetsOff},
		{ID: "rt_bbbbbbbbbb", Job: "ops/watch", Gets: routing.GetsOff},
	}})
	routingtest.Write(t, wsPath, "slack", routing.File{Routes: []routing.Route{{ID: "rt_cccccccccc", Agent: "ops", Gets: routing.GetsOff, UseFor: "Chat"}}})
	code, got, _ := doJSON(t, "GET", base+"/v1/workspaces/"+ws+"/agents/ops", nil)
	routes, _ := got["routes"].([]any)
	if code != http.StatusOK || len(routes) != 2 {
		t.Fatalf("ops = %d %v", code, got["routes"])
	}
	code, got, _ = doJSON(t, "GET", base+"/v1/workspaces/"+ws+"/agents/cmo", nil)
	if routes, ok := got["routes"].([]any); code != http.StatusOK || !ok || len(routes) != 0 {
		t.Fatalf("cmo = %d %v", code, got["routes"])
	}
}

// routingFixture starts the API with a workspace that has the agents ops and
// cmo and the connections gmail and slack.
func TestJobDeleteGuard(t *testing.T) {
	base, ws, wsPath := routingFixture(t)
	path := routingtest.Write(t, wsPath, "gmail", routing.File{Routes: []routing.Route{
		{ID: "rt_aaaaaaaaaa", Job: "ops/watch", Gets: routing.GetsOff},
		{ID: "rt_bbbbbbbbbb", Agent: "cmo", Gets: routing.GetsOff},
	}, Rules: []routing.Rule{{Field: routing.RuleSenderIs, Value: "a@b.c", To: "ops/watch"}}})
	del := base + "/v1/jobs/agents%2Fops%2Fwatch?workspace=" + ws
	// Without confirm the answer lists the routes and rules.
	code, body, _ := doJSON(t, "DELETE", del, nil)
	details, _ := body["error"].(map[string]any)["details"].(map[string]any)
	if code != http.StatusBadRequest || details["confirm_required"] != true ||
		len(details["routes"].([]any)) != 1 || len(details["rules"].([]any)) != 1 {
		t.Fatalf("guard = %d %v", code, body)
	}
	route := details["routes"].([]any)[0].(map[string]any)
	if route["connection"] != "gmail" || route["route"] != "rt_aaaaaaaaaa" || route["target"] != "ops/watch" {
		t.Fatalf("route = %v", route)
	}
	// With confirm the job, its route, and its rule go in one call.
	code, body, _ = doJSON(t, "DELETE", del+"&confirm=1", nil)
	if code != http.StatusOK {
		t.Fatalf("delete = %d %v", code, body)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "ops/watch") || !strings.Contains(string(data), "agent: cmo") {
		t.Fatalf("routes after delete:\n%s", data)
	}
}

func TestAgentDeleteGuard(t *testing.T) {
	base, ws, wsPath := routingFixture(t)
	path := routingtest.Write(t, wsPath, "gmail", routing.File{Routes: []routing.Route{
		{ID: "rt_aaaaaaaaaa", Agent: "ops", Gets: routing.GetsOff},
		{ID: "rt_bbbbbbbbbb", Job: "ops/watch", Gets: routing.GetsOff},
		{ID: "rt_cccccccccc", Agent: "cmo", Gets: routing.GetsOff},
	}})
	del := base + "/v1/workspaces/" + ws + "/agents/ops"
	// Purge lists the routes of the agent and of its jobs.
	code, body, _ := doJSON(t, "DELETE", del+"?purge=1", nil)
	details, _ := body["error"].(map[string]any)["details"].(map[string]any)
	if code != http.StatusBadRequest || len(details["routes"].([]any)) != 2 {
		t.Fatalf("purge guard = %d %v", code, body)
	}
	// A move lists only the routes of the agent itself.
	code, body, _ = doJSON(t, "DELETE", del+"?move_jobs_to=cmo", nil)
	details, _ = body["error"].(map[string]any)["details"].(map[string]any)
	if code != http.StatusBadRequest || len(details["routes"].([]any)) != 1 {
		t.Fatalf("move guard = %d %v", code, body)
	}
	code, body, _ = doJSON(t, "DELETE", del+"?move_jobs_to=cmo&confirm=1", nil)
	if code != http.StatusOK {
		t.Fatalf("delete = %d %v", code, body)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{"job: cmo/watch", "agent: cmo"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %q in\n%s", want, data)
		}
	}
	if strings.Contains(string(data), "agent: ops") || strings.Contains(string(data), "ops/") {
		t.Fatalf("routes after delete:\n%s", data)
	}
}

func TestJobDeleteConfirmRoutes(t *testing.T) {
	base, ws, wsPath := routingFixture(t)
	routingtest.Write(t, wsPath, "gmail", routing.File{Routes: []routing.Route{
		{ID: "rt_aaaaaaaaaa", Job: "ops/watch", Gets: routing.GetsOff},
	}})
	cs := mcpSession(t, base)
	in := map[string]any{"name": "agents/ops/watch", "workspace": ws}
	_, text := mcpCall(t, cs, "job", "delete", in)
	for _, want := range []string{"confirm_required", "rt_aaaaaaaaaa", "also removes 1 route (gmail)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	in["confirm"] = true
	if _, text = mcpCall(t, cs, "job", "delete", in); strings.Contains(text, "confirm_required") {
		t.Fatalf("confirmed delete refused: %s", text)
	}
	if code, _, _ := doJSON(t, "GET", base+"/v1/jobs/agents%2Fops%2Fwatch?workspace="+ws, nil); code != http.StatusNotFound {
		t.Fatalf("job still exists: %d", code)
	}
}

func routingFixture(t *testing.T) (base, wsID, wsPath string) {
	t.Helper()
	_, base, _ = startAPI(t)
	wsPath = t.TempDir()
	writeAgentFile(t, wsPath, "ops")
	writeAgentFile(t, wsPath, "cmo")
	writeTestFile(t, filepath.Join(wsPath, "agents", "ops", "watch", "JOB.md"), "---\nharness: fake\n---\n\nRun.\n")
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
