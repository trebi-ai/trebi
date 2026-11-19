package api_test

import (
	"net/http"
	"testing"
)

func TestDecisionRoutes(t *testing.T) {
	e := startEmailAPI(t)
	wsID := newEmailWorkspace(t, e, "ellis")
	for _, id := range []string{"dec_01", "dec_02", "dec_03"} {
		if _, err := e.st.DB().Exec(`INSERT INTO decisions (id, workspace_id, kind, kind_version, subject_kind, subject_id, decider,
			method, status, outcome, questions_json, answers_json, took_ms, cost_usd, tokens_input, tokens_output, tokens_cache_read,
			tokens_cache_creation, created_at) VALUES (?, ?, 'email.reply_risk', 1, 'email', ?, 'fake', 'verbal', 'ok', 'deliver',
			'[]', '{"answers":[],"reasons":[]}', 5, 0.01, 10, 20, 300, 40, '2099-01-01T00:00:00Z')`, id, wsID, "m"+id); err != nil {
			t.Fatal(err)
		}
	}
	code, m := e.do(t, http.MethodGet, "/v1/decisions?workspace=ellis&limit=2", nil)
	if code != 200 || len(m["items"].([]any)) != 2 || m["next_cursor"] != "dec_02" {
		t.Fatalf("list: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodGet, "/v1/decisions?cursor=dec_02", nil); code != 200 || len(m["items"].([]any)) != 1 {
		t.Fatalf("page 2: %d %v", code, m)
	}
	if code, _ = e.do(t, http.MethodGet, "/v1/decisions?cursor=bad", nil); code != 400 {
		t.Fatalf("bad cursor: %d", code)
	}
	if code, m = e.do(t, http.MethodGet, "/v1/decisions?subject_kind=email&subject_id=mdec_03", nil); code != 200 || len(m["items"].([]any)) != 1 {
		t.Fatalf("subject: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodGet, "/v1/decisions/kinds", nil); code != 200 || len(m["items"].([]any)) != 2 {
		t.Fatalf("kinds: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodGet, "/v1/decisions/stats?kind=email.reply_risk&since=2098-01-01T00:00:00Z", nil); code != 200 || m["total"] != 3.0 {
		t.Fatalf("stats: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodPost, "/v1/decisions/dec_01/feedback", map[string]any{"value": "agree"}); code != 200 || m["feedback"] != "agree" {
		t.Fatalf("feedback: %d %v", code, m)
	}
	if code, _ = e.do(t, http.MethodPost, "/v1/decisions/dec_01/feedback", map[string]any{"value": "maybe"}); code != 400 {
		t.Fatalf("bad feedback: %d", code)
	}
	if code, m = e.do(t, http.MethodGet, "/v1/decisions/dec_01/raw", nil); code != 200 || m["raw"] != "" {
		t.Fatalf("raw: %d %v", code, m)
	}
	code, m = e.do(t, http.MethodGet, "/v1/decisions/dec_01", nil)
	tok, _ := m["tokens"].(map[string]any)
	if code != 200 || m["cost_usd"] != 0.01 || tok["input"] != 10.0 || tok["output"] != 20.0 || tok["cache_read"] != 300.0 ||
		tok["cache_creation"] != 40.0 || tok["billed"] != 70.0 || tok["cached"] != 300.0 {
		t.Fatalf("get tokens: %d %v", code, m)
	}
	if code, _ = e.do(t, http.MethodGet, "/v1/decisions/dec_99", nil); code != 404 {
		t.Fatalf("unknown: %d", code)
	}
	if code, _ = e.do(t, http.MethodPost, "/v1/decisions/test", map[string]any{"kind": "nope", "text": "hi"}); code != 400 {
		t.Fatalf("unknown kind: %d", code)
	}
	// No decider is wired in a bare server, so the test fails closed.
	code, m = e.do(t, http.MethodPost, "/v1/decisions/test", map[string]any{"kind": "email.reply_risk", "text": "Thanks for the quote."})
	if code != 200 || m["outcome"] != "hold" || m["status"] == "ok" || m["id"] != "" {
		t.Fatalf("test: %d %v", code, m)
	}
}
