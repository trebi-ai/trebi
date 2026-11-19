package api_test

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/tools"
)

const routeDecisionID = "dec_route1"

func TestDecisionListWrong(t *testing.T) {
	base, wsID, evID, _, _ := routeViewFixture(t)
	if n := len(decisionItems(t, base, "feedback=disagree")); n != 0 {
		t.Fatalf("before the reroute: %d wrong", n)
	}
	if code, _, _ := doJSON(t, "POST", base+"/v1/events/"+evID+"/reroute", map[string]any{"workspace": wsID, "to": "ops/reply"}); code != http.StatusOK {
		t.Fatalf("reroute = %d", code)
	}
	if n := len(decisionItems(t, base, "feedback=disagree&scope=email")); n != 1 {
		t.Fatalf("wrong = %d, want 1", n)
	}
	if n := len(decisionItems(t, base, "feedback=agree")); n != 0 {
		t.Fatalf("agree = %d, want 0", n)
	}
	if n := len(decisionItems(t, base, "scope=other")); n != 0 {
		t.Fatalf("other scope = %d, want 0", n)
	}
	if code, _, _ := doJSON(t, "GET", base+"/v1/decisions?feedback=maybe", nil); code != http.StatusBadRequest {
		t.Fatalf("bad feedback = %d", code)
	}
}

func TestDecisionListWaiting(t *testing.T) {
	base, wsID, evID, _, _ := routeViewFixture(t)
	items := decisionItems(t, base, "waiting=1")
	if len(items) != 1 || items[0].(map[string]any)["id"] != routeDecisionID {
		t.Fatalf("waiting = %v", items)
	}
	if code, _, _ := doJSON(t, "POST", base+"/v1/events/"+evID+"/reroute", map[string]any{"workspace": wsID, "to": "ops"}); code != http.StatusOK {
		t.Fatalf("reroute = %d", code)
	}
	if n := len(decisionItems(t, base, "waiting=1")); n != 0 {
		t.Fatalf("after the reroute: %d waiting, want none (not all)", n)
	}
	if n := len(decisionItems(t, base, "")); n != 1 {
		t.Fatalf("all = %d, want 1", n)
	}
}

func TestDecisionShowRouteBlock(t *testing.T) {
	base, wsID, evID, inboxID, _ := routeViewFixture(t)
	code, d, _ := doJSON(t, "GET", base+"/v1/decisions/"+routeDecisionID, nil)
	rb, _ := d["route"].(map[string]any)
	if code != http.StatusOK || rb == nil {
		t.Fatalf("show = %d %v", code, d)
	}
	choices, _ := rb["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("choices = %v", choices)
	}
	sum := rb["nobody_share"].(float64)
	for _, c := range choices {
		sum += c.(map[string]any)["share"].(float64)
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("shares add up to %v", sum)
	}
	c0 := choices[0].(map[string]any)
	if c0["route_id"] != rerouteAgentRoute || c0["target"] != "ops" || c0["target_kind"] != "agent" {
		t.Fatalf("choice = %v", c0)
	}
	pick, _ := rb["pick"].(map[string]any)
	if pick["method"] != "ask" || pick["best_route_id"] != rerouteAgentRoute {
		t.Fatalf("pick = %v", pick)
	}
	ev, _ := rb["event"].(map[string]any)
	if rb["connection"] != "email" || ev["id"] != evID || ev["sender"] != "a@b.co" || ev["text"] != "Can we meet?" ||
		rb["waiting"] != true || rb["inbox_id"] != inboxID || rb["sure_at"] != 0.7 {
		t.Fatalf("block = %v", rb)
	}
	if _, ok := rb["chosen_by_you"]; ok {
		t.Fatalf("chosen_by_you before a choice: %v", rb)
	}
	if code, _, _ := doJSON(t, "POST", base+"/v1/events/"+evID+"/reroute", map[string]any{"workspace": wsID, "to": "ops/reply"}); code != http.StatusOK {
		t.Fatalf("reroute = %d", code)
	}
	_, d, _ = doJSON(t, "GET", base+"/v1/decisions/"+routeDecisionID, nil)
	rb, _ = d["route"].(map[string]any)
	you, _ := rb["chosen_by_you"].(map[string]any)
	if you["route_id"] != rerouteJobRoute || you["wrong"] != true || rb["waiting"] != false {
		t.Fatalf("after the reroute: %v", rb)
	}
}

