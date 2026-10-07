package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
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
