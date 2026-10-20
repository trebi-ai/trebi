package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
)

// TestConnectorKeyLoginActor proves that the assistant never gets the
// field of a key method: the begin names the form, and a submit is 403.
// A person on a paired phone gets the flow.
func TestConnectorKeyLoginActor(t *testing.T) {
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
	assistant := &tools.Actor{Kind: "assistant", Name: "assistant"}
	phone := &tools.Actor{Kind: tools.ActorMobile, Name: "phone", Scopes: []string{"full"}}
	do := func(actor *tools.Actor, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if actor != nil {
			req = req.WithContext(tools.WithActor(req.Context(), *actor))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	entry := `{"type":"apollo","slug":"apollo","from":"inline","manifest":{"schema":"trebi-connector/1","name":"apollo","title":"Apollo","description":"d","version":"1.0.0",
		"actions":{"cli":{"commands":["sh"]}},
		"setup":{"inputs":[{"name":"APOLLO_KEY","secret":true}],"login":[{"id":"key","type":"key","input":"APOLLO_KEY"}]}}}`
	if code, body := do(nil, "POST", "/v1/connectors", entry); code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, body)
	}

	code, body := do(assistant, "GET", "/v1/connectors/apollo/login/methods", "")
	if methods, _ := body["methods"].([]any); code != http.StatusOK || len(methods) != 1 {
		t.Fatalf("methods = %d %v", code, body)
	}
	code, body = do(assistant, "POST", "/v1/connectors/apollo/login", `{}`)
	if code != http.StatusOK || body["flow_id"] != nil || !strings.Contains(body["form"].(string), "tab=login&method=key") {
		t.Fatalf("assistant begin = %d %v", code, body)
	}

	code, body = do(phone, "POST", "/v1/connectors/apollo/login", `{"method":"key"}`)
	flow, _ := body["flow_id"].(string)
	if code != http.StatusOK || flow == "" {
		t.Fatalf("phone begin = %d %v", code, body)
	}
	code, body = do(assistant, "POST", "/v1/connectors/apollo/login/"+flow, `{"fields":{"APOLLO_KEY":"k"}}`)
	if code != http.StatusForbidden || body["error"].(map[string]any)["code"] != "use_login_form" {
		t.Fatalf("assistant submit = %d %v", code, body)
	}
	code, body = do(phone, "POST", "/v1/connectors/apollo/login/"+flow, `{"fields":{"APOLLO_KEY":"k"}}`)
	if done, _ := body["done"].(map[string]any); code != http.StatusOK || done["ok"] != true {
		t.Fatalf("phone submit = %d %v", code, body)
	}
}
