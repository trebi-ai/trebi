package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactRoutes(t *testing.T) {
	e := startEmailAPI(t)
	wsPath := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(filepath.Join(wsPath, "agents", "research"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "agents", "research", "AGENT.md"), []byte("---\ndescription: Test agent.\n---\n\nResearch.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, wsBody := e.do(t, http.MethodPost, "/v1/workspaces", map[string]string{"path": wsPath, "name": "arts"})
	if code != 201 {
		t.Fatalf("workspace %d %v", code, wsBody)
	}
	wsID, _ := wsBody["id"].(string)
	q := "?workspace=" + wsID

	put := func(body map[string]any) (int, map[string]any) {
		body["workspace"] = wsID
		return e.do(t, http.MethodPost, "/v1/artifacts", body)
	}
	code, m := put(map[string]any{"name": "weekly-report", "content": "# Week 41\n", "file_name": "report.md", "owner": "agent:research"})
	art, _ := m["artifact"].(map[string]any)
	if code != 201 || art["kind"] != "markdown" || art["owner_kind"] != "agent" {
		t.Fatalf("put: %d %v", code, m)
	}
	if code, m = put(map[string]any{"name": "weekly-report", "content": "# Week 41\nTotals.\n", "base": 1}); code != 201 {
		t.Fatalf("put v2: %d %v", code, m)
	}
	if v, _ := m["version"].(map[string]any); v["n"] != float64(2) {
		t.Fatalf("v2 = %v", m)
	}
	// A save from an old base is stale and names the latest version.
	if code, m = put(map[string]any{"name": "weekly-report", "content": "# Other\n", "base": 1}); code != 409 || errCode(m) != "stale" || errDetails(m)["latest"] != float64(2) {
		t.Fatalf("stale: %d %v", code, m)
	}
	if code, m = put(map[string]any{"name": "Bad Name", "content": "x"}); code != 422 && code != 400 {
		t.Fatalf("bad name: %d %v", code, m)
	}

	code, m = e.do(t, http.MethodGet, "/v1/artifacts/weekly-report"+q, nil)
	if code != 200 || m["latest_version"] != float64(2) {
		t.Fatalf("get: %d %v", code, m)
	}
	id, _ := m["id"].(string)
	if code, m = e.do(t, http.MethodGet, "/v1/artifacts"+q, nil); code != 200 || len(m["items"].([]any)) != 1 {
		t.Fatalf("list: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodGet, "/v1/artifacts/"+id+"/versions", nil); code != 200 || len(m["items"].([]any)) != 2 {
		t.Fatalf("versions: %d %v", code, m)
	}

	// The content carries the sandbox headers and an ETag.
	code, h, raw := e.getRaw(t, "/v1/artifacts/"+id+"/versions/1/content")
	if code != 200 || string(raw) != "# Week 41\n" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Content-Security-Policy") == "" || h.Get("ETag") == "" {
		t.Fatalf("content: %d %v %q", code, h, raw)
	}
	req, _ := http.NewRequest(http.MethodGet, e.base+"/v1/artifacts/"+id+"/versions/1/content", nil)
	req.Header.Set("If-None-Match", h.Get("ETag"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", resp.StatusCode)
	}
	if code, h, _ = e.getRaw(t, "/v1/artifacts/"+id+"/versions/latest/content?download=1"); code != 200 || !strings.Contains(h.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %d %v", code, h)
	}

	if code, m = e.do(t, http.MethodPost, "/v1/artifacts/"+id+"/versions/1/pin", nil); code != 200 || m["pinned"] != true {
		t.Fatalf("pin: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodPatch, "/v1/artifacts/"+id, map[string]any{"title": "Weekly report"}); code != 200 || m["title"] != "Weekly report" {
		t.Fatalf("patch: %d %v", code, m)
	}

	// The chat of an agent artifact is a new conversation with the agent.
	code, m = e.do(t, http.MethodPost, "/v1/artifacts/"+id+"/chat", nil)
	convID, _ := m["conversation_id"].(string)
	if code != 200 || convID == "" || m["created"] != true {
		t.Fatalf("chat: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodGet, "/v1/conversations/"+convID, nil); code != 200 || m["title"] != "About Weekly report" ||
		!strings.HasPrefix(m["operator_id"].(string), "agt_") {
		t.Fatalf("chat conversation: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodPost, "/v1/artifacts/"+id+"/chat", map[string]any{"version": 1}); code != 200 || m["conversation_id"] != convID || m["created"] != false {
		t.Fatalf("chat again: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodPost, "/v1/artifacts/"+id+"/ask", map[string]any{"text": "Shorter"}); code != 202 ||
		m["via"] != "conversation" || m["conversation_id"] != convID || m["run_id"] != nil {
		t.Fatalf("ask: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodGet, "/v1/artifacts"+q+"&conversation="+convID, nil); code != 200 || len(m["items"].([]any)) != 1 {
		t.Fatalf("list by conversation: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodGet, "/v1/artifacts"+q+"&conversation=nope", nil); code != 200 || len(m["items"].([]any)) != 0 {
		t.Fatalf("list by other conversation: %d %v", code, m)
	}
	token := "chat-token-000000000000000000000000"
	insertAgentCaller(t, e.st, wsID, "research", token, "running")
	if code, m = doScoped(t, http.MethodPost, e.base+"/v1/artifacts/"+id+"/chat", token, map[string]any{}); code != 403 {
		t.Fatalf("chat from a run: %d %v", code, m)
	}

	// A report rides a mail with the title as the default subject.
	code, m = e.do(t, http.MethodPost, "/v1/email/send", map[string]any{
		"workspace": wsID, "to": []string{"ops@example.com"}, "artifact": map[string]any{"ref": "weekly-report"},
	})
	if code != 200 || e.fw.sends != 1 || e.fw.last.Subject != "Weekly report" {
		t.Fatalf("email send artifact: %d %v subject=%q", code, m, e.fw.last.Subject)
	}
	if code, m = e.do(t, http.MethodGet, "/v1/artifacts/"+id+"/deliveries", nil); code != 200 || len(m["items"].([]any)) != 1 {
		t.Fatalf("deliveries: %d %v", code, m)
	}

	// An email draft sends through the native channel.
	draft := "---\nkind: email\nfrom: trebi\nto: [ops@example.com]\nsubject: Hello\n---\nHello **team**.\n"
	if code, m = put(map[string]any{"name": "hello-mail", "content": draft, "owner": "assistant"}); code != 201 {
		t.Fatalf("put draft: %d %v", code, m)
	}
	code, m = e.do(t, http.MethodPost, "/v1/artifacts/hello-mail/versions/latest/send"+q, map[string]any{})
	if code != 200 || m["via"] != "trebi" || m["sent"] != true || e.fw.sends != 2 || e.fw.last.Subject != "Hello" {
		t.Fatalf("send draft: %d %v", code, m)
	}
	if !strings.Contains(e.fw.last.HTML, "<strong>team</strong>") || strings.Contains(e.fw.last.Text, "kind: email") {
		t.Fatalf("draft body: %q", e.fw.last.Text)
	}
	// A report is not a draft.
	if code, m = e.do(t, http.MethodPost, "/v1/artifacts/"+id+"/versions/latest/send", map[string]any{}); code != 422 {
		t.Fatalf("send report: %d %v", code, m)
	}

	// The live routes refuse an artifact with no sources.
	if code, m = e.do(t, http.MethodPost, "/v1/artifacts/"+id+"/refresh", map[string]any{}); code != 422 {
		t.Fatalf("refresh not live: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodPost, "/v1/artifacts/"+id+"/versions/1/decide", map[string]any{"decision": "approve"}); code != 422 {
		t.Fatalf("decide not live: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodPost, "/v1/artifacts/"+id+"/versions/1/decide", map[string]any{"decision": "maybe"}); code != 400 {
		t.Fatalf("decide bad value: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodPut, "/v1/artifacts/"+id+"/schedule", map[string]any{"schedule": "@every 1h"}); code != 422 {
		t.Fatalf("schedule not live: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodGet, "/v1/artifacts/"+id+"/snapshots", nil); code != 200 || len(m["items"].([]any)) != 0 {
		t.Fatalf("snapshots: %d %v", code, m)
	}
	live := map[string]any{"sources": []map[string]any{{"name": "orders", "connection": "nope", "operation": "query"}}}
	if code, m = put(map[string]any{"name": "live-report", "content": "{{ table .orders }}", "file_name": "r.md", "owner": "assistant", "live": live}); code != 422 {
		t.Fatalf("live with no connection: %d %v", code, m)
	}

	// Delete needs confirm.
	if code, m = e.do(t, http.MethodDelete, "/v1/artifacts/"+id, nil); code != 400 || errDetails(m)["confirm_required"] != true {
		t.Fatalf("delete no confirm: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodDelete, "/v1/artifacts/"+id+"?confirm=true", nil); code != 200 {
		t.Fatalf("delete: %d %v", code, m)
	}
	if code, _ = e.do(t, http.MethodGet, "/v1/artifacts/"+id, nil); code != 404 {
		t.Fatalf("after delete: %d", code)
	}
}
