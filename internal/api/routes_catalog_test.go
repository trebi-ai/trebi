package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// hostileEntry declares an env name and a skill name that would reach
// outside ~/.trebi if the install trusted them.
func hostileEntry(base, envName, skillName string) string {
	return `{
  "server": {
    "$schema": "https://static.modelcontextprotocol.io/schemas/2025-09-08/server.schema.json",
    "name": "acme/hostile",
    "version": "1.0.0",
    "packages": [{"registryType": "npm", "identifier": "hostile-mcp", "version": "1.0.0", "transport": {"type": "stdio"},
      "environmentVariables": [{"name": "` + envName + `", "isSecret": true}]}],
    "_meta": {"ai.trebi/connector": {
      "skill": {"name": "` + skillName + `", "url": "` + base + `/skill.md"},
      "actions": {"cli": {"commands": ["hostile-cli"]}}
    }}
  },
  "_meta": {}
}`
}

// remoteAuthEntry is a remote whose Authorization header takes a secret
// variable.
const remoteAuthEntry = `{
  "server": {
    "name": "acme/remote-auth",
    "version": "1.0.0",
    "remotes": [{
      "type": "streamable-http",
      "url": "https://mcp.acme.example/{tenant}/mcp",
      "variables": {"tenant": {"isRequired": true}},
      "headers": [{"name": "Authorization", "value": "Bearer {api_key}",
        "variables": {"api_key": {"isRequired": true, "isSecret": true}}}]
    }]
  },
  "_meta": {}
}`

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
		case "acme/hostile-env":
			writeFixtureJSON(w, hostileEntry("http://"+r.Host, "../../x", "hostile"))
		case "acme/hostile-skill":
			writeFixtureJSON(w, hostileEntry("http://"+r.Host, "TOKEN", "../y"))
		case "acme/remote-auth":
			writeFixtureJSON(w, remoteAuthEntry)
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
// server, and returns the base URL. Its skill client may reach the loopback
// fixture.
func startCatalogAPI(t *testing.T) string {
	t.Helper()
	return startCatalogAPIWith(t, func(fixture string) registry.Config {
		return registry.Config{
			Registries: []config.Registry{{Name: "official", URL: fixture}},
			SkillHTTP:  &http.Client{Timeout: 5 * time.Second},
		}
	})
}

// startCatalogAPIWith builds the daemon with the registry config that cfg
// returns for the fixture server URL.
func startCatalogAPIWith(t *testing.T, cfg func(fixture string) registry.Config) string {
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
	reg := registry.New(cfg(newFixtureRegistry(t).URL))
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
		"package": 0, "env": map[string]any{"ADTEST_API_KEY": "new"}, "overwrite_secrets": true,
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

// TestConnectorsInstallRefusesUnsafeNames proves registry data cannot name a
// file outside ~/.trebi: a bad env name or skill name returns 400 and the
// install writes no file.
func TestConnectorsInstallRefusesUnsafeNames(t *testing.T) {
	base := startCatalogAPI(t)
	for _, name := range []string{"acme/hostile-env", "acme/hostile-skill"} {
		code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
			"registry": "official", "name": name, "as": "hostile", "package": 0,
			"env": map[string]any{"../../x": "v", "TOKEN": "v"},
		})
		if code != 400 || errCode(body) != "validation" {
			t.Fatalf("%s: status %d %v", name, code, body)
		}
	}
	for _, dir := range []string{home.SecretsDir(), home.SkillsDir()} {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("%s holds %v", dir, entries)
		}
	}
	for _, p := range []string{
		filepath.Join(home.Base(), "x"), filepath.Join(home.SecretsDir(), "x"),
		filepath.Join(filepath.Dir(home.Base()), "x"), filepath.Join(filepath.Dir(home.SkillsDir()), "y"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("install wrote %s", p)
		}
	}
	if _, err := os.Stat(home.ConnectorsPath()); !os.IsNotExist(err) {
		t.Fatalf("connectors.yaml written: %v", err)
	}
}

// TestConnectorsInstallSecretReference proves a secret: value is stored as
// a reference, with no new secret and no owned secret (P1-32).
func TestConnectorsInstallSecretReference(t *testing.T) {
	base := startCatalogAPI(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "ai.adtest/adtest-mcp", "as": "adtest",
		"package": 0, "env": map[string]any{"ADTEST_API_KEY": "secret:shared_key"},
	})
	if code != 201 {
		t.Fatalf("status %d %v", code, body)
	}
	mcp := body["actions"].(map[string]any)["mcp"].(map[string]any)
	if mcp["env"].(map[string]any)["ADTEST_API_KEY"] != "secret:shared_key" {
		t.Fatalf("env = %v", mcp["env"])
	}
	if names, _ := secrets.List(); len(names) != 0 {
		t.Fatalf("secrets = %v", names)
	}
	if src, _ := body["source"].(map[string]any); src["secrets"] != nil {
		t.Fatalf("source = %v", src)
	}
}

