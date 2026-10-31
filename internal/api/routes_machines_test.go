package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestMachinesRoutes(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "mach-ws")

	code, body, _ := doJSON(t, "GET", base+"/v1/machines", nil)
	list, _ := body["machines"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("list: %d %v", code, body)
	}
	self, _ := list[0].(map[string]any)
	id, _ := self["id"].(string)
	if self["name"] != "This machine" || self["is_self"] != true || self["device_id"] != nil || self["last_seen"] == nil || self["visor_build"] != "" || id == "" {
		t.Fatalf("self row: %v", self)
	}
	if _, ok := self["capabilities"]; ok {
		t.Fatalf("list row has capabilities: %v", self)
	}

	for _, ref := range []string{"self", id} {
		code, got, _ := doJSON(t, "GET", base+"/v1/machines/"+ref, nil)
		caps, _ := got["capabilities"].(map[string]any)
		if code != 200 || got["id"] != id || caps == nil {
			t.Fatalf("get %s: %d %v", ref, code, got)
		}
		if _, ok := caps["harnesses"].([]any); !ok {
			t.Fatalf("harnesses: %v", caps)
		}
		if _, ok := caps["connectors"].([]any); !ok {
			t.Fatalf("connectors: %v", caps)
		}
		if v, _ := caps["versions"].(map[string]any); v["trebi"] != api.Version {
			t.Fatalf("versions: %v", caps["versions"])
		}
	}
	if code, got, _ := doJSON(t, "GET", base+"/v1/machines/mch_nope", nil); code != 404 || errCode(got) != "not_found" {
		t.Fatalf("missing: %d %v", code, got)
	}

	code, ws, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID, nil)
	m, _ := ws["machine"].(map[string]any)
	if code != 200 || m["id"] != id || m["name"] != "This machine" || m["is_self"] != true {
		t.Fatalf("workspace machine: %d %v", code, ws)
	}
}

func TestJobAndAgentReadiness(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(wsDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ready/JOB.md", "---\nharness: fake\nenv:\n  API_KEY: secret:api_key\nconnectors: [notion]\n---\n\nDo work.\n")
	write("agents/scout/AGENT.md", "---\nharness: fake\ndescription: Scout.\nkit:\n  connectors: [linear]\n---\n\nScout.\n")
	wsID := addWorkspace(t, base, wsDir, "ready-ws")

	item := func(kind, name, state, action string) any {
		return map[string]any{"kind": kind, "name": name, "state": state, "action": action}
	}
	code, got, _ := doJSON(t, "GET", base+"/v1/jobs/ready?workspace="+wsID, nil)
	if code != 200 {
		t.Fatalf("job get: %d %v", code, got)
	}
	r, _ := got["readiness"].([]any)
	if len(r) != 3 {
		t.Fatalf("job readiness: %v", got["readiness"])
	}
	if h, _ := r[0].(map[string]any); h["kind"] != "harness" || h["name"] != "fake" {
		t.Fatalf("harness item: %v", r[0])
	}
	if want := []any{item("connector", "notion", "missing", "Connect"), item("secret", "api_key", "missing", "Add secret")}; !reflect.DeepEqual(r[1:], want) {
		t.Fatalf("job readiness = %v", r)
	}

	code, got, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/agents/scout", nil)
	if code != 200 {
		t.Fatalf("agent get: %d %v", code, got)
	}
	r, _ = got["readiness"].([]any)
	if len(r) != 2 || !reflect.DeepEqual(r[1], item("connector", "linear", "missing", "Connect")) {
		t.Fatalf("agent readiness = %v", got["readiness"])
	}
}

func TestObservedSessionRoutes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, base := startTestServer(t)

	code, body, _ := doJSON(t, "GET", base+"/v1/machines/self/sessions", nil)
	if list, ok := body["sessions"].([]any); code != 200 || !ok || len(list) != 0 {
		t.Fatalf("list: %d %v", code, body)
	}
	if code, got, _ := doJSON(t, "GET", base+"/v1/machines/self/sessions/ses_nope", nil); code != 404 || errCode(got) != "not_found" {
		t.Fatalf("missing session: %d %v", code, got)
	}
	if code, got, _ := doJSON(t, "GET", base+"/v1/machines/mch_nope/sessions", nil); code != 404 || errCode(got) != "not_found" {
		t.Fatalf("missing machine: %d %v", code, got)
	}

	if code, got, _ := doJSON(t, "GET", base+"/v1/harnesses/claude/hooks", nil); code != 200 || got["state"] != "off" {
		t.Fatalf("hooks: %d %v", code, got)
	}
	if code, got, _ := doJSON(t, "PUT", base+"/v1/harnesses/claude/hooks", map[string]any{"on": true}); code != 200 || got["state"] != "on" {
		t.Fatalf("hooks on: %d %v", code, got)
	}
	if code, got, _ := doJSON(t, "PUT", base+"/v1/harnesses/claude/hooks", map[string]any{}); code != 400 || errCode(got) != "validation" {
		t.Fatalf("no on: %d %v", code, got)
	}
	if code, got, _ := doJSON(t, "GET", base+"/v1/harnesses/nope/hooks", nil); code != 404 {
		t.Fatalf("unknown harness: %d %v", code, got)
	}
}

func TestForwardMachine(t *testing.T) {
	_, base := startTestServerSeeded(t, api.Options{}, func(st *store.Store) {
		now := "2026-10-30T00:00:00Z"
		if _, err := st.DB().Exec(`INSERT INTO machines (id, name, is_self, created_at, updated_at) VALUES ('mch_other', 'Studio', 0, ?, ?)`, now, now); err != nil {
			t.Fatal(err)
		}
	})
	_, body, _ := doJSON(t, "GET", base+"/v1/machines/self", nil)
	self, _ := body["id"].(string)

	get := func(url, target string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if target != "" {
			req.Header.Set("X-Trebi-Machine", target)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)
		return resp.StatusCode, got
	}
	if code, got := get(base+"/v1/machines", self); code != 200 || got["machines"] == nil {
		t.Fatalf("self: %d %v", code, got)
	}
	if code, got := get(base+"/v1/machines?trebi_machine="+self, ""); code != 200 || got["machines"] == nil {
		t.Fatalf("self by query: %d %v", code, got)
	}
	if code, got := get(base+"/v1/machines", "mch_nope"); code != 404 || errCode(got) != "not_found" {
		t.Fatalf("unknown: %d %v", code, got)
	}
	if code, got := get(base+"/v1/machines", "mch_other"); code != 409 || errCode(got) != "not_connected" {
		t.Fatalf("not connected: %d %v", code, got)
	}
	code, got := get(base+"/v1/machines/mch_other", "")
	if link, _ := got["link"].(map[string]any); code != 200 || link["state"] != "none" {
		t.Fatalf("link field: %d %v", code, got)
	}

	if code, got, _ := doJSON(t, "POST", base+"/v1/machines/mch_other/link", map[string]any{"link": "https://example.com/"}); code != 400 || errCode(got) != "validation" {
		t.Fatalf("bad link: %d %v", code, got)
	}
	if code, got, _ := doJSON(t, "DELETE", base+"/v1/machines/mch_other/link", nil); code != 400 || errCode(got) != "validation" {
		t.Fatalf("unlink without confirm: %d %v", code, got)
	}
	if code, got, _ := doJSON(t, "DELETE", base+"/v1/machines/mch_other/link?confirm=1", nil); code != 204 {
		t.Fatalf("unlink: %d %v", code, got)
	}
}
