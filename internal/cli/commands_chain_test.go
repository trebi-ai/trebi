package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
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

// TestChainEndToEnd runs ceo → cfo → legal on the fake harness with the
// daemon workers and the API: legal replies, cfo replies, ceo ends. Each run
// links both ways.
func TestChainEndToEnd(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })
	wsDir := filepath.Join(dir, "ws")
	for _, name := range []string{"ceo", "cfo", "legal"} {
		p := filepath.Join(wsDir, "agents", name, "AGENT.md")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("---\ndescription: The "+name+".\nharness: fake\n---\n\nWork.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cp := daemon.New(st, daemon.Options{QueueInterval: 50 * time.Millisecond, TurnWatchInterval: 50 * time.Millisecond})
	srv := api.New(st, api.Options{})
	srv.BindControlPlane(cp.Runs, cp.Workspaces, cp.Connectors, cp.Schedule, cp.Alerts, cp.Files, cp.Skills, cp.Conversations, cp.Harness, cp.Email, cp.Cloud, cp.Webhooks, cp.Launcher, cp.Requests)
	srv.BindChains(cp.Chains)
	ctx, cancel := context.WithCancel(context.Background())
	cp.Start(ctx)
	t.Cleanup(func() { cancel(); cp.Stop() })
	addr, err := srv.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	base := "http://" + addr
	var ws map[string]any
	e2eJSON(t, "POST", base+"/v1/workspaces", map[string]any{"path": wsDir, "name": "chain-e2e"}, &ws)
	wsID, _ := ws["id"].(string)

	// Each agent asks the agent that its prompt names, then ends the turn.
	ask := `curl -s -o /dev/null -X POST -H "X-Trebi-Run-Token: $TREBI_RUN_TOKEN" -H "Content-Type: application/json" ` +
		`-d '{"text":"%s"}' ` + base + "/v1/workspaces/" + wsID + "/agents/%s/ask"
	t.Setenv("TREBI_FAKE_HARNESS_CMD", `case "$TREBI_FAKE_PROMPT" in
*"Reply from agents/"*) ;;
*"NEXT=legal"*) `+fmt.Sprintf(ask, "Check the contract.", "legal")+` ;;
*"NEXT=cfo"*) `+fmt.Sprintf(ask, "Budget? NEXT=legal", "cfo")+` ;;
esac
echo "$TREBI_RUN_COMPLETE"`)
	var ceo map[string]any
	e2eJSON(t, "POST", base+"/v1/workspaces/"+wsID+"/agents/ceo/wake", map[string]any{"extra_prompt": "Plan Q3. NEXT=cfo"}, &ceo)
	ceoID, _ := ceo["id"].(string)

	deadline := time.Now().Add(30 * time.Second)
	for {
		var run map[string]any
		e2eJSON(t, "GET", base+"/v1/runs/"+ceoID, nil, &run)
		if run["status"] == "complete" && run["turn_count"] == float64(2) {
			break
		}
		if time.Now().After(deadline) {
			var chain map[string]any
			e2eJSON(t, "GET", base+"/v1/chains/"+mapStr(run, "trace_id"), nil, &chain)
			b, _ := json.MarshalIndent(chain, "", " ")
			t.Fatalf("ceo did not finish: status %v turns %v\nchain %s", run["status"], run["turn_count"], b)
		}
		time.Sleep(100 * time.Millisecond)
	}

	links := func(runID string) (in, out []map[string]any) {
		var body map[string]any
		e2eJSON(t, "GET", base+"/v1/runs/"+runID, nil, &body)
		chain, _ := body["chain"].(map[string]any)
		for _, side := range []struct {
			key string
			dst *[]map[string]any
		}{{"in", &in}, {"out", &out}} {
			list, _ := chain[side.key].([]any)
			for _, h := range list {
				*side.dst = append(*side.dst, h.(map[string]any))
			}
		}
		return in, out
	}
	end := func(h map[string]any, side string) (agent, runID string) {
		e, _ := h[side].(map[string]any)
		return mapStr(e, "agent"), mapStr(e, "run_id")
	}
	ceoIn, ceoOut := links(ceoID)
	if len(ceoOut) != 1 || len(ceoIn) != 1 || mapStr(ceoOut[0], "status") != "answered" || mapStr(ceoIn[0], "kind") != "reply" {
		t.Fatalf("ceo links in=%v out=%v", ceoIn, ceoOut)
	}
	_, cfoID := end(ceoOut[0], "to")
	if a, r := end(ceoIn[0], "from"); a != "cfo" || r != cfoID {
		t.Fatalf("ceo reply from %s %s, want cfo %s", a, r, cfoID)
	}
	cfoIn, cfoOut := links(cfoID)
	if len(cfoIn) != 2 || len(cfoOut) != 2 {
		t.Fatalf("cfo links in=%v out=%v", cfoIn, cfoOut)
	}
	if a, r := end(cfoIn[0], "from"); a != "ceo" || r != ceoID || mapStr(cfoIn[0], "kind") != "ask" {
		t.Fatalf("cfo asked by %s %s", a, r)
	}
	_, legalID := end(cfoOut[0], "to")
	if a, r := end(cfoIn[1], "from"); a != "legal" || r != legalID || mapStr(cfoIn[1], "kind") != "reply" {
		t.Fatalf("cfo reply from %s %s, want legal %s", a, r, legalID)
	}
	if a, r := end(cfoOut[1], "to"); a != "ceo" || r != ceoID || mapStr(cfoOut[1], "kind") != "reply" {
		t.Fatalf("cfo replied to %s %s", a, r)
	}
	legalIn, legalOut := links(legalID)
	if len(legalIn) != 1 || len(legalOut) != 1 || mapStr(legalOut[0], "kind") != "reply" {
		t.Fatalf("legal links in=%v out=%v", legalIn, legalOut)
	}
	if a, r := end(legalIn[0], "from"); a != "cfo" || r != cfoID {
		t.Fatalf("legal asked by %s %s", a, r)
	}
}

func e2eJSON(t *testing.T, method, url string, body, out any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %d %s", method, url, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}
