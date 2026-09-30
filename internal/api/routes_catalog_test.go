package api_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// hostileEntry declares an env name that would reach outside ~/.trebi if
// the install trusted it.
const hostileEntry = `{
  "server": {
    "$schema": "https://static.modelcontextprotocol.io/schemas/2025-09-08/server.schema.json",
    "name": "acme/hostile",
    "version": "1.0.0",
    "packages": [{"registryType": "npm", "identifier": "hostile-mcp", "version": "1.0.0", "transport": {"type": "stdio"},
      "environmentVariables": [{"name": "../../x", "isSecret": true}]}]
  },
  "_meta": {}
}`

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

// TestConnectorsInstallSuccess proves an MCP registry install creates an
// inline connection whose secret input is owned by its folder.
func TestConnectorsInstallSuccess(t *testing.T) {
	base := startCatalogAPI(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "ai.adtest/adtest-mcp", "version": "1.1.0", "slug": "adtest",
		"title": "Ad test", "package": 0, "inputs": map[string]any{"ADTEST_API_KEY": "key-1"},
	})
	if code != 201 {
		t.Fatalf("status %d %v", code, body)
	}
	id, _ := body["id"].(string)
	if body["slug"] != "adtest" || body["title"] != "Ad test" || !strings.HasPrefix(id, "con_") {
		t.Fatalf("row = %v", body)
	}
	if body["from"] != "inline" {
		t.Fatalf("from = %v", body["from"])
	}
	mcp := installedMCP(t, body)
	if mcp["command"] != "npx" || mcp["env"].(map[string]any)["ADTEST_API_KEY"] != "${ADTEST_API_KEY}" {
		t.Fatalf("mcp = %v", mcp)
	}
	if in := body["inputs"].(map[string]any); in["ADTEST_API_KEY"] != "***" {
		t.Fatalf("inputs = %v", in)
	}
	if v := ownedSecret(t, id, "ADTEST_API_KEY"); v != "key-1" {
		t.Fatalf("owned secret = %q", v)
	}
	if names, _ := secrets.List(); len(names) != 0 {
		t.Fatalf("user secrets = %v", names)
	}
	for _, ref := range []string{"adtest", id} {
		if code, _, _ := doJSON(t, "GET", base+"/v1/connectors/"+ref, nil); code != 200 {
			t.Fatalf("get %s status %d", ref, code)
		}
	}
}

// TestConnectorsInstallConflictKeepsSecrets proves an install onto a taken
// slug fails before it writes anything: no new folder and no secret.
func TestConnectorsInstallConflictKeepsSecrets(t *testing.T) {
	base := startCatalogAPI(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors", inlineEntry("adtest", map[string]any{
		"actions": map[string]any{"mcp": map[string]any{"command": "npx"}},
	}))
	if code != 201 {
		t.Fatalf("seed connector %d %v", code, body)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "ai.adtest/adtest-mcp", "slug": "adtest",
		"package": 0, "inputs": map[string]any{"ADTEST_API_KEY": "new"},
	})
	if code != 409 {
		t.Fatalf("status %d %v", code, body)
	}
	if entries, _ := os.ReadDir(home.ConnectionsDir()); len(entries) != 1 {
		t.Fatalf("connection folders = %v", entries)
	}
	if names, _ := secrets.List(); len(names) != 0 {
		t.Fatalf("user secrets = %v", names)
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
// default registry list: the Trebi catalog first.
func TestConfigGetIncludesRegistries(t *testing.T) {
	_, base, _ := startAPI(t)
	code, body, _ := doJSON(t, "GET", base+"/v1/config", nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	cfg, _ := body["config"].(map[string]any)
	conns, _ := cfg["connectors"].(map[string]any)
	regs, _ := conns["registries"].([]any)
	want := []string{"trebi", "official", "docker"}
	if len(regs) != len(want) {
		t.Fatalf("registries = %v", conns)
	}
	for i, name := range want {
		if got := regs[i].(map[string]any)["name"]; got != name {
			t.Fatalf("registry %d = %v; want %s", i, got, name)
		}
	}
}

// TestConnectorsInstallRefusesUnsafeNames proves registry data cannot name a
// file outside ~/.trebi: a bad env name returns 400 and the install writes
// no file.
func TestConnectorsInstallRefusesUnsafeNames(t *testing.T) {
	base := startCatalogAPI(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "acme/hostile", "slug": "hostile", "package": 0,
		"inputs": map[string]any{"../../x": "v"},
	})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("status %d %v", code, body)
	}
	if entries, _ := os.ReadDir(home.SecretsDir()); len(entries) != 0 {
		t.Fatalf("secrets dir holds %v", entries)
	}
	for _, p := range []string{
		filepath.Join(home.Base(), "x"), filepath.Join(home.SecretsDir(), "x"), filepath.Join(filepath.Dir(home.Base()), "x"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("install wrote %s", p)
		}
	}
	if entries, _ := os.ReadDir(home.ConnectionsDir()); len(entries) != 0 {
		t.Fatalf("connection folders written: %v", entries)
	}
}

