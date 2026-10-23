package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors/oauth/oauthtest"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
)

// TestOnlineLoginAndProxy adds an online server with login, plays the
// browser, and proves that the return page finishes the login once and
// never shows the code. Then a run reaches the server through the proxy
// with its run token, and only for its own connections.
func TestOnlineLoginAndProxy(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(st, Options{})
	h := s.Handler()
	srv := oauthtest.New(t, oauthtest.Options{DCR: true})
	do := func(method, path, body string, token ...string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if len(token) > 0 {
			req.Header.Set(tools.HeaderRunToken, token[0])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := do("POST", "/v1/connectors/sources/check", `{"url":"`+srv.MCPURL()+`"}`)
	var chk map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &chk)
	if login, _ := chk["login"].(map[string]any); rec.Code != http.StatusOK || chk["kind"] != "online" || login["needed"] != true || login["registration"] != "dcr" {
		t.Fatalf("check = %d %s", rec.Code, rec.Body)
	}

	rec = do("POST", "/v1/connectors", `{"url":"`+srv.MCPURL()+`","title":"Granola","login":true}`)
	var row map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &row)
	login, _ := row["login"].(map[string]any)
	step, _ := login["step"].(map[string]any)
	if rec.Code != http.StatusCreated || login["flow_id"] == nil || step["kind"] != "url" {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	flow := "/v1/connectors/" + row["id"].(string) + "/login/" + login["flow_id"].(string)
	if rec = do("GET", flow+"?wait=0", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"step"`) {
		t.Fatalf("status now = %d %s", rec.Code, rec.Body)
	}
	if rec = do("GET", flow+"?wait=soon", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad wait = %d %s", rec.Code, rec.Body)
	}
	ret, err := srv.Authorize(step["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	code := ret.Query().Get("code")
	waited := make(chan *httptest.ResponseRecorder, 1)
	go func() { waited <- do("GET", flow+"?wait=30", "") }()
	rec = do("GET", "/login/done?"+ret.RawQuery, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "You are logged in to Granola") || strings.Contains(rec.Body.String(), code) {
		t.Fatalf("return = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "/connectors/"+url.PathEscape(row["id"].(string))+"?tab=login") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("return page = %s", rec.Body)
	}
	if w := <-waited; w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("status after wait = %d %s", w.Code, w.Body)
	}
	if rec = do("GET", "/login/done?"+ret.RawQuery, ""); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "This login is not open") {
		t.Fatalf("second return = %d %s", rec.Code, rec.Body)
	}
	rec = do("GET", "/v1/connectors/"+row["id"].(string), "")
	_ = json.Unmarshal(rec.Body.Bytes(), &row)
	if l, _ := row["login"].(map[string]any); l["state"] != "connected" {
		t.Fatalf("row after login = %v", row["login"])
	}

	id := row["id"].(string)
	var view map[string]any
	rec = do("GET", "/v1/connectors/"+id+"/oauth", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if st, _ := view["status"].(map[string]any); rec.Code != http.StatusOK || st["logged_in"] != true || st["client_kind"] != "dcr" {
		t.Fatalf("oauth = %d %s", rec.Code, rec.Body)
	}
	rec = do("PATCH", "/v1/connectors/"+id+"/oauth", `{"mode":"header","scopes":["read"]}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if set, _ := view["settings"].(map[string]any); rec.Code != http.StatusOK || set["mode"] != "header" {
		t.Fatalf("oauth set = %d %s", rec.Code, rec.Body)
	}
	if rec = do("PATCH", "/v1/connectors/"+id+"/oauth", `{"mode":"tunnel"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode = %d %s", rec.Code, rec.Body)
	}
	if rec = do("PATCH", "/v1/connectors/"+id+"/oauth", `{"client_id":"app","client_secret_name":"nope"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing secret = %d %s", rec.Code, rec.Body)
	}
	if rec = do("PATCH", "/v1/connectors/"+id+"/oauth", `{"mode":"proxy"}`); rec.Code != http.StatusOK {
		t.Fatalf("back to proxy = %d %s", rec.Code, rec.Body)
	}
	ctx := t.Context()
	for tok, conns := range map[string][]string{"tok-own-0123456789abcdef0123456789abcdef": {id}, "tok-other-0123456789abcdef0123456789abcdef": {"other"}} {
		if _, err := s.runs.Create(ctx, runs.CreateOpts{WorkspaceID: "default", JobID: "j", JobName: "j", Harness: "claude", Connectors: conns, ActorToken: tok}); err != nil {
			t.Fatal(err)
		}
	}
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`
	if rec = do("POST", "/v1/connectors/"+id+"/mcp", call, "tok-own-0123456789abcdef0123456789abcdef"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"result"`) {
		t.Fatalf("own run = %d %s", rec.Code, rec.Body)
	}
	if rec = do("POST", "/v1/connectors/"+id+"/mcp", call, "tok-other-0123456789abcdef0123456789abcdef"); rec.Code != http.StatusForbidden {
		t.Fatalf("other run = %d %s", rec.Code, rec.Body)
	}
	if rec = do("POST", "/v1/connectors/"+id+"/mcp", call, "not-a-token-0123456789abcdef0123456789abcdef"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d %s", rec.Code, rec.Body)
	}
}
