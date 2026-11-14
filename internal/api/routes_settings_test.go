package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigSchemaDraftYAML covers plan Part F.1: the schema rows, a draft
// check that writes nothing, and the masked YAML view.
func TestConfigSchemaDraftYAML(t *testing.T) {
	_, base, dir := startAPI(t)
	cfgPath := filepath.Join(dir, "config.yaml")
	text := "defaults:\n  command: claude\nassistant:\n  api_key: sk-live-123\n"
	if err := os.WriteFile(cfgPath, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body, _ := doJSON(t, "GET", base+"/v1/config/schema", nil)
	if code != 200 {
		t.Fatalf("schema %d %v", code, body)
	}
	keys, _ := body["keys"].([]any)
	byKey := map[string]map[string]any{}
	for _, k := range keys {
		m, _ := k.(map[string]any)
		byKey[m["key"].(string)] = m
	}
	if r := byKey["notify.route"]; r == nil || r["type"] != "json" || r["section"] != "notify" {
		t.Fatalf("notify.route = %v", r)
	}
	if r := byKey["api.port"]; r == nil || r["type"] != "int" || r["restart"] != true || r["default"] != "7433" {
		t.Fatalf("api.port = %v", r)
	}
	if r := byKey["assistant.api_key"]; r == nil || r["secret"] != true {
		t.Fatalf("assistant.api_key = %v", r)
	}
	if secs, _ := body["sections"].([]any); len(secs) != 20 {
		t.Fatalf("sections = %v", body["sections"])
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/config/validate", map[string]any{
		"set": map[string]any{"notify.route": []any{map[string]any{"via": "email", "to": "admins"}}},
	})
	if code != 200 || body["ok"] != true {
		t.Fatalf("good draft %d %v", code, body)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/config/validate", map[string]any{
		"set": map[string]any{"api.port": 70000, "terminal": "xterm"},
	})
	errs, _ := body["errors"].([]any)
	if code != 200 || body["ok"] != false || len(errs) != 2 {
		t.Fatalf("bad draft %d %v", code, body)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/config/validate", map[string]any{"yaml": "bogus: 1\n"})
	if code != 200 || body["ok"] != false {
		t.Fatalf("bad yaml %d %v", code, body)
	}
	if b, _ := os.ReadFile(cfgPath); string(b) != text {
		t.Fatalf("draft wrote config.yaml: %q", b)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/config/yaml", nil)
	if code != 200 {
		t.Fatalf("yaml %d %v", code, body)
	}
	y, _ := body["yaml"].(string)
	if strings.Contains(y, "sk-live-123") || !strings.Contains(y, "***") || body["hash"] == "" {
		t.Fatalf("yaml = %v", body)
	}
}
