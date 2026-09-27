package api_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/registry"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/store"
)

const adtestServers = `{
  "servers": [
    {"server": {
      "$schema": "https://static.modelcontextprotocol.io/schemas/2025-09-08/server.schema.json",
      "name": "ai.adtest/adtest-mcp",
      "description": "Ad testing MCP server.",
      "version": "1.1.0",
      "packages": [{
        "registryType": "npm",
        "identifier": "adtest-mcp",
        "version": "1.1.0",
        "runtimeHint": "npx",
        "transport": {"type": "stdio"},
        "environmentVariables": [
          {"name": "ADTEST_API_KEY", "isRequired": true, "isSecret": true}
        ]
      }]
    }, "_meta": {}}
  ],
  "metadata": {"nextCursor": "", "count": 1}
}`

const adtestEntry = `{
  "server": {
    "$schema": "https://static.modelcontextprotocol.io/schemas/2025-09-08/server.schema.json",
    "name": "ai.adtest/adtest-mcp",
    "description": "Ad testing MCP server.",
    "version": "1.1.0",
    "packages": [{
      "registryType": "npm",
      "identifier": "adtest-mcp",
      "version": "1.1.0",
      "runtimeHint": "npx",
      "transport": {"type": "stdio"},
      "environmentVariables": [
        {"name": "ADTEST_API_KEY", "description": "key", "format": "string", "isRequired": true, "isSecret": true},
        {"name": "ADTEST_API_BASE", "format": "string"}
      ]
    }]
  },
  "_meta": {}
}`

const cargoEntry = `{
  "server": {
    "$schema": "https://static.modelcontextprotocol.io/schemas/2025-09-08/server.schema.json",
    "name": "acme/cargo",
    "description": "Cargo MCP.",
    "version": "1.0.0",
    "packages": [{"registryType": "cargo", "identifier": "acme-mcp", "version": "1.0.0", "transport": {"type": "stdio"}}]
  },
  "_meta": {}
}`

// contributedEntry carries a Trebi _meta block: a contributed skill plus the
// CLI action that skill backs. base is the fixture server URL.
func contributedEntry(base string) string {
	return `{
  "server": {
    "$schema": "https://static.modelcontextprotocol.io/schemas/2025-09-08/server.schema.json",
    "name": "acme/contributed",
    "description": "Contributed skill server.",
    "version": "1.0.0",
    "packages": [{"registryType": "npm", "identifier": "contributed-mcp", "version": "1.0.0", "runtimeHint": "npx", "transport": {"type": "stdio"}}],
    "_meta": {"ai.trebi/connector": {
      "skill": {"name": "contributed", "url": "` + base + `/skill.md"},
      "actions": {"cli": {"commands": ["contributed-cli"]}}
    }}
  },
  "_meta": {}
}`
}

const contributedSkillMD = `---
name: contributed
description: Drive the contributed service.
---

Run contributed-cli.
`

