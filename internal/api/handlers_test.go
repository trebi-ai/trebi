package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestGapHandlers_RealNotStubs exercises attach-related setup, memory ledger,
// assistant PUT, integration cursor/logs, and doctor fix — proving handlers
// are more than empty JSON stubs.
func TestGapHandlers_RealNotStubs(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	// Workspace with job + assistant for memory/assistant routes
	wsPath := filepath.Join(dir, "ws")
	if err := os.MkdirAll(filepath.Join(wsPath, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "demo", "JOB.md"),
		[]byte("---\nharness: fake\n---\n\nJob.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "TREBI.md"),
		[]byte("---\nharness: fake\n---\n\nAssistant.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, api.Options{Token: "tok"})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr
	client := &http.Client{Timeout: 10 * time.Second}
	do := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, base+path, r)
		req.Header.Set("Authorization", "Bearer tok")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return resp.StatusCode, m
	}

	// Register workspace
	code, wsBody := do("POST", "/v1/workspaces", map[string]string{"path": wsPath, "name": "gaps-ws"})
	if code != 201 {
		t.Fatalf("workspace %d %v", code, wsBody)
	}
	wsID, _ := wsBody["id"].(string)
	if wsID == "" {
		t.Fatal("no workspace id")
	}

	// --- Assistant PUT (real file write) — D1 If-Match optional ---
	// A request without If-Match proceeds (D1: absent header = unconditional).
	code, _ = do("PUT", "/v1/workspaces/"+wsID+"/assistant", map[string]string{
		"content": "---\nharness: fake\n---\n\nNo match.\n",
	})
	if code != 200 {
		t.Fatalf("assistant put without If-Match want 200 got %d", code)
	}
	// If-Match: * proceeds unconditionally when the file exists.
	req, _ := http.NewRequest("PUT", base+"/v1/workspaces/"+wsID+"/assistant",
		bytes.NewBufferString(`{"content":"---\nharness: fake\n---\n\nUpdated assistant body.\n"}`))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", "*")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	code = resp.StatusCode
	resp.Body.Close()
	if code != 200 {
		t.Fatalf("assistant put with If-Match * %d", code)
	}
	data, err := os.ReadFile(filepath.Join(wsPath, "TREBI.md"))
	if err != nil || !strings.Contains(string(data), "Updated assistant body") {
		t.Fatalf("TREBI.md not updated: %v %s", err, data)
	}

	// --- Memory entries (B22 ledger) ---
	code, ent := do("POST", "/v1/jobs/demo/memory/entries?workspace="+wsID, map[string]string{
		"entry": "remember the widget",
	})
	if code != 201 {
		t.Fatalf("memory entry post %d %v", code, ent)
	}
	eid, _ := ent["id"].(string)
	memPath := filepath.Join(wsPath, "demo", "MEMORY.md")
	mb, err := os.ReadFile(memPath)
	if err != nil || !strings.Contains(string(mb), "remember the widget") {
		t.Fatalf("MEMORY.md missing entry: %v %s", err, mb)
	}
	if eid != "" {
		code, _ = do("DELETE", "/v1/jobs/demo/memory/entries/"+eid+"?workspace="+wsID, nil)
		if code != 200 {
			t.Fatalf("memory entry delete %d", code)
		}
	}

	// --- Integration cursor (DB write) ---
	// create integration first
	code, _ = do("POST", "/v1/integrations", map[string]any{
		"name": "gap-src", "type": "stream", "command": "true", "format": "line",
	})
	if code != 201 {
		t.Fatalf("integ create %d", code)
	}
	code, cur := do("PUT", "/v1/integrations/gap-src/cursor", map[string]string{"last_event_id": "evt-99"})
	if code != 200 {
		t.Fatalf("cursor put %d %v", code, cur)
	}
	code, cur = do("GET", "/v1/integrations/gap-src/cursor", nil)
	if code != 200 || cur["last_event_id"] != "evt-99" {
		t.Fatalf("cursor get %d %v", code, cur)
	}

	// --- Integration logs (route returns SSE or JSON, not 404) ---
	req, _ = http.NewRequest("GET", base+"/v1/integrations/gap-src/logs", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ct := resp.Header.Get("Content-Type")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("logs status %d", resp.StatusCode)
	}
	if !strings.Contains(ct, "event-stream") && !strings.Contains(ct, "json") {
		t.Fatalf("logs content-type %q", ct)
	}

	// --- Doctor fix (writes config when missing) ---
	_ = os.Remove(home.ConfigPath())
	code, fix := do("POST", "/v1/system/doctor/config-missing/fix", nil)
	if code != 200 {
		t.Fatalf("doctor fix %d %v", code, fix)
	}
	if _, err := os.Stat(home.ConfigPath()); err != nil {
		t.Fatalf("doctor fix did not create config: %v (body=%v)", err, fix)
	}

	// --- Attach: WebSocket upgrade through the audit wrapper ---
	code, run := do("POST", "/v1/jobs/demo/run", map[string]any{
		"workspace": wsID, "harness": "fake", "prompt_override": "hi",
	})
	if code != 201 {
		t.Fatalf("run create %d %v", code, run)
	}
	runID, _ := run["id"].(string)

	// Unknown run id → 404 not_found, not a hijack 500.
	code, _ = do("GET", "/v1/runs/01NOPE/attach", nil)
	if code != 404 {
		t.Fatalf("attach unknown run: want 404 got %d", code)
	}

	// Real run: the handshake must upgrade (statusWriter implements
	// http.Hijacker). The fake run has a synthetic tmux name, so the server
	// then sends an error/screen frame — never a 500 hijack failure.
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/v1/runs/" + runID + "/attach"
	conn, wsResp, derr := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer tok"}})
	if derr != nil {
		status, body := 0, ""
		if wsResp != nil {
			status = wsResp.StatusCode
			b, _ := io.ReadAll(wsResp.Body)
			wsResp.Body.Close()
			body = string(b)
		}
		t.Fatalf("attach dial failed: %v (status=%d body=%s)", derr, status, body)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, rerr := conn.ReadMessage()
	if rerr != nil {
		t.Fatalf("attach read: %v", rerr)
	}
	var frame map[string]any
	if json.Unmarshal(msg, &frame) != nil || frame["type"] == nil {
		t.Fatalf("attach frame not typed JSON: %s", msg)
	}
}
