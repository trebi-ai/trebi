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
	text := "defaults:\n  command: claude\n"
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

	// The loader rejects a flat key; the masked view reads the raw file.
	withKey := text + "assistant:\n  api_key: sk-live-123\n"
	if err := os.WriteFile(cfgPath, []byte(withKey), 0o644); err != nil {
		t.Fatal(err)
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

// TestSecretsOwners checks that a harness secret lists with its owner and
// that a user write or delete of it is refused.
func TestSecretsOwners(t *testing.T) {
	_, base := startTestServer(t)
	code, row, _ := doJSON(t, "POST", base+"/v1/harnesses", map[string]any{
		"name": "claude-zai", "base": "claude", "label": "Claude Z.ai",
		"env": map[string]any{"ANTHROPIC_AUTH_TOKEN": map[string]any{"value": "tok-123", "secret": true}},
	})
	if code != 201 {
		t.Fatalf("harness create %d %v", code, row)
	}
	if code, body, _ := doJSON(t, "PUT", base+"/v1/secrets/MY_TOKEN", map[string]any{"value": "v"}); code/100 != 2 {
		t.Fatalf("user secret put %d %v", code, body)
	}
	const owned = "harness.claude-zai.ANTHROPIC_AUTH_TOKEN"
	code, body, _ := doJSON(t, "GET", base+"/v1/secrets", nil)
	if code != 200 {
		t.Fatalf("list %d %v", code, body)
	}
	owners, _ := body["owners"].(map[string]any)
	o, _ := owners[owned].(map[string]any)
	if o["kind"] != "harness" || o["label"] != "Claude Z.ai" || o["link"] != "/settings/harnesses?configure=claude-zai" {
		t.Fatalf("owner of %s = %v", owned, owners)
	}
	if _, ok := owners["MY_TOKEN"]; ok {
		t.Fatalf("user secret has an owner: %v", owners)
	}
	code, body, _ = doJSON(t, "PUT", base+"/v1/secrets/"+owned, map[string]any{"value": "x"})
	if msg, _ := body["error"].(map[string]any)["message"].(string); code != 409 || errCode(body) != "conflict" || !strings.Contains(msg, "Claude Z.ai") {
		t.Fatalf("owned put %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "DELETE", base+"/v1/secrets/"+owned+"?confirm=1", nil); code != 409 || errCode(body) != "conflict" {
		t.Fatalf("owned delete %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "DELETE", base+"/v1/secrets/MY_TOKEN?confirm=1", nil); code != 200 {
		t.Fatalf("user delete %d %v", code, body)
	}
}
