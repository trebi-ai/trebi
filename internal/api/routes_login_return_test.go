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
	"github.com/trebi-ai/trebi/internal/store"
)

// TestLoginReturnPage adds an online server with login, plays the browser,
// and proves that the return page finishes the login once and never shows
// the code.
func TestLoginReturnPage(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := New(st, Options{}).Handler()
	srv := oauthtest.New(t, oauthtest.Options{DCR: true})
	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
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
	ret, err := srv.Authorize(step["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	code := ret.Query().Get("code")
	rec = do("GET", "/login/done?"+ret.RawQuery, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "You are logged in to Granola") || strings.Contains(rec.Body.String(), code) {
		t.Fatalf("return = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "/connectors/"+url.PathEscape(row["id"].(string))+"?tab=login") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("return page = %s", rec.Body)
	}
	if rec = do("GET", "/login/done?"+ret.RawQuery, ""); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "This login is not open") {
		t.Fatalf("second return = %d %s", rec.Code, rec.Body)
	}
	rec = do("GET", "/v1/connectors/"+row["id"].(string), "")
	_ = json.Unmarshal(rec.Body.Bytes(), &row)
	if l, _ := row["login"].(map[string]any); l["state"] != "connected" {
		t.Fatalf("row after login = %v", row["login"])
	}
}
