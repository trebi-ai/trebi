package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const routingViewJSON = `{"workspace":"ws_aaaa1111","connection":{"id":"con_1","slug":"gmail","type":"gmail"},"path":"routes/gmail.yaml","exists":true,"hash":"abc","mode":"on","notes":"","unsure":"ask","no_fit":"drop","sure_at":0.7,
"routes":[{"id":"rt_aaaaaaaaaa","target":{"kind":"job","name":"sling-cs/email-support","exists":true},"gets":"by_hand","use_for":"","filters":{"rooms":["x"],"from":"people","contains":[]}}],
"rules":[{"field":"sender_is","value":"a@b.co","to":"nobody"}],"problems":[]}`

func TestRoutingSetFlags(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		input string
		body  string
		match string
	}{
		{"mode", []string{"--mode", "on"}, "", `{"mode":"on"}`, ""},
		{"gets", []string{"--gets", "rt_a=chosen"}, "", `{"routes":[{"gets":"chosen","id":"rt_a"}]}`, ""},
		{"gets and use-for", []string{"--gets", "rt_a=off", "--use-for", "rt_a=Billing"}, "", `{"routes":[{"gets":"off","id":"rt_a","use_for":"Billing"}]}`, ""},
		{"settings", []string{"--notes", "n", "--unsure", "guess", "--no-fit", "ask", "--sure-at", "0.5"}, "", `{"no_fit":"ask","notes":"n","sure_at":0.5,"unsure":"guess"}`, ""},
		{"patch stdin", []string{"--patch", "-"}, `{"remove_routes":["rt_b"]}`, `{"remove_routes":["rt_b"]}`, ""},
		{"patch inline plus flag", []string{"--patch", `{"mode":"off"}`, "--notes", "x"}, "", `{"mode":"off","notes":"x"}`, ""},
		{"if-match", []string{"--mode", "off", "--if-match", `"abc"`}, "", `{"mode":"off"}`, `"abc"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, calls := routingServer(t, routingViewJSON)
			args := append([]string{"connector", "routing", "set", "gmail", "--workspace", "sling"}, tc.args...)
			if _, err := runCLIWithInput(t, client, tc.input, args...); err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 1 {
				t.Fatalf("calls = %+v", *calls)
			}
			c := (*calls)[0]
			if c.Method != "PATCH" || c.Path != "/v1/workspaces/ws_aaaa1111/connectors/gmail/routing" || c.Body != tc.body || c.IfMatch != tc.match {
				t.Fatalf("call = %+v, want body %s", c, tc.body)
			}
		})
	}
	client, calls := routingServer(t, routingViewJSON)
	for _, args := range [][]string{
		{"--mode", "maybe"}, {"--gets", "rt_a"}, {"--sure-at", "2"}, {},
	} {
		if _, err := runCLI(t, client, append([]string{"connector", "routing", "set", "gmail", "-w", "sling"}, args...)...); err == nil {
			t.Errorf("args %v: no usage error", args)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("a bad flag sent a request: %+v", *calls)
	}
}

func TestRoutingAdd(t *testing.T) {
	client, calls := routingServer(t, routingViewJSON)
	out, err := runCLI(t, client, "connector", "routing", "add", "gmail", "-w", "sling",
		"--job", "sling-cs/email-support", "--gets", "by_hand", "--room", "x", "--input", "topic=billing")
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || (*calls)[0].Method != "GET" || (*calls)[1].Method != "PATCH" {
		t.Fatalf("calls = %+v", *calls)
	}
	var body struct {
		Routes []map[string]any `json:"routes"`
	}
	if err := json.Unmarshal([]byte((*calls)[1].Body), &body); err != nil || len(body.Routes) != 1 {
		t.Fatalf("body = %s", (*calls)[1].Body)
	}
	r := body.Routes[0]
	if r["id"] != nil || r["job"] != "sling-cs/email-support" || r["gets"] != "by_hand" || r["rooms"].([]any)[0] != "x" || r["inputs"].(map[string]any)["topic"] != "billing" {
		t.Fatalf("route = %v", r)
	}
	if _, err := runCLI(t, client, "connector", "routing", "add", "gmail", "-w", "sling"); err == nil {
		t.Fatal("a route with no target passed")
	}
	if _, err := runCLI(t, client, "connector", "routing", "add", "gmail", "-w", "sling", "--agent", "a", "--job", "a/b"); err == nil {
		t.Fatal("two targets passed")
	}
	_ = out
}

func TestRoutingRemoveNeedsYes(t *testing.T) {
	client, calls := routingServer(t, routingViewJSON)
	_, err := runCLI(t, client, "connector", "routing", "remove", "gmail", "rt_a", "-w", "sling")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("calls = %+v", *calls)
	}
	if _, err := runCLI(t, client, "connector", "routing", "remove", "gmail", "rt_a", "rt_b", "--yes", "-w", "sling"); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0].Body != `{"remove_routes":["rt_a","rt_b"]}` {
		t.Fatalf("calls = %+v", *calls)
	}
}

func TestRoutingShowProblems(t *testing.T) {
	view := strings.Replace(routingViewJSON, `"problems":[]`, `"problems":[{"code":"route_target_missing","severity":"error","path":"routes[rt_aaaaaaaaaa]","message":"agent gone does not exist","fix":{"action":"connector.routing_set","label":"Remove this route","input":{"connector":"gmail","workspace":"ws_aaaa1111","remove_routes":["rt_aaaaaaaaaa"]}}}]`, 1)
	client, _ := routingServer(t, view)
	out, err := runCLI(t, client, "connector", "routing", "show", "gmail", "-w", "sling")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"routing on", "sling-cs/email-support", "by_hand", "rooms=x", "rule 0: sender_is a@b.co -> nobody", "route_target_missing", "trebi connector routing remove gmail rt_aaaaaaaaaa --yes --workspace sling"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	out, err = runCLI(t, client, "connector", "routing", "show", "gmail", "-w", "sling", "--json")
	if err != nil || !strings.Contains(out, `"hash": "abc"`) {
		t.Fatalf("json = %s %v", out, err)
	}
}

func TestRoutingList(t *testing.T) {
	client, _ := routingServer(t, `{"routing":[`+routingViewJSON+`]}`)
	out, err := runCLI(t, client, "connector", "routing", "list", "-w", "sling")
	if err != nil || !strings.Contains(strings.ToLower(out), "gmail") {
		t.Fatalf("list = %s %v", out, err)
	}
}

type routingCall struct {
	Method, Path, IfMatch, Body string
}

// routingServer answers the workspace list and the routing routes, and
// records each routing call.
func routingServer(t *testing.T, view string) (*APIClient, *[]routingCall) {
	t.Helper()
	var calls []routingCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/workspaces" {
			_, _ = w.Write([]byte(`{"workspaces":[{"id":"ws_aaaa1111","name":"sling"}]}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, routingCall{r.Method, r.URL.Path, r.Header.Get("If-Match"), string(b)})
		_, _ = w.Write([]byte(view))
	}))
	t.Cleanup(srv.Close)
	return &APIClient{base: srv.URL, httpClient: srv.Client()}, &calls
}
