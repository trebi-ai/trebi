package api_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestHarnessAuthGetMode(t *testing.T) {
	_, base := startTestServer(t)

	code, body, _ := doJSON(t, "GET", base+"/v1/harnesses/claude/auth", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if body["auth_mode"] != "shared" && body["mode"] != "shared" {
		// auth_mode is the field
		if body["auth_mode"] != harness.AuthModeShared {
			t.Fatalf("mode=%v (must not be hard-coded incorrectly when config missing; default shared)", body["auth_mode"])
		}
	}
	if _, ok := body["login_modes"]; !ok {
		t.Fatalf("login_modes missing: %v", body)
	}
	if _, ok := body["keychain_bound"]; !ok {
		t.Fatalf("keychain_bound missing: %v", body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/harnesses/nope/auth", nil)
	if code != 404 {
		t.Fatalf("unknown %d %v", code, body)
	}
}

func TestHarnessAuthMarkGone(t *testing.T) {
	_, base := startTestServer(t)
	code, _, _ := doJSON(t, "POST", base+"/v1/harnesses/claude/auth", map[string]any{})
	if code != 404 && code != 405 {
		t.Fatalf("POST /auth should be gone, got %d", code)
	}
}

func TestHarnessListAuthFields(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "GET", base+"/v1/harnesses", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	list, _ := body["harnesses"].([]any)
	if len(list) == 0 {
		t.Fatal("empty")
	}
	row, _ := list[0].(map[string]any)
	for _, k := range []string{"auth_mode", "login_modes", "keychain_bound", "source"} {
		if _, ok := row[k]; !ok {
			t.Fatalf("missing %s in %v", k, row)
		}
	}
}

func TestHarnessLocalOnlyRefusal(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/harnesses/cursor/login", map[string]any{})
	if code != 422 {
		t.Fatalf("want 422 got %d %v", code, body)
	}
	if errCode(body) != "precondition_failed" {
		t.Fatalf("code %v", body)
	}
	err, _ := body["error"].(map[string]any)
	details, _ := err["details"].(map[string]any)
	if details["local_cmd"] == nil || details["hint"] == nil {
		t.Fatalf("details %v", details)
	}
}

func TestHarnessLoginCodeNoSession(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/harnesses/claude/login", map[string]any{"code": "ABCD"})
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("%d %v", code, body)
	}
}

func TestHarnessLogoutSharedRequiresConfirm(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/harnesses/claude/logout", map[string]any{})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("%d %v", code, body)
	}
}

func TestHarnessLoginToken(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/harnesses/claude/login", map[string]any{
		"token": "sk-test",
	})
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if body["auth_status"] != "ok" && body["auth_ok"] != true {
		t.Fatalf("token login should probe ok via L1: %v", body)
	}
}

func TestHarnessPatchMode(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "PATCH", base+"/v1/harnesses/claude", map[string]any{
		"auth_mode": "separate",
	})
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if body["auth_mode"] != "separate" {
		t.Fatalf("%v", body)
	}
}

func TestHarnessPatchModeFakeNotPersisted(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "PATCH", base+"/v1/harnesses/fake", map[string]any{
		"auth_mode": "separate",
	})
	if code != 422 || errCode(body) != "precondition_failed" {
		t.Fatalf("%d %v", code, body)
	}
}

func TestHarnessEnsureFake(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/harnesses/fake/ensure", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if body["ensured"] != true {
		t.Fatalf("%v", body)
	}
}