func TestDecisionStatsCost(t *testing.T) {
	base, _, _, _, _ := routeViewFixture(t)
	code, st, _ := doJSON(t, "GET", base+"/v1/decisions/stats?scope=email", nil)
	if code != http.StatusOK || st["total"] != float64(1) || st["cost_usd"] != 0.25 {
		t.Fatalf("stats = %d %v", code, st)
	}
	_, st, _ = doJSON(t, "GET", base+"/v1/decisions/stats?connection=other", nil)
	if st["total"] != float64(0) {
		t.Fatalf("other scope = %v", st)
	}
}

func TestEventRoutingBlock(t *testing.T) {
	base, wsID, evID, inboxID, _ := routeViewFixture(t)
	routingOf := func() map[string]any {
		t.Helper()
		code, ev, _ := doJSON(t, "GET", base+"/v1/events/"+evID, nil)
		list, _ := ev["routing"].([]any)
		if code != http.StatusOK || len(list) != 1 {
			t.Fatalf("event = %d %v", code, ev)
		}
		return list[0].(map[string]any)
	}
	r := routingOf()
	if r["status"] != "waiting" || r["method"] != "router" || r["decision_id"] != routeDecisionID || r["inbox_id"] != inboxID || r["wrong"] != false {
		t.Fatalf("waiting routing = %v", r)
	}
	if code, _, _ := doJSON(t, "POST", base+"/v1/events/"+evID+"/reroute", map[string]any{"workspace": wsID, "to": "ops/reply"}); code != http.StatusOK {
		t.Fatalf("reroute = %d", code)
	}
	r = routingOf()
	if r["status"] != "chosen" || r["route_id"] != rerouteJobRoute || r["method"] != "you" || r["wrong"] != true || r["share"] != 0.3 {
		t.Fatalf("chosen routing = %v", r)
	}
	code, ev, _ := doJSON(t, "GET", base+"/v1/events/"+evID, nil)
	outs, _ := ev["outcomes"].([]any)
	found := false
	for _, o := range outs {
		m := o.(map[string]any)
		if m["route_id"] == rerouteJobRoute && m["route_method"] == "you" && m["decision_id"] == routeDecisionID && m["share"] == 0.3 {
			found = true
		}
	}
	if code != http.StatusOK || !found {
		t.Fatalf("outcomes = %v", outs)
	}
	code, list, _ := doJSON(t, "GET", base+"/v1/events?workspace="+wsID, nil)
	rows, _ := list["events"].([]any)
	if code != http.StatusOK || len(rows) == 0 || rows[0].(map[string]any)["routing"] == nil {
		t.Fatalf("list = %d %v", code, list)
	}
}

func TestEventOutcomeFilterNotChosen(t *testing.T) {
	base, wsID, evID, _, _ := routeViewFixture(t)
	if code, _, _ := doJSON(t, "POST", base+"/v1/events/"+evID+"/reroute", map[string]any{"workspace": wsID, "to": "ops/reply"}); code != http.StatusOK {
		t.Fatalf("reroute = %d", code)
	}
	code, list, _ := doJSON(t, "GET", base+"/v1/events?outcome=not_chosen", nil)
	rows, _ := list["events"].([]any)
	if code != http.StatusOK || len(rows) != 1 {
		t.Fatalf("not_chosen = %d %v", code, list)
	}
	row := rows[0].(map[string]any)
	if row["route_id"] != rerouteAgentRoute || row["outcome"] != "not_chosen" || row["route_method"] != "you" || row["share"] != 0.5 {
		t.Fatalf("row = %v", row)
	}
	_, list, _ = doJSON(t, "GET", base+"/v1/events?outcome=route_wait", nil)
	if rows, _ := list["events"].([]any); len(rows) != 0 {
		t.Fatalf("route_wait = %v", rows)
	}
}

func TestEventOutcomeIgnored(t *testing.T) {
	base, wsID, evID, _, _ := routeViewFixture(t)
	if code, _, _ := doJSON(t, "POST", base+"/v1/events/"+evID+"/reroute", map[string]any{"workspace": wsID, "to": "nobody", "ignore": true}); code != http.StatusOK {
		t.Fatalf("ignore = %d", code)
	}
	_, ev, _ := doJSON(t, "GET", base+"/v1/events/"+evID, nil)
	outs, _ := ev["outcomes"].([]any)
	if len(outs) == 0 {
		t.Fatalf("outcomes = %v", ev)
	}
	for _, o := range outs {
		m := o.(map[string]any)
		if m["outcome"] != "not_chosen" || m["route_method"] != "ignored" {
			t.Fatalf("outcome = %v", m)
		}
	}
	if list, _ := ev["routing"].([]any); len(list) != 1 || list[0].(map[string]any)["method"] != "ignored" {
		t.Fatalf("routing = %v", ev["routing"])
	}
}