// TestConnectorsInstallSecretConflict proves a stored secret with another
// value is a conflict unless the body sets overwrite_secrets, and that the
// same value needs no write.
func TestConnectorsInstallSecretConflict(t *testing.T) {
	base := startCatalogAPI(t)
	if err := secrets.Set("adtest_ADTEST_API_KEY", "old"); err != nil {
		t.Fatal(err)
	}
	install := func(value string, overwrite bool) (int, map[string]any) {
		code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
			"registry": "official", "name": "ai.adtest/adtest-mcp", "as": "adtest",
			"package": 0, "env": map[string]any{"ADTEST_API_KEY": value}, "overwrite_secrets": overwrite,
		})
		return code, body
	}
	if code, body := install("new", false); code != 409 || errCode(body) != "conflict" {
		t.Fatalf("status %d %v", code, body)
	}
	if v, _ := secrets.Get("adtest_ADTEST_API_KEY"); string(v) != "old" {
		t.Fatalf("secret = %q", v)
	}
	code, body := install("new", true)
	if code != 201 {
		t.Fatalf("status %d %v", code, body)
	}
	if v, _ := secrets.Get("adtest_ADTEST_API_KEY"); string(v) != "new" {
		t.Fatalf("secret = %q", v)
	}
	// The secret existed before, so the connector does not own it.
	if src, _ := body["source"].(map[string]any); src["secrets"] != nil {
		t.Fatalf("source = %v", src)
	}
	if code, _, _ := doJSON(t, "DELETE", base+"/v1/connectors/adtest?confirm=1", nil); code != 200 && code != 204 {
		t.Fatalf("delete %d", code)
	}
	if _, err := secrets.Get("adtest_ADTEST_API_KEY"); err != nil {
		t.Fatal("a secret the install did not create must survive the delete")
	}
	if code, body := install("new", false); code != 201 {
		t.Fatalf("same value: status %d %v", code, body)
	}
}

// TestConnectorsDeleteRemovesOwnedSecrets proves a connector delete removes
// the secrets its install wrote, unless another connector refers to one.
func TestConnectorsDeleteRemovesOwnedSecrets(t *testing.T) {
	base := startCatalogAPI(t)
	install := func() {
		t.Helper()
		code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
			"registry": "official", "name": "ai.adtest/adtest-mcp", "as": "adtest",
			"package": 0, "env": map[string]any{"ADTEST_API_KEY": "key-1"},
		})
		if code != 201 {
			t.Fatalf("status %d %v", code, body)
		}
		src, _ := body["source"].(map[string]any)
		if got, _ := src["secrets"].([]any); len(got) != 1 || got[0] != "adtest_ADTEST_API_KEY" {
			t.Fatalf("source = %v", src)
		}
	}
	install()
	if code, _, _ := doJSON(t, "DELETE", base+"/v1/connectors/adtest?confirm=1", nil); code != 200 && code != 204 {
		t.Fatalf("delete %d", code)
	}
	if _, err := secrets.Get("adtest_ADTEST_API_KEY"); err == nil {
		t.Fatal("the owned secret must be removed")
	}

	install()
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors", map[string]any{
		"name": "other", "actions": map[string]any{"mcp": map[string]any{
			"command": "npx", "env": map[string]any{"KEY": "secret:adtest_ADTEST_API_KEY"},
		}},
	})
	if code != 201 {
		t.Fatalf("other %d %v", code, body)
	}
	if code, _, _ := doJSON(t, "DELETE", base+"/v1/connectors/adtest?confirm=1", nil); code != 200 && code != 204 {
		t.Fatalf("delete %d", code)
	}
	if _, err := secrets.Get("adtest_ADTEST_API_KEY"); err != nil {
		t.Fatal("a secret another connector refers to must survive")
	}
}

func TestConnectorsInstallPackageAndRemote(t *testing.T) {
	base := startCatalogAPI(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "ai.adtest/adtest-mcp", "package": 0, "remote": 0,
	})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("status %d %v", code, body)
	}
}

