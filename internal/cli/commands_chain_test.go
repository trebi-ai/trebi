package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const chainFixture = `{"chain":{"trace_id":"T1","status":"open","root_kind":"agent","root_run_id":"R1","root_agent":"ceo",
"limits":{"max_hops":5,"max_turns":20},"usage":{"depth":1,"turns":3,"cost_usd":0.84}},
"hops":[
 {"id":"hop_A","kind":"ask","status":"answered","from":{"kind":"agent","agent":"ceo","run_id":"R1","turn":1},"to":{"kind":"agent","agent":"cfo","run_id":"R2","turn":1}},
 {"id":"hop_B","kind":"ask","status":"running","from":{"kind":"agent","agent":"cfo","run_id":"R2","turn":1},"to":{"kind":"agent","agent":"legal","run_id":"R3","turn":1}},
 {"id":"hop_C","kind":"reply","status":"answered","reply_of":"hop_A","from":{"kind":"agent","agent":"cfo","run_id":"R2","turn":2},"to":{"kind":"agent","agent":"ceo","run_id":"R1","turn":2}}],
"runs":[{"id":"R1","agent":"ceo","depth":0,"turns":[1,2]}]}`

func TestChainShowTree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chains/R1" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, chainFixture)
	}))
	t.Cleanup(srv.Close)
	out, err := runCLI(t, &APIClient{base: srv.URL, httpClient: srv.Client()}, "chain", "show", "R1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"chain T1  open  depth 1 of 5  steps 3 of 20  $0.84",
		"ceo      run R1  turns 1,2  root",
		"└─ ask    hop_A → cfo  run R2 turn 1  answered",
		"   ├─ ask    hop_B → legal  run R3 turn 1  running",
		"   └─ reply  hop_C → ceo  run R1 turn 2",
	}
	if got := strings.TrimSpace(out); got != strings.Join(want, "\n") {
		t.Fatalf("tree:\n%s", out)
	}
}

func TestAgentAskCLI(t *testing.T) {
	var body map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"hop":{"id":"hop_A","status":"queued"},"next":"end_turn"}`)
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	t.Setenv("TREBI_RUN_TOKEN", "tok")
	out, err := runCLI(t, client, "agent", "ask", "-w", "ws1", "cfo", "What", "is", "the", "budget?", "--context", "Q3", "--wait", "30s")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/workspaces/ws1/agents/cfo/ask" || body["text"] != "What is the budget?" || body["context"] != "Q3" || body["wait_ms"] != nil {
		t.Fatalf("request %s %v", path, body)
	}
	if !strings.Contains(out, "Asked cfo: hop hop_A (queued)") || !strings.Contains(out, "End your turn now.") {
		t.Fatalf("out %q", out)
	}
	if _, err := runCLI(t, client, "agent", "handoff", "-w", "ws1", "legal", "Review", "it."); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/workspaces/ws1/agents/legal/handoff" {
		t.Fatalf("handoff path %s", path)
	}
}