// TestConnectorsInstallSecretReference proves a secret: value is stored as
// a reference, with no new secret and no owned secret (P1-32).
func TestConnectorsInstallSecretReference(t *testing.T) {
	base := startCatalogAPI(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "ai.adtest/adtest-mcp", "slug": "adtest",
		"package": 0, "inputs": map[string]any{"ADTEST_API_KEY": "secret:shared_key"},
	})
	if code != 201 {
		t.Fatalf("status %d %v", code, body)
	}
	if in := body["inputs"].(map[string]any); in["ADTEST_API_KEY"] != "secret:shared_key" {
		t.Fatalf("inputs = %v", in)
	}
	if names, _ := secrets.List(); len(names) != 0 {
		t.Fatalf("secrets = %v", names)
	}
	if owned, _ := os.ReadDir(filepath.Join(home.ConnectionDir(body["id"].(string)), "secrets")); len(owned) != 0 {
		t.Fatalf("owned secrets = %v", owned)
	}
}

// TestConnectorsDeleteRemovesOwnedSecrets proves two installs of one type
// each own their secret, and a delete removes only its own folder.
func TestConnectorsDeleteRemovesOwnedSecrets(t *testing.T) {
	base := startCatalogAPI(t)
	install := func(slug, key string) string {
		t.Helper()
		code, body, _ := doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
			"registry": "official", "name": "ai.adtest/adtest-mcp", "slug": slug,
			"package": 0, "inputs": map[string]any{"ADTEST_API_KEY": key},
		})
		if code != 201 {
			t.Fatalf("status %d %v", code, body)
		}
		return body["id"].(string)
	}
	one, two := install("ad-one", "key-1"), install("ad-two", "key-2")
	if code, _, _ := doJSON(t, "DELETE", base+"/v1/connectors/ad-one?confirm=1", nil); code != 200 && code != 204 {
		t.Fatalf("delete %d", code)
	}
	if _, err := os.Stat(home.ConnectionDir(one)); !os.IsNotExist(err) {
		t.Fatalf("delete kept the folder: %v", err)
	}
	if v := ownedSecret(t, two, "ADTEST_API_KEY"); v != "key-2" {
		t.Fatalf("other secret = %q", v)
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
// variable and its secret header from the inputs (P1-33).
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
		"registry": "official", "name": "acme/remote-auth", "slug": "remote",
		"inputs": map[string]any{"tenant": "t1", "api_key": "k-1"},
	})
	if code != 201 {
		t.Fatalf("status %d %v", code, body)
	}
	mcp := installedMCP(t, body)
	if mcp["url"] != "https://mcp.acme.example/t1/mcp" {
		t.Fatalf("mcp = %v", mcp)
	}
	if mcp["headers"].(map[string]any)["Authorization"] != "${AUTHORIZATION}" {
		t.Fatalf("headers = %v", mcp["headers"])
	}
	if in := body["inputs"].(map[string]any); in["AUTHORIZATION"] != "***" {
		t.Fatalf("inputs = %v", in)
	}
	if v := ownedSecret(t, body["id"].(string), "AUTHORIZATION"); v != "Bearer k-1" {
		t.Fatalf("owned secret = %q", v)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"registry": "official", "name": "acme/remote-auth", "slug": "remote2",
		"inputs": map[string]any{"tenant": "t1", "api_key": "k", "bogus": "x"},
	})
	if code != 400 || !strings.Contains(fmt.Sprint(body), "bogus") {
		t.Fatalf("unused input: status %d %v", code, body)
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

// TestConnectorsTrebiCatalogFlow installs from the signed catalog, then
// lists versions, checks, updates, inspects, and collects.
func TestConnectorsTrebiCatalogFlow(t *testing.T) {
	catURL, pub := trebiCatalogFixture(t)
	base := startCatalogAPIWith(t, func(fixture string) registry.Config {
		return registry.Config{
			Registries:  []config.Registry{{Name: "trebi", URL: catURL, Kind: config.RegistryTrebi}, {Name: "official", URL: fixture}},
			CatalogKeys: []ed25519.PublicKey{pub},
		}
	})
	code, body, _ := doJSON(t, "GET", base+"/v1/connectors/catalog?q=demo&registry=trebi", nil)
	entries, _ := body["entries"].([]any)
	if code != 200 || len(entries) != 1 {
		t.Fatalf("search %d %v", code, body)
	}
	if row, _ := entries[0].(map[string]any); row["registry"] != "trebi" || row["trust"] != "official" || row["publisher"] != "trebi" || row["versions"] == nil {
		t.Fatalf("row = %v", row)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{"registry": "trebi", "name": "demo", "version": "1.0.0", "track": "minor"})
	if code != 201 || body["version"] != "1.0.0" || body["track"] != "minor" {
		t.Fatalf("install %d %v", code, body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/connectors/demo/versions", nil)
	vers, _ := body["versions"].([]any)
	if code != 200 || len(vers) != 2 {
		t.Fatalf("versions %d %v", code, body)
	}
	if last, _ := vers[1].(map[string]any); last["version"] != "1.0.0" || last["installed"] != true || last["current"] != true {
		t.Fatalf("version row = %v", last)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/check-updates", nil)
	res, _ := body["results"].([]any)
	if code != 200 || len(res) != 1 {
		t.Fatalf("check %d %v", code, body)
	}
	if r, _ := res[0].(map[string]any); r["to"] != "1.1.0" || r["applied"] != false {
		t.Fatalf("check row = %v", r)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/connectors/demo", nil)
	if u, _ := body["update"].(map[string]any); code != 200 || u["version"] != "1.1.0" {
		t.Fatalf("row update %d %v", code, body["update"])
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/demo/update", map[string]any{})
	if code != 200 || body["version"] != "1.1.0" {
		t.Fatalf("update %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "POST", base+"/v1/connectors/demo/update", map[string]any{"version": "9.9.9"}); code != 404 {
		t.Fatalf("update to a missing version %d %v", code, body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/connectors/demo/capabilities", nil)
	if code != 200 || body["version"] != "1.1.0" {
		t.Fatalf("capabilities %d %v", code, body)
	}
	if sk, _ := body["skill"].(map[string]any); sk["description"] != "Use sh." {
		t.Fatalf("skill = %v", body["skill"])
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/gc", map[string]any{"dry_run": true})
	if removed, _ := body["removed"].([]any); code != 200 || len(removed) != 0 {
		t.Fatalf("gc %d %v", code, body)
	}
}

// newFixtureRegistry serves the catalog fixtures used by the route tests.
func newFixtureRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v0.1/servers", func(w http.ResponseWriter, _ *http.Request) {
		writeFixtureJSON(w, adtestServers)
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
		case "acme/hostile":
			writeFixtureJSON(w, hostileEntry)
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
// server, and returns the base URL.
func startCatalogAPI(t *testing.T) string {
	t.Helper()
	return startCatalogAPIWith(t, func(fixture string) registry.Config {
		return registry.Config{Registries: []config.Registry{{Name: "official", URL: fixture}}}
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

// ownedSecret reads the owned secret of one input from a connection folder.
func ownedSecret(t *testing.T, id, input string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home.ConnectionDir(id), "secrets", input))
	if err != nil {
		t.Fatalf("owned secret %s: %v", input, err)
	}
	return string(b)
}

// installedMCP returns actions.mcp of the resolved manifest on a row.
func installedMCP(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	m, _ := row["manifest"].(map[string]any)
	acts, _ := m["actions"].(map[string]any)
	mcp, _ := acts["mcp"].(map[string]any)
	if mcp == nil {
		t.Fatalf("row has no actions.mcp: %v", row)
	}
	return mcp
}

// trebiCatalogFixture serves a signed Trebi catalog with two versions of
// one connector that needs no program.
func trebiCatalogFixture(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := func(v string) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		files := map[string]string{
			"trebi-connector.yaml": "schema: trebi-connector/1\nname: demo\ntitle: Demo\ndescription: A demo.\nversion: " + v +
				"\npublisher: trebi\nactions:\n  cli:\n    commands: [sh]\n",
			"skill/SKILL.md": "---\nname: demo\ndescription: Use sh.\n---\n\nRun sh.\n",
		}
		for name, body := range files {
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	snaps := map[string][]byte{"1.0.0": snap("1.0.0"), "1.1.0": snap("1.1.0")}
	var idx []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/index.json":
			_, _ = w.Write(idx)
		case r.URL.Path == "/v1/index.json.sig":
			_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, idx))))
		case strings.HasPrefix(r.URL.Path, "/v1/snapshots/demo/"):
			v := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/snapshots/demo/"), ".tar.gz")
			_, _ = w.Write(snaps[v])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	var vers []registry.IndexVersion
	for _, v := range []string{"1.0.0", "1.1.0"} {
		sum := sha256.Sum256(snaps[v])
		vers = append(vers, registry.IndexVersion{Version: v, Digest: "sha256:" + hex.EncodeToString(sum[:]),
			URL: srv.URL + "/v1/snapshots/demo/" + v + ".tar.gz", PublishedAt: "2026-09-28T00:00:00Z"})
	}
	idx, err = json.Marshal(registry.Index{GeneratedAt: "2026-09-28T00:00:00Z", Entries: []registry.IndexEntry{{
		Name: "demo", Title: "Demo", Description: "A demo.", Publisher: "trebi", Capabilities: []string{"actions"},
		Latest: "1.1.0", Versions: vers,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return srv.URL + "/v1", pub
}