func TestHarnessEnsureInstallError(t *testing.T) {
	_, base := startTestServer(t)
	dir := home.HarnessDir("claude")
	if err := os.MkdirAll(filepath.Join(dir, "trebi-settings.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, body, _ := doJSON(t, "POST", base+"/v1/harnesses/claude/ensure", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if body["ensured"] != false {
		t.Fatalf("want ensured false got %v", body)
	}
	if body["error"] == nil || body["error"] == "" {
		t.Fatalf("want error got %v", body)
	}
}

type fakeLimitsReader struct{ l agentwire.Limits }

func (f fakeLimitsReader) Limits(context.Context, agentwire.Harness, agentwire.ModelQuery) (agentwire.Limits, error) {
	return f.l, nil
}

func TestHarnessUsageRoutes(t *testing.T) {
	var st *store.Store
	srv, base := startTestServerSeeded(t, api.Options{}, func(s *store.Store) { st = s })
	reader := fakeLimitsReader{l: agentwire.Limits{Plan: "plus", Windows: []agentwire.LimitWindow{
		{Key: agentwire.WindowWeekly, Minutes: 10080, UsedPercent: 42, ResetsAt: time.Now().Add(time.Hour)},
	}}}
	srv.BindUsage(harness.NewUsagePoller(harness.NewUsageStore(st), reader, nil, time.Hour).WithSessionDirs(t.TempDir()))

	code, body, _ := doJSON(t, "GET", base+"/v1/harnesses/usage", nil)
	if list, _ := body["harnesses"].([]any); code != 200 || list == nil || len(list) != 0 {
		t.Fatalf("empty: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/harnesses/usage/refresh", nil)
	if code != 200 {
		t.Fatalf("refresh: %d %v", code, body)
	}
	list, _ := body["harnesses"].([]any)
	if len(list) != 1 {
		t.Fatalf("refresh body %v", body)
	}
	h, _ := list[0].(map[string]any)
	wins, _ := h["windows"].([]any)
	w0, _ := wins[0].(map[string]any)
	if h["harness"] != "codex" || h["plan"] != "plus" || w0["key"] != "weekly" || w0["used_percent"] != 42.0 || w0["source"] != "probe" {
		t.Fatalf("usage %v", h)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/harnesses/usage/refresh", nil)
	if code != 429 {
		t.Fatalf("second refresh: %d %v", code, body)
	}
}

func TestHarnessPatchSettings(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "PATCH", base+"/v1/harnesses/codex", map[string]any{
		"enabled": true, "model": "gpt-5.5", "effort": "high",
	})
	if code != 200 || body["model"] != "gpt-5.5" || body["effort"] != "high" || body["enabled"] != true {
		t.Fatalf("round trip: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PATCH", base+"/v1/harnesses/codex", map[string]any{"effort": "huge"})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("bad effort: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PATCH", base+"/v1/harnesses/fake", map[string]any{"model": "x"})
	if code != 422 || errCode(body) != "precondition_failed" {
		t.Fatalf("model on a harness with no model setting: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PATCH", base+"/v1/harnesses/codex", map[string]any{"enabled": false})
	if code != 200 || body["enabled"] != false {
		t.Fatalf("turn off: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PATCH", base+"/v1/harnesses/grok", map[string]any{"approval": "auto"})
	if code != 200 || body["approval"] != "auto" || body["asks_approval"] != false {
		t.Fatalf("grok approval: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PATCH", base+"/v1/harnesses/grok", map[string]any{"approval": "always"})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("bad approval: %d %v", code, body)
	}
}

func TestHarnessFilesRoutes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, base := startTestServer(t)
	files := base + "/v1/harnesses/claude/files"
	code, body, _ := doJSON(t, "GET", files, nil)
	list, _ := body["files"].([]any)
	if code != 200 || len(list) != 4 {
		t.Fatalf("list: %d %v", code, body)
	}
	if code, body, _ = doJSON(t, "PUT", files+"/settings_prefs", map[string]any{"content": "{}"}); code != 409 {
		t.Fatalf("write in yours mode: %d %v", code, body)
	}
	if code, body, _ = doJSON(t, "POST", files+"/settings_prefs/own", map[string]any{"start": "empty"}); code != 200 {
		t.Fatalf("own: %d %v", code, body)
	}
	code, body, hdr := doJSON(t, "GET", files+"/settings_prefs", nil)
	etag, _ := body["etag"].(string)
	if code != 200 || etag == "" || hdr.Get("ETag") == "" {
		t.Fatalf("read: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PUT", files+"/settings_prefs", map[string]any{"content": "{\n  \"a\": ,\n}"})
	details, _ := body["error"].(map[string]any)["details"].(map[string]any)
	if code != 422 || details["line"] != float64(2) {
		t.Fatalf("parse error: %d %v", code, body)
	}
	code, body, _ = doJSONHeaders(t, "PUT", files+"/settings_prefs", map[string]any{"content": "{}\n"}, map[string]string{"If-Match": `"stale"`})
	if code != 412 {
		t.Fatalf("stale: %d %v", code, body)
	}
	code, body, _ = doJSONHeaders(t, "PUT", files+"/settings_prefs", map[string]any{"content": "{\"model\": \"opus\"}\n"}, map[string]string{"If-Match": `"` + etag + `"`})
	if code != 200 {
		t.Fatalf("write: %d %v", code, body)
	}
	if code, body, _ = doJSON(t, "DELETE", files+"/settings_prefs/own", nil); code != 400 {
		t.Fatalf("reset needs confirm: %d %v", code, body)
	}
	if code, body, _ = doJSON(t, "DELETE", files+"/settings_prefs/own?confirm=1", nil); code != 200 {
		t.Fatalf("reset: %d %v", code, body)
	}
	if code, _, _ = doJSON(t, "GET", files+"/nope", nil); code != 404 {
		t.Fatalf("unknown key: %d", code)
	}
}

func TestHarnessRowUsesVisorInstall(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, base := startTestServer(t)
	code, list, _ := doJSON(t, "GET", base+"/v1/harnesses", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, list)
	}
	var body map[string]any
	for _, r := range list["harnesses"].([]any) {
		if row, _ := r.(map[string]any); row["name"] == "fake" {
			body = row
		}
	}
	if body["installed"] != true || body["bin_path"] != bin || body["source"] != "system" {
		t.Fatalf("installed=%v bin_path=%v source=%v", body["installed"], body["bin_path"], body["source"])
	}
}

// TestHarnessVariantCRUD covers a variant from create to delete: the env
// with a secret that the API never returns, the env patch, the jobs count,
// and the move of the jobs on delete.
func TestHarnessVariantCRUD(t *testing.T) {
	_, base := startTestServer(t)

	code, row, _ := doJSON(t, "POST", base+"/v1/harnesses", map[string]any{
		"name": "claude-zai", "base": "claude", "label": "Claude (Z.ai)", "model": "glm-5",
		"env": map[string]any{
			"ANTHROPIC_BASE_URL":   map[string]any{"value": "https://api.z.ai/api/anthropic"},
			"ANTHROPIC_AUTH_TOKEN": map[string]any{"value": "tok-123", "secret": true},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("create %d %v", code, row)
	}
	if row["base"] != "claude" || row["label"] != "Claude (Z.ai)" || row["sign_in"] != harness.SignInService || row["enabled"] != true {
		t.Fatalf("row: %v", row)
	}
	env := map[string]map[string]any{}
	for _, e := range row["env"].([]any) {
		m := e.(map[string]any)
		env[m["key"].(string)] = m
	}
	if tok := env["ANTHROPIC_AUTH_TOKEN"]; tok["secret"] != true || tok["set"] != true || tok["value"] != nil {
		t.Fatalf("secret env: %v", tok)
	}
	if env["ANTHROPIC_BASE_URL"]["value"] != "https://api.z.ai/api/anthropic" {
		t.Fatalf("plain env: %v", env)
	}
	if v, err := secrets.Get("harness.claude-zai.ANTHROPIC_AUTH_TOKEN"); err != nil || v != "tok-123" {
		t.Fatalf("secret %q %v", v, err)
	}

	for _, tc := range []struct {
		body map[string]any
		code int
	}{
		{map[string]any{"name": "claude-zai", "base": "claude"}, http.StatusConflict},
		{map[string]any{"name": "codex", "base": "claude"}, http.StatusBadRequest},
		{map[string]any{"name": "Bad Name", "base": "claude"}, http.StatusBadRequest},
		{map[string]any{"name": "codex-x", "base": "codex"}, http.StatusBadRequest},
		{map[string]any{"name": "claude-y", "base": "claude", "env": map[string]any{"CLAUDE_CONFIG_DIR": map[string]any{"value": "/x"}}}, http.StatusBadRequest},
	} {
		if code, body, _ := doJSON(t, "POST", base+"/v1/harnesses", tc.body); code != tc.code {
			t.Fatalf("create %v: %d %v", tc.body, code, body)
		}
	}

	// A secret becomes plain only with a new value. A removed secret is gone.
	if code, body, _ := doJSON(t, "PATCH", base+"/v1/harnesses/claude-zai", map[string]any{"env": map[string]any{"ANTHROPIC_AUTH_TOKEN": map[string]any{"secret": false}}}); code != http.StatusBadRequest {
		t.Fatalf("secret to plain with no value: %d %v", code, body)
	}
	code, row, _ = doJSON(t, "PATCH", base+"/v1/harnesses/claude-zai", map[string]any{
		"label": "Z.ai", "env": map[string]any{"ANTHROPIC_AUTH_TOKEN": nil, "ANTHROPIC_MODEL": map[string]any{"value": "glm-5"}},
	})
	if code != http.StatusOK || row["label"] != "Z.ai" {
		t.Fatalf("patch %d %v", code, row)
	}
	if _, err := secrets.Get("harness.claude-zai.ANTHROPIC_AUTH_TOKEN"); err == nil {
		t.Fatal("the removed secret stays")
	}
	if code, body, _ := doJSON(t, "PATCH", base+"/v1/harnesses/claude-zai", map[string]any{"updates": "keep"}); code == http.StatusOK {
		t.Fatalf("a variant took updates: %v", body)
	}

	wsDir := t.TempDir()
	writeFiles(t, map[string]string{
		filepath.Join(wsDir, "agents", "ops", "AGENT.md"):      "---\ndescription: Ops agent.\n---\n\nRun ops.\n",
		filepath.Join(wsDir, "agents", "ops", "zai", "JOB.md"): "---\nharness: claude-zai\n---\n\nDo work.\n",
	})
	wsID := addWorkspace(t, base, wsDir, "variant-ws")
	if j := listJobs(t, base, "workspace="+wsID)["agents/ops/zai"]; j["harness"] != "claude-zai" {
		t.Fatalf("job harness: %v", j)
	}
	_, list, _ := doJSON(t, "GET", base+"/v1/harnesses", nil)
	for _, h := range list["harnesses"].([]any) {
		if m := h.(map[string]any); m["name"] == "claude-zai" && m["jobs"] != float64(1) {
			t.Fatalf("jobs count: %v", m)
		}
	}

	code, body, _ := doJSON(t, "DELETE", base+"/v1/harnesses/claude-zai", nil)
	if code != http.StatusConflict {
		t.Fatalf("delete in use %d %v", code, body)
	}
	if e, _ := body["error"].(map[string]any); e["code"] != "in_use" || e["details"].(map[string]any)["jobs"] != float64(1) {
		t.Fatalf("in_use: %v", body)
	}
	if code, body, _ := doJSON(t, "DELETE", base+"/v1/harnesses/claude", nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("delete built-in %d %v", code, body)
	}
	if code, body, _ := doJSON(t, "DELETE", base+"/v1/harnesses/claude-zai?move_to=claude", nil); code != http.StatusNoContent {
		t.Fatalf("delete %d %v", code, body)
	}
	if j := listJobs(t, base, "workspace="+wsID)["agents/ops/zai"]; j["harness"] != "claude" {
		t.Fatalf("moved job: %v", j)
	}
	data, err := os.ReadFile(filepath.Join(wsDir, "agents", "ops", "zai", "JOB.md"))
	if err != nil || !strings.Contains(string(data), "harness: claude\n") {
		t.Fatalf("JOB.md: %q %v", data, err)
	}
	if code, _, _ := doJSON(t, "GET", base+"/v1/harnesses/claude-zai/auth", nil); code != http.StatusNotFound {
		t.Fatalf("deleted variant answers %d", code)
	}
}
