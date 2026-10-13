package api_test

import (
	"net/http"
	"reflect"
	"testing"
)

// TestConnectorScopeRoutes proves the scope takes workspace names, stores
// ids, hides the connection outside the scope, and refuses a room bind there.
func TestConnectorScopeRoutes(t *testing.T) {
	do := startConnectorAPI(t)
	ids := map[string]string{}
	for _, name := range []string{"wa", "wb"} {
		code, body := do("POST", "/v1/workspaces", map[string]any{"path": t.TempDir(), "name": name})
		if code != http.StatusCreated {
			t.Fatalf("workspace %d %v", code, body)
		}
		ids[name], _ = body["id"].(string)
	}
	def := inlineEntry("chat", map[string]any{
		"events":  map[string]any{"command": "sleep 30", "protocol": "trebi-connector/1", "types": []map[string]any{{"type": "message"}}},
		"channel": map[string]any{},
	})
	def["scope"] = []string{"wa"}
	if code, body := do("POST", "/v1/connectors", def); code != http.StatusCreated || !reflect.DeepEqual(body["scope"], []any{ids["wa"]}) {
		t.Fatalf("create %d %v", code, body)
	}
	if _, row := do("GET", "/v1/connectors/chat?workspace=wb", nil); row["in_scope"] != false {
		t.Fatalf("row in wb %v", row)
	}
	if _, row := do("GET", "/v1/connectors/chat?workspace=wa", nil); row["in_scope"] != true {
		t.Fatalf("row in wa %v", row)
	}
	if code, body := do("POST", "/v1/connectors/chat/rooms/r1/bind", map[string]any{"workspace": ids["wb"]}); code != http.StatusConflict {
		t.Fatalf("bind out of scope %d %v", code, body)
	}
	if code, body := do("POST", "/v1/connectors/chat/rooms/r1/bind", map[string]any{"workspace": ids["wa"]}); code != http.StatusOK {
		t.Fatalf("bind in scope %d %v", code, body)
	}

	if code, body := do("PATCH", "/v1/connectors/chat", map[string]any{"scope": []string{"nosuch"}}); code != http.StatusBadRequest {
		t.Fatalf("unknown workspace %d %v", code, body)
	}
	if code, body := do("PATCH", "/v1/connectors/chat", map[string]any{"scope": []string{}}); code != http.StatusBadRequest {
		t.Fatalf("empty scope %d %v", code, body)
	}
	if code, body := do("PATCH", "/v1/connectors/chat", map[string]any{"scope": []string{"wa", ids["wb"]}}); code != http.StatusOK ||
		!reflect.DeepEqual(body["scope"], []any{ids["wa"], ids["wb"]}) {
		t.Fatalf("names and ids %d %v", code, body)
	}
	if code, body := do("PATCH", "/v1/connectors/chat", map[string]any{"scope": "global"}); code != http.StatusOK || !reflect.DeepEqual(body["scope"], []any{"global"}) {
		t.Fatalf("global %d %v", code, body)
	}
}