// newFixtureRegistry serves the catalog fixtures used by the route tests.
func newFixtureRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v0.1/servers", func(w http.ResponseWriter, _ *http.Request) {
		writeFixtureJSON(w, adtestServers)
	})
	mux.HandleFunc("/skill.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = w.Write([]byte(contributedSkillMD))
	})
	mux.HandleFunc("/v0.1/servers/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.EscapedPath(), "/v0.1/servers/")
		nameEsc, _, _ := strings.Cut(rest, "/versions/")
		name, _ := url.PathUnescape(nameEsc)
		switch name {
		case "ai.adtest/adtest-mcp":
			writeFixtureJSON(w, adtestEntry)
		case "acme/cargo":
			writeFixtureJSON(w, cargoEntry)
		case "acme/contributed":
			writeFixtureJSON(w, contributedEntry("http://"+r.Host))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeFixtureJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// startCatalogAPI builds a daemon whose registry client points at the fixture
// server, and returns the base URL.
func startCatalogAPI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	reg := registry.New(registry.Config{Registries: []config.Registry{
		{Name: "official", URL: newFixtureRegistry(t).URL},
	}})
	s := api.New(st, api.Options{Registry: reg})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	return "http://" + addr
}

func TestConnectorsCatalogSearch(t *testing.T) {
	base := startCatalogAPI(t)
	code, body, _ := doJSON(t, "GET", base+"/v1/connectors/catalog?q=adtest&registry=official", nil)
	if code != 200 {
		t.Fatalf("status %d %v", code, body)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %v", body)
	}
	first, _ := entries[0].(map[string]any)
	if first["name"] != "ai.adtest/adtest-mcp" || first["registry"] != "official" {
		t.Fatalf("entry = %v", first)
	}
}

func TestConnectorsCatalogEntryEnvFlags(t *testing.T) {
	base := startCatalogAPI(t)
	code, body, _ := doJSON(t, "GET", base+"/v1/connectors/catalog/entry?registry=official&name=ai.adtest%2Fadtest-mcp&version=1.1.0", nil)
	if code != 200 {
		t.Fatalf("status %d %v", code, body)
	}
	env, _ := body["env"].([]any)
	if len(env) != 2 {
		t.Fatalf("env = %v", body["env"])
	}
	key, _ := env[0].(map[string]any)
	if key["name"] != "ADTEST_API_KEY" || key["required"] != true || key["secret"] != true {
		t.Fatalf("key = %v", key)
	}
}

func TestConnectorsCatalogEntryMissingName(t *testing.T) {
	base := startCatalogAPI(t)
	code, body, _ := doJSON(t, "GET", base+"/v1/connectors/catalog/entry?registry=official", nil)
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("status %d %v", code, body)
	}
}

func TestConnectorsInstallSuccess(t *testing.T) {
	base := startCatalogAPI(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "ai.adtest/adtest-mcp", "version": "1.1.0", "as": "adtest",
		"package": 0, "env": map[string]any{"ADTEST_API_KEY": "key-1"},
	})
	if code != 201 {
		t.Fatalf("status %d %v", code, body)
	}
	if body["name"] != "adtest" {
		t.Fatalf("row = %v", body)
	}
	actions, _ := body["actions"].(map[string]any)
	mcp, _ := actions["mcp"].(map[string]any)
	if mcp["command"] != "npx" {
		t.Fatalf("mcp = %v", mcp)
	}
	if mcp["env"].(map[string]any)["ADTEST_API_KEY"] != "secret:adtest_ADTEST_API_KEY" {
		t.Fatalf("mcp env = %v", mcp["env"])
	}
	secret, err := secrets.Get("adtest_ADTEST_API_KEY")
	if err != nil || string(secret) != "key-1" {
		t.Fatalf("secret = %q err %v", secret, err)
	}
	// The connector is readable at its own route.
	code, _, _ = doJSON(t, "GET", base+"/v1/connectors/adtest", nil)
	if code != 200 {
		t.Fatalf("get status %d", code)
	}
}

// TestConnectorsInstallSecretRollback proves a create failure leaves the
// secret store as it was: a prior value is restored, a new one is removed.
func TestConnectorsInstallSecretRollback(t *testing.T) {
	base := startCatalogAPI(t)
	// Occupy the name so connectors.Create returns 409 after the secret write.
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors", map[string]any{
		"name": "adtest", "actions": map[string]any{"mcp": map[string]any{"command": "npx"}},
	})
	if code != 201 {
		t.Fatalf("seed connector %d %v", code, body)
	}
	if err := secrets.Set("adtest_ADTEST_API_KEY", "old"); err != nil {
		t.Fatal(err)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "ai.adtest/adtest-mcp", "as": "adtest",
		"package": 0, "env": map[string]any{"ADTEST_API_KEY": "new"},
	})
	if code != 409 {
		t.Fatalf("status %d %v", code, body)
	}
	secret, err := secrets.Get("adtest_ADTEST_API_KEY")
	if err != nil || string(secret) != "old" {
		t.Fatalf("secret after rollback = %q err %v", secret, err)
	}

	// A secret that did not exist is deleted on rollback.
	if err := secrets.Delete("adtest_ADTEST_API_KEY"); err != nil {
		t.Fatal(err)
	}
	code, _, _ = doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "ai.adtest/adtest-mcp", "as": "adtest",
		"package": 0, "env": map[string]any{"ADTEST_API_KEY": "new"},
	})
	if code != 409 {
		t.Fatalf("status %d", code)
	}
	if _, err := secrets.Get("adtest_ADTEST_API_KEY"); err == nil {
		t.Fatal("secret must be removed on rollback")
	}
}

