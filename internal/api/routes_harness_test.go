package api_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/home"
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
	for _, k := range []string{"auth_mode", "login_modes", "keychain_bound"} {
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
	dir := filepath.Join(home.Base(), "harness", "claude")
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
