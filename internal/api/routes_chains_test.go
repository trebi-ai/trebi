package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestChainAskRoutes(t *testing.T) {
	_, base, dir := startAPI(t)
	wsDir := t.TempDir()
	writeTeamFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "chains")
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	ceoTok, opsTok := "ceo-token-00000000000000000000000", "ops-token-00000000000000000000000"
	insertAgentCaller(t, st, wsID, "ceo", ceoTok, "running")
	insertAgentCaller(t, st, wsID, "ops", opsTok, "running")
	agents := base + "/v1/workspaces/" + wsID + "/agents/"

	// A person asks: 201, queued, and a repeat of the key returns the hop with 200.
	ask := map[string]any{"text": "What is the Q3 budget?", "context": "Board meets Friday.", "idempotency_key": "k1"}
	code, body, _ := doJSON(t, "POST", agents+"cfo/ask", ask)
	hop, _ := body["hop"].(map[string]any)
	if code != http.StatusCreated || hop["status"] != "queued" || body["conversation_id"] == "" || body["next"] != nil {
		t.Fatalf("human ask: %d %v", code, body)
	}
	if from, _ := hop["from"].(map[string]any); from["kind"] != "human" {
		t.Fatalf("from = %v", hop["from"])
	}
	humanHop, humanTrace := hop["id"].(string), hop["trace_id"].(string)
	code, body, _ = doJSON(t, "POST", agents+"cfo/ask", ask)
	if hop, _ = body["hop"].(map[string]any); code != http.StatusOK || hop["id"] != humanHop {
		t.Fatalf("repeat: %d %v", code, body)
	}

	for _, c := range []struct {
		name, path, token string
		body              map[string]any
		code              int
		errCode, reason   string
	}{
		{"unknown agent", "nobody/ask", "", map[string]any{"text": "hi"}, 404, "not_found", ""},
		{"no text", "cfo/ask", "", map[string]any{"text": " "}, 400, "validation", ""},
		{"self", "ceo/ask", ceoTok, map[string]any{"text": "hi"}, 400, "validation", ""},
		{"team", "cfo/ask", opsTok, map[string]any{"text": "hi"}, 403, "forbidden", "team"},
	} {
		code, body := doScoped(t, "POST", agents+c.path, c.token, c.body)
		if code != c.code || errCode(body) != c.errCode {
			t.Fatalf("%s: %d %v", c.name, code, body)
		}
		if c.reason != "" && (errDetails(body)["refuse_reason"] != c.reason || errDetails(body)["hop_id"] == "") {
			t.Fatalf("%s details: %v", c.name, body)
		}
	}

	// An agent asks: the reply tells it to end the turn.
	code, body = doScoped(t, "POST", agents+"cfo/ask", ceoTok, map[string]any{"text": "Can we hire two people?"})
	hop, _ = body["hop"].(map[string]any)
	if code != http.StatusCreated || body["next"] != "end_turn" || body["message"] == "" || hop["trace_id"] != "trace" {
		t.Fatalf("agent ask: %d %v", code, body)
	}
	if path, _ := hop["path"].([]any); len(path) != 2 || path[1] != "cfo" {
		t.Fatalf("path = %v", hop["path"])
	}
	code, body = doScoped(t, "POST", agents+"legal/handoff", ceoTok, map[string]any{"text": "Review docs/nda.md."})
	if hop, _ = body["hop"].(map[string]any); code != http.StatusCreated || body["next"] != nil || hop["kind"] != "handoff" {
		t.Fatalf("handoff: %d %v", code, body)
	}

	// Reads.
	code, body, _ = doJSON(t, "GET", base+"/v1/chains?workspace="+wsID, nil)
	if list, _ := body["chains"].([]any); code != 200 || len(list) != 2 {
		t.Fatalf("list: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/chains?workspace="+wsID+"&agent=legal", nil)
	if list, _ := body["chains"].([]any); code != 200 || len(list) != 1 {
		t.Fatalf("list agent: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/chains/trace", nil)
	chain, _ := body["chain"].(map[string]any)
	hops, _ := body["hops"].([]any)
	if code != 200 || chain["root_kind"] != "agent" || chain["root_agent"] != "ceo" || chain["live"] != true || len(hops) != 3 {
		t.Fatalf("show: %d %v", code, body)
	}
	if h := hops[0].(map[string]any); h["text"] != nil || h["status"] != "refused" {
		t.Fatalf("first hop = %v", h)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/hops/"+humanHop, nil)
	if hop, _ = body["hop"].(map[string]any); code != 200 || hop["text"] != ask["text"] || hop["context"] != ask["context"] || hop["trace_id"] != humanTrace {
		t.Fatalf("hop: %d %v", code, body)
	}
	if code, _, _ = doJSON(t, "GET", base+"/v1/chains/nope", nil); code != 404 {
		t.Fatalf("missing chain: %d", code)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/runs/run-ceo-running", nil)
	if _, ok := body["chain"]; code != 200 || !ok {
		t.Fatalf("run chain key: %d %v", code, body)
	}

	// Chains off: every ask is refused.
	if code, _, _ = doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID, map[string]any{"chains": map[string]any{"enabled": false}}); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	code, body, _ = doJSON(t, "POST", agents+"cfo/ask", map[string]any{"text": "Still there?"})
	if code != http.StatusConflict || errCode(body) != "agent_unavailable" || errDetails(body)["refuse_reason"] != "disabled" {
		t.Fatalf("disabled: %d %v", code, body)
	}
}

func TestChainStopRoute(t *testing.T) {
	_, base, dir := startAPI(t)
	wsDir := t.TempDir()
	writeTeamFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "chain-stop")
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	tok := "ceo-token-00000000000000000000000"
	insertAgentCaller(t, st, wsID, "ceo", tok, "running")
	agents := base + "/v1/workspaces/" + wsID + "/agents/"
	if code, body := doScoped(t, "POST", agents+"cfo/ask", tok, map[string]any{"text": "Budget?"}); code != http.StatusCreated {
		t.Fatalf("ask: %d %v", code, body)
	}
	stop := base + "/v1/chains/trace/stop"
	code, body, _ := doJSON(t, "POST", stop, map[string]any{"reason": "Wrong plan."})
	if code != http.StatusBadRequest || errDetails(body)["confirm_required"] == nil {
		t.Fatalf("no confirm: %d %v", code, body)
	}
	if code, body := doScoped(t, "POST", stop+"?confirm=1", tok, map[string]any{}); code != http.StatusForbidden {
		t.Fatalf("agent stop: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "POST", stop+"?confirm=1", map[string]any{"reason": "Wrong plan."})
	if hops, _ := body["cancelled_hops"].([]any); code != 200 || body["stopped"] != true || len(hops) != 1 {
		t.Fatalf("stop: %d %v", code, body)
	}
	code, body = doScoped(t, "POST", agents+"legal/ask", tok, map[string]any{"text": "Again?"})
	if code != http.StatusConflict || errCode(body) != "chain_stopped" {
		t.Fatalf("ask after stop: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/chains/trace", nil)
	if chain, _ := body["chain"].(map[string]any); code != 200 || chain["status"] != "stopped" || chain["stop_reason"] != "Wrong plan." {
		t.Fatalf("show: %d %v", code, body)
	}
}

func TestChainPairRate(t *testing.T) {
	_, base, dir := startAPI(t)
	wsDir := t.TempDir()
	writeTeamFixture(t, wsDir)
	wsID := addWorkspace(t, base, wsDir, "chain-rate")
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	tok := "ceo-token-00000000000000000000000"
	insertAgentCaller(t, st, wsID, "ceo", tok, "running")
	if code, body, _ := doJSON(t, "PATCH", base+"/v1/workspaces/"+wsID, map[string]any{"chains": map[string]any{"pair_per_hour": 1, "max_turns": 50}}); code != 200 {
		t.Fatalf("settings: %d %v", code, body)
	}
	url := base + "/v1/workspaces/" + wsID + "/agents/cfo/ask"
	if code, body := doScoped(t, "POST", url, tok, map[string]any{"text": "One."}); code != http.StatusCreated {
		t.Fatalf("first: %d %v", code, body)
	}
	code, body := doScoped(t, "POST", url, tok, map[string]any{"text": "Two."})
	if code != http.StatusTooManyRequests || errCode(body) != "rate_limited" || errDetails(body)["refuse_reason"] != "rate" {
		t.Fatalf("second: %d %v", code, body)
	}
}

func writeTeamFixture(t *testing.T, wsDir string) {
	t.Helper()
	files := map[string]string{
		"ceo":   "---\ndescription: Runs the company.\nharness: fake\n---\n\nLead.\n",
		"cfo":   "---\ndescription: Budget and spend.\nharness: fake\n---\n\nCount.\n",
		"ops":   "---\ndescription: Operations.\nharness: fake\nteam:\n  can_ask: [legal]\n---\n\nOperate.\n",
		"legal": "---\ndescription: Contracts.\nharness: fake\n---\n\nReview.\n",
	}
	for name, md := range files {
		path := filepath.Join(wsDir, "agents", name, "AGENT.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCreateRunInheritsTrace(t *testing.T) {
	_, base, dir := startAPI(t)
	wsDir := t.TempDir()
	writeTeamFixture(t, wsDir)
	for _, n := range []string{"plain", "dry", "mcp", "caller"} {
		writeFiles(t, map[string]string{filepath.Join(wsDir, "agents", "ceo", n, "JOB.md"): "---\nharness: fake\n---\n\nDo it.\n"})
	}
	wsID := addWorkspace(t, base, wsDir, "trace")
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	token := "ceo-token-00000000000000000000000"
	insertAgentCaller(t, st, wsID, "ceo", token, "running")
	insertCallerTurn(t, st, "run-ceo-running", "T", []string{"x", indexedJobID(t, st, wsID, "ceo")})

	wantPath := func(name string) string { return `["x","` + indexedJobID(t, st, wsID, name) + `"]` }
	launch := func(name, tok string, body map[string]any) (int, map[string]any) {
		body["workspace"] = wsID
		return doScoped(t, "POST", base+"/v1/jobs/"+url.PathEscape(name)+"/run", tok, body)
	}

	code, run := launch("ceo/plain", token, map[string]any{})
	if code != http.StatusCreated {
		t.Fatalf("token launch: %d %v", code, run)
	}
	if trace, path, depth := firstTurn(t, st, run["id"].(string)); trace != "T" || path != wantPath("ceo/plain") || depth != 1 {
		t.Fatalf("token turn: %q %s %d", trace, path, depth)
	}

	code, run = launch("ceo/plain", "", map[string]any{"if_running": "allow"})
	if code != http.StatusCreated {
		t.Fatalf("person launch: %d %v", code, run)
	}
	if trace, path, depth := firstTurn(t, st, run["id"].(string)); trace == "T" || trace == "" || path != "[]" || depth != 0 {
		t.Fatalf("person turn: %q %s %d", trace, path, depth)
	}

	var before int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM run_turns`).Scan(&before)
	code, _ = launch("ceo/dry", token, map[string]any{"dry_run": true})
	var after int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM run_turns`).Scan(&after)
	if code != http.StatusOK || after != before {
		t.Fatalf("dry run: %d turns %d -> %d", code, before, after)
	}

	// An agent token cannot use /mcp; a job run token can.
	jobToken := "job-token-00000000000000000000000"
	insertRunCaller(t, st, wsID, "ceo/caller", "job", jobToken)
	insertCallerTurn(t, st, "run-"+jobToken[:8], "TJ", []string{"x", indexedJobID(t, st, wsID, "ceo/caller")})
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "trace-test", Version: "1"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: base + "/mcp", DisableStandaloneSSE: true, HTTPClient: &http.Client{Transport: tokenTransport(jobToken)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, text := mcpCall(t, cs, "job", "run", map[string]any{"name": "ceo/mcp", "workspace": wsID})
	if res.IsError {
		t.Fatalf("mcp run: %s", text)
	}
	mrun := mcpJSON(t, "job.run", text)
	if trace, path, depth := firstTurn(t, st, mrun["id"].(string)); trace != "TJ" || path != wantPath("ceo/mcp") || depth != 1 {
		t.Fatalf("mcp turn: %q %s %d", trace, path, depth)
	}
}

func TestCreateRunStoppedChain(t *testing.T) {
	_, base, dir := startAPI(t)
	wsDir := t.TempDir()
	writeTeamFixture(t, wsDir)
	writeFiles(t, map[string]string{filepath.Join(wsDir, "agents", "ceo", "digest", "JOB.md"): "---\nharness: fake\n---\n\nDo it.\n"})
	wsID := addWorkspace(t, base, wsDir, "stopped")
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	token := "ceo-token-00000000000000000000000"
	insertAgentCaller(t, st, wsID, "ceo", token, "running")
	insertCallerTurn(t, st, "run-ceo-running", "TS", []string{indexedJobID(t, st, wsID, "ceo")})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`INSERT INTO chains (trace_id, workspace_id, root_kind, status, max_hops, max_turns, created_at, updated_at)
		VALUES ('TS', ?, 'agent', 'stopped', 5, 20, ?, ?)`, wsID, now, now); err != nil {
		t.Fatal(err)
	}
	var before int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&before)
	code, body := doScoped(t, "POST", base+"/v1/jobs/"+url.PathEscape("ceo/digest")+"/run", token, map[string]any{"workspace": wsID})
	if code != http.StatusConflict || errCode(body) != "chain_stopped" {
		t.Fatalf("stopped launch: %d %v", code, body)
	}
	var after int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&after)
	if after != before {
		t.Fatalf("runs %d -> %d", before, after)
	}
}

// insertCallerTurn gives a caller run its turn 1 on a trace.
func insertCallerTurn(t *testing.T, st *store.Store, runID, trace string, path []string) {
	t.Helper()
	raw, _ := json.Marshal(path)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`INSERT INTO run_turns (run_id, n, reason, status, started_at, trace_id, depth, path_json)
		VALUES (?, 1, 'manual', 'running', ?, ?, ?, ?)`, runID, now, trace, len(path)-1, string(raw)); err != nil {
		t.Fatal(err)
	}
}

func firstTurn(t *testing.T, st *store.Store, runID string) (trace, path string, depth int) {
	t.Helper()
	if err := st.DB().QueryRow(`SELECT trace_id, path_json, depth FROM run_turns WHERE run_id=? AND n=1`, runID).Scan(&trace, &path, &depth); err != nil {
		t.Fatalf("turn of %s: %v", runID, err)
	}
	return trace, path, depth
}
