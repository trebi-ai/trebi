package api_test

import (
	"testing"

	"github.com/trebi-ai/trebi/internal/harness"
)

func TestHarnessAuthGetMode(t *testing.T) {
	s, base := startTestServer(t)
	token := s.Token()

	code, body, _ := doJSON(t, "GET", base+"/v1/harnesses/claude/auth", token, nil)
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

	code, body, _ = doJSON(t, "GET", base+"/v1/harnesses/nope/auth", token, nil)
	if code != 404 {
		t.Fatalf("unknown %d %v", code, body)
	}
}

func TestHarnessAuthMarkGone(t *testing.T) {
	s, base := startTestServer(t)
	code, _, _ := doJSON(t, "POST", base+"/v1/harnesses/claude/auth", s.Token(), map[string]any{})
	if code != 404 && code != 405 {
		t.Fatalf("POST /auth should be gone, got %d", code)
	}
}

func TestHarnessListAuthFields(t *testing.T) {
	s, base := startTestServer(t)
	code, body, _ := doJSON(t, "GET", base+"/v1/harnesses", s.Token(), nil)
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
	s, base := startTestServer(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/harnesses/cursor/login", s.Token(), map[string]any{})
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
	s, base := startTestServer(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/harnesses/claude/login", s.Token(), map[string]any{"code": "ABCD"})
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("%d %v", code, body)
	}
}

func TestHarnessLogoutSharedRequiresConfirm(t *testing.T) {
	s, base := startTestServer(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/harnesses/claude/logout", s.Token(), map[string]any{})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("%d %v", code, body)
	}
}

func TestHarnessLoginToken(t *testing.T) {
	s, base := startTestServer(t)
	code, body, _ := doJSON(t, "POST", base+"/v1/harnesses/claude/login", s.Token(), map[string]any{
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
	s, base := startTestServer(t)
	code, body, _ := doJSON(t, "PATCH", base+"/v1/harnesses/claude", s.Token(), map[string]any{
		"auth_mode": "separate",
	})
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if body["auth_mode"] != "separate" {
		t.Fatalf("%v", body)
	}
}