// TestConnectorsInstallRemoteInputs proves a remote install fills its URL
// variable and its secret header from env (P1-33).
func TestConnectorsInstallRemoteInputs(t *testing.T) {
	base := startCatalogAPI(t)
	code, body, _ := doJSON(t, "GET", base+"/v1/connectors/catalog/entry?registry=official&name=acme%2Fremote-auth", nil)
	if code != 200 {
		t.Fatalf("entry %d %v", code, body)
	}
	remotes, _ := body["remote_inputs"].([]any)
	if len(remotes) != 1 || len(remotes[0].([]any)) != 2 {
		t.Fatalf("remote_inputs = %v", body["remote_inputs"])
	}
	if pk, _ := body["package_inputs"].([]any); len(pk) != 0 {
		t.Fatalf("package_inputs = %v", body["package_inputs"])
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "acme/remote-auth", "as": "remote",
		"env": map[string]any{"tenant": "t1", "api_key": "k-1"},
	})
	if code != 201 {
		t.Fatalf("status %d %v", code, body)
	}
	mcp := body["actions"].(map[string]any)["mcp"].(map[string]any)
	if mcp["url"] != "https://mcp.acme.example/t1/mcp" {
		t.Fatalf("mcp = %v", mcp)
	}
	if mcp["headers"].(map[string]any)["Authorization"] != "secret:remote_Authorization" {
		t.Fatalf("headers = %v", mcp["headers"])
	}
	if v, err := secrets.Get("remote_Authorization"); err != nil || string(v) != "Bearer k-1" {
		t.Fatalf("secret = %q err %v", v, err)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "acme/remote-auth", "as": "remote2",
		"env": map[string]any{"tenant": "t1", "api_key": "k", "bogus": "x"},
	})
	if code != 400 || !strings.Contains(fmt.Sprint(body), "bogus") {
		t.Fatalf("unused input: status %d %v", code, body)
	}
}

// TestConnectorsInstallRefusesUnsafeSkillURL proves the default skill client
// refuses an http URL, and the install leaves nothing behind (P1-35).
func TestConnectorsInstallRefusesUnsafeSkillURL(t *testing.T) {
	base := startCatalogAPIWith(t, func(fixture string) registry.Config {
		return registry.Config{Registries: []config.Registry{{Name: "official", URL: fixture}}}
	})
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "acme/contributed", "as": "contributed", "package": 0,
	})
	if code != 502 || !strings.Contains(fmt.Sprint(body), "https required") {
		t.Fatalf("status %d %v", code, body)
	}
	if code, _, _ := doJSON(t, "GET", base+"/v1/connectors/contributed", nil); code != 404 {
		t.Fatalf("connector survives the failed install: %d", code)
	}
}

func TestConnectorsInstallSkillExists(t *testing.T) {
	base := startCatalogAPI(t)
	if err := os.MkdirAll(filepath.Join(home.SkillsDir(), "contributed"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "acme/contributed", "as": "contributed", "package": 0,
	})
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("status %d %v", code, body)
	}
	if code, _, _ := doJSON(t, "GET", base+"/v1/connectors/contributed", nil); code != 404 {
		t.Fatalf("connector survives the failed install: %d", code)
	}
}

// TestConnectorsCatalogPartialFailure proves the reply lists the configured
// registries and reports a failing one while the others answer (P1-37).
func TestConnectorsCatalogPartialFailure(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	base := startCatalogAPIWith(t, func(fixture string) registry.Config {
		return registry.Config{Registries: []config.Registry{
			{Name: "official", URL: fixture}, {Name: "down", URL: down.URL},
		}}
	})
	code, body, _ := doJSON(t, "GET", base+"/v1/connectors/catalog", nil)
	if code != 200 {
		t.Fatalf("status %d %v", code, body)
	}
	if entries, _ := body["entries"].([]any); len(entries) != 1 {
		t.Fatalf("entries = %v", body["entries"])
	}
	regs, _ := body["registries"].([]any)
	if len(regs) != 2 || regs[1].(map[string]any)["name"] != "down" {
		t.Fatalf("registries = %v", body["registries"])
	}
	errs, _ := body["registry_errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["registry"] != "down" || errs[0].(map[string]any)["error"] == "" {
		t.Fatalf("registry_errors = %v", body["registry_errors"])
	}
}

// TestConnectorsCatalogFollowsConfig proves the catalog client follows a
// config change while requests run. Run it with -race.
func TestConnectorsCatalogFollowsConfig(t *testing.T) {
	_, base, dir := startAPI(t)
	fixture := newFixtureRegistry(t).URL
	write := func(name string) {
		yaml := "connectors:\n  registries:\n    - { name: " + name + ", url: " + fixture + " }\n"
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("one")
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				resp, err := http.Get(base + "/v1/connectors/catalog")
				if err == nil {
					resp.Body.Close()
				}
			}
		}()
		if i%2 == 0 {
			write("two")
		} else {
			write("one")
		}
	}
	wg.Wait()
	write("final")
	code, body, _ := doJSON(t, "GET", base+"/v1/connectors/catalog", nil)
	regs, _ := body["registries"].([]any)
	if code != 200 || len(regs) != 1 || regs[0].(map[string]any)["name"] != "final" {
		t.Fatalf("status %d registries %v", code, body["registries"])
	}
}
