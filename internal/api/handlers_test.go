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
	"github.com/trebi-ai/trebi/internal/waittest"
)

// TestGapHandlers_RealNotStubs exercises attach-related setup, memory ledger,
// assistant PUT, connector cursor/logs, and doctor fix — proving handlers
// are more than empty JSON stubs.
func TestGapHandlers_RealNotStubs(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	// Workspace with job + assistant for memory/assistant routes
	wsPath := filepath.Join(dir, "ws")
	writeAgentFile(t, wsPath, "ops")
	if err := os.MkdirAll(filepath.Join(wsPath, "agents", "ops", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "agents", "ops", "demo", "JOB.md"),
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
	s := api.New(st, api.Options{})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr
	client := &http.Client{Timeout: waittest.D(waittest.Default)}
	do := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, base+path, r)
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

	// --- Assistant settings PUT (trebi.yaml) — D1 If-Match optional ---
	// A request without If-Match proceeds (D1: absent header = unconditional).
	code, body2 := do("PUT", "/v1/workspaces/"+wsID+"/assistant", map[string]any{
		"override": map[string]any{"harness": strPtrOf("fake")},
	})
	if code != 200 {
		t.Fatalf("assistant put without If-Match want 200 got %d %v", code, body2)
	}
	hash1, _ := body2["hash"].(string)
	// A stale If-Match is 412.
	req2, _ := http.NewRequest("PUT", base+"/v1/workspaces/"+wsID+"/assistant",
		bytes.NewBufferString(`{"override":{"name":"ellis"}}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("If-Match", "stale")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 412 {
		t.Fatalf("assistant put with stale If-Match %d", resp2.StatusCode)
	}
	// The current hash writes.
	req3, _ := http.NewRequest("PUT", base+"/v1/workspaces/"+wsID+"/assistant",
		bytes.NewBufferString(`{"override":{"name":"ellis"}}`))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("If-Match", hash1)
	resp3, err := client.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("assistant put with If-Match %d", resp3.StatusCode)
	}
	data, err := os.ReadFile(filepath.Join(wsPath, "trebi.yaml"))
	if err != nil || !strings.Contains(string(data), "ellis") {
		t.Fatalf("trebi.yaml not updated: %v %s", err, data)
	}

	// --- Memory log entries ---
	code, ent := do("POST", "/v1/jobs/agents%2Fops%2Fdemo/memory/log?workspace="+wsID, map[string]string{
		"text": "remember the widget",
	})
	if code != 201 {
		t.Fatalf("memory log post %d %v", code, ent)
	}
	eid, _ := ent["id"].(string)
	memPath := filepath.Join(wsPath, "agents", "ops", "demo", "MEMORY.md")
	mb, err := os.ReadFile(memPath)
	if err != nil || !strings.Contains(string(mb), "remember the widget") {
		t.Fatalf("MEMORY.md missing entry: %v %s", err, mb)
	}
	if eid != "" {
		code, _ = do("DELETE", "/v1/jobs/agents%2Fops%2Fdemo/memory/log/"+eid+"?workspace="+wsID, nil)
		if code != 200 {
			t.Fatalf("memory log delete %d", code)
		}
	}

	// --- Connector cursor (DB write) ---
	// create connector first
	code, _ = do("POST", "/v1/connectors", inlineEntry("gap-src", map[string]any{"events": map[string]any{"command": "true", "format": "line"}}))
	if code != 201 {
		t.Fatalf("connector create %d", code)
	}
	code, cur := do("PUT", "/v1/connectors/gap-src/cursor", map[string]string{"last_event_id": "evt-99"})
	if code != 200 {
		t.Fatalf("cursor put %d %v", code, cur)
	}
	code, cur = do("GET", "/v1/connectors/gap-src/cursor", nil)
	if code != 200 || cur["last_event_id"] != "evt-99" {
		t.Fatalf("cursor get %d %v", code, cur)
	}

	// --- Connector logs (route returns SSE or JSON, not 404) ---
	req4, _ := http.NewRequest("GET", base+"/v1/connectors/gap-src/logs", nil)
	resp4, err := client.Do(req4)
	if err != nil {
		t.Fatal(err)
	}
	ct := resp4.Header.Get("Content-Type")
	resp4.Body.Close()
	if resp4.StatusCode != 200 {
		t.Fatalf("logs status %d", resp4.StatusCode)
	}
	if !strings.Contains(ct, "event-stream") && !strings.Contains(ct, "json") {
		t.Fatalf("logs content-type %q", ct)
	}
	// The route sends only the last lines of a long log.
	_, row := do("GET", "/v1/connectors/gap-src", nil)
	logPath := filepath.Join(home.ConnectionDir(row["id"].(string)), "logs", "stream.jsonl")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	if err := os.WriteFile(logPath, []byte(strings.Repeat(`{"t":"","stream":"stdout","line":"x"}`+"\n", 1005)), 0o644); err != nil {
		t.Fatal(err)
	}
	resp4, err = client.Get(base + "/v1/connectors/gap-src/logs")
	if err != nil {
		t.Fatal(err)
	}
	body4, _ := io.ReadAll(resp4.Body)
	resp4.Body.Close()
	if n := strings.Count(string(body4), `data: {"t"`); n != 1000 {
		t.Fatalf("log tail sent %d lines", n)
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
	code, run := do("POST", "/v1/jobs/agents%2Fops%2Fdemo/run", map[string]any{
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
	// http.Hijacker). The fake run has a synthetic pane id, so the server
	// then sends an error/screen frame — never a 500 hijack failure.
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/v1/runs/" + runID + "/attach"
	conn, wsResp, derr := websocket.DefaultDialer.Dial(wsURL, nil)
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
	_ = conn.SetReadDeadline(time.Now().Add(waittest.D(5 * time.Second)))
	_, msg, rerr := conn.ReadMessage()
	if rerr != nil {
		t.Fatalf("attach read: %v", rerr)
	}
	var frame map[string]any
	if json.Unmarshal(msg, &frame) != nil || frame["type"] == nil {
		t.Fatalf("attach frame not typed JSON: %s", msg)
	}
}

func strPtrOf(s string) *string { return &s }