func TestConnectorsInstallBadRequests(t *testing.T) {
	base := startCatalogAPI(t)
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing name", map[string]any{"registry": "official"}, 400},
		{"missing required env", map[string]any{"registry": "official", "name": "ai.adtest/adtest-mcp", "package": 0}, 400},
		{"unsupported package", map[string]any{"registry": "official", "name": "acme/cargo", "package": 0}, 400},
		{"unknown registry", map[string]any{"registry": "nope", "name": "ai.adtest/adtest-mcp"}, 400},
		{"unknown entry", map[string]any{"registry": "official", "name": "acme/missing"}, 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", tc.body)
			if code != tc.want {
				t.Fatalf("status %d %v", code, body)
			}
		})
	}
	// A missing required variable names the variable.
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "ai.adtest/adtest-mcp", "package": 0,
	})
	if code != 400 || !strings.Contains(body["error"].(map[string]any)["message"].(string), "ADTEST_API_KEY") {
		t.Fatalf("status %d %v", code, body)
	}
}

func TestConnectorsInstallUnknownField(t *testing.T) {
	base := startCatalogAPI(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "ai.adtest/adtest-mcp", "bogus": 1,
	})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("status %d %v", code, body)
	}
}

// TestConfigGetIncludesRegistries proves the GET /v1/config shape carries the
// default registry list.
func TestConfigGetIncludesRegistries(t *testing.T) {
	_, base, _ := startAPI(t)
	code, body, _ := doJSON(t, "GET", base+"/v1/config", nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	cfg, _ := body["config"].(map[string]any)
	conns, _ := cfg["connectors"].(map[string]any)
	regs, _ := conns["registries"].([]any)
	if len(regs) != 1 {
		t.Fatalf("registries = %v", conns)
	}
	first, _ := regs[0].(map[string]any)
	if first["name"] != "official" || first["url"] != "https://registry.modelcontextprotocol.io" {
		t.Fatalf("registry = %v", first)
	}
}

// TestConnectorsInstallWritesContributedSkill proves an entry that
// contributes a skill writes it with connector provenance, points the CLI
// action at it, and loses it again when the connector is deleted (Part H.4).
func TestConnectorsInstallWritesContributedSkill(t *testing.T) {
	base := startCatalogAPI(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "acme/contributed", "as": "contributed", "package": 0,
	})
	if code != 201 {
		t.Fatalf("status %d %v", code, body)
	}
	skill, _ := body["skill"].(map[string]any)
	if skill == nil || skill["name"] != "contributed" || skill["from_connector"] != "contributed" {
		t.Fatalf("skill = %v", body["skill"])
	}
	actions, _ := body["actions"].(map[string]any)
	cli, _ := actions["cli"].(map[string]any)
	if cli["skill"] != "contributed" {
		t.Fatalf("cli = %v", cli)
	}
	mdPath := filepath.Join(home.SkillsDir(), "contributed", "SKILL.md")
	md, err := os.ReadFile(mdPath)
	if err != nil || !strings.Contains(string(md), "contributed-cli") {
		t.Fatalf("skill body %q err %v", md, err)
	}
	code, listed, _ := doJSON(t, "GET", base+"/v1/skills", nil)
	if code != 200 {
		t.Fatalf("skills status %d", code)
	}
	found := false
	for _, raw := range listed["skills"].([]any) {
		sk, _ := raw.(map[string]any)
		if sk["name"] == "contributed" && sk["provenance"] == "connector" && sk["from_connector"] == "contributed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("skills = %v", listed["skills"])
	}
	code, body, _ = doJSON(t, "DELETE", base+"/v1/connectors/contributed?confirm=1", nil)
	if code != 200 && code != 204 {
		t.Fatalf("delete status %d %v", code, body)
	}
	if _, err := os.Stat(mdPath); !os.IsNotExist(err) {
		t.Fatalf("skill body survives the connector delete: %v", err)
	}
}