func TestRoutingTestEndpoint(t *testing.T) {
	base, wsID, _, _, srv := routeViewFixture(t)
	url := base + "/v1/workspaces/" + wsID + "/connectors/email/routing/test"
	draft := map[string]any{"rules": []map[string]string{{"text_has": "meet", "to": "ops/reply"}}}
	code, rep, _ := doJSON(t, "POST", url, map[string]any{"text": "Can we meet?", "from": "a@b.co", "routing": draft})
	results, _ := rep["results"].([]any)
	if code != http.StatusOK || len(results) != 1 {
		t.Fatalf("test = %d %v", code, rep)
	}
	res := results[0].(map[string]any)
	if res["outcome"] != "chosen" || res["route_id"] != rerouteJobRoute || res["method"] != "rule" || res["rule_index"] != float64(1) {
		t.Fatalf("result = %v", res)
	}
	var before int
	st := mustOpenStore(t, filepath.Join(os.Getenv("TREBI_HOME"), "trebi.db"))
	t.Cleanup(func() { _ = st.Close() })
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue`).Scan(&before); err != nil || before != 0 {
		t.Fatalf("queue rows = %d, %v", before, err)
	}
	if code, _, _ := doJSON(t, "POST", url, map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("empty body = %d", code)
	}
	if code, _, _ := doJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/connectors/nope/routing/test", map[string]any{"text": "hi"}); code != http.StatusNotFound {
		t.Fatalf("unknown connection = %d", code)
	}
	other := addWorkspace(t, base, t.TempDir(), "other-ws")
	h := srv.Handler()
	do := func(actor tools.Actor) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+wsID+"/connectors/email/routing/test",
			strings.NewReader(`{"text":"Can we meet?","routing":{"rules":[{"text_has":"meet","to":"ops"}]}}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.WithContext(tools.WithActor(req.Context(), actor)))
		return rec.Code
	}
	if code := do(tools.Actor{Kind: "assistant", Name: "assistant", WorkspaceID: other}); code != http.StatusForbidden {
		t.Fatalf("assistant of another workspace = %d", code)
	}
	if code := do(tools.Actor{Kind: "assistant", Name: "assistant", WorkspaceID: wsID}); code != http.StatusOK {
		t.Fatalf("assistant of the workspace = %d", code)
	}
}

// routeViewFixture is rerouteFixture with a full router decision: shares of
// 0.5, 0.3, and 0.2 for the agent route, the job route, and nobody.
func routeViewFixture(t *testing.T) (base, wsID, evID, inboxID string, s *api.Server) {
	t.Helper()
	srv, base, wsID, evID, inboxID := rerouteFixture(t)
	st := mustOpenStore(t, filepath.Join(os.Getenv("TREBI_HOME"), "trebi.db"))
	t.Cleanup(func() { _ = st.Close() })
	choices, _ := json.Marshal(map[string]string{"r1": rerouteAgentRoute, "r2": rerouteJobRoute})
	facts, _ := json.Marshal(map[string]string{"sure_at": "0.7", "unsure": "ask", "no_fit": "ask", "choices": string(choices)})
	questions, _ := json.Marshal([]map[string]any{{"id": "route", "type": "choice", "shares": true, "options": []map[string]string{
		{"key": "r1", "ref": rerouteAgentRoute, "description": "ops (agent): Billing questions"},
		{"key": "r2", "ref": rerouteJobRoute, "description": "ops/reply (job): Support questions"},
		{"key": "nobody", "description": "Nobody."},
	}}})
	answers, _ := json.Marshal(map[string]any{"answers": []map[string]any{{
		"id": "route", "value": "r1", "confidence": 0.5, "distribution": map[string]float64{"r1": 0.5, "r2": 0.3, "nobody": 0.2},
	}}})
	if _, err := st.DB().Exec(`UPDATE decisions SET scope='email', facts_json=?, questions_json=?, answers_json=?, cost_usd=0.25 WHERE id=?`,
		string(facts), string(questions), string(answers), routeDecisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE events SET text='Can we meet?', sender='a@b.co', identity='a@b.co' WHERE id=?`, evID); err != nil {
		t.Fatal(err)
	}
	return base, wsID, evID, inboxID, srv
}

func decisionItems(t *testing.T, base, query string) []any {
	t.Helper()
	code, body, _ := doJSON(t, "GET", base+"/v1/decisions?"+query, nil)
	if code != http.StatusOK {
		t.Fatalf("list %s = %d %v", query, code, body)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items = %v", body["items"])
	}
	return items
}
