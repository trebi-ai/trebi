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
	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/waittest"
)

// TestRunAttachCLIEvents pins the Part H attach contract: a CLI-runtime run
// streams normalized events over the existing WebSocket, never a pane.
func TestRunAttachCLIEvents(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	// A fake `claude` on PATH keeps commandUnavailable true without a vendor.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	wsPath := filepath.Join(dir, "ws")
	writeAgentFile(t, wsPath, "ops")
	if err := os.MkdirAll(filepath.Join(wsPath, "agents", "ops", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "agents", "ops", "demo", "JOB.md"),
		[]byte("---\nharness: claude\n---\n\nJob.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, api.Options{})
	s.SetCLIDriver(agentwire.FakeDriver{Script: `read -r line; echo '{"type":"assistant","text":"streamed hello"}'; sleep 30`})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr
	client := &http.Client{Timeout: waittest.D(waittest.Default)}
	do := func(method, path string, body any) (int, map[string]any) {
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

	code, ws := do("POST", "/v1/workspaces", map[string]any{"name": "cli-ws", "path": wsPath})
	if code != 201 {
		t.Fatalf("workspace %d %v", code, ws)
	}
	wsID, _ := ws["id"].(string)
	if wsID == "" {
		wsID, _ = ws["workspace"].(string)
	}
	if wsID == "" {
		t.Fatalf("no workspace id: %v", ws)
	}
	code, run := do("POST", "/v1/jobs/agents%2Fops%2Fdemo/run", map[string]any{
		"workspace": wsID, "prompt_override": "stream please",
	})
	if code != 201 {
		t.Fatalf("run create %d %v", code, run)
	}
	runID, _ := run["id"].(string)
	if run["runtime"] != "cli" {
		t.Fatalf("runtime = %v (%v)", run["runtime"], run)
	}
	if pane, _ := run["pane_session"].(string); pane != "" {
		t.Fatalf("cli run carries pane %q", pane)
	}
	if attach, _ := run["attach"].(map[string]any); attach == nil || attach["kind"] != "events" {
		t.Fatalf("attach = %v", run["attach"])
	}

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/v1/runs/" + runID + "/attach"
	conn, wsResp, derr := websocket.DefaultDialer.Dial(wsURL, nil)
	if derr != nil {
		body := ""
		if wsResp != nil {
			b, _ := io.ReadAll(wsResp.Body)
			wsResp.Body.Close()
			body = string(b)
		}
		t.Fatalf("attach dial: %v body=%s", derr, body)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(waittest.D(10 * time.Second)))
	sawEvent := false
	sawScreen := false
	for i := 0; i < 10 && !sawEvent; i++ {
		_, msg, rerr := conn.ReadMessage()
		if rerr != nil {
			t.Fatalf("attach read: %v", rerr)
		}
		var frame map[string]any
		if json.Unmarshal(msg, &frame) != nil {
			continue
		}
		switch frame["type"] {
		case "event", "status":
			sawEvent = true
		case "screen":
			sawScreen = true
		}
	}
	if sawScreen {
		t.Fatal("CLI attach sent a pane frame")
	}
	if !sawEvent {
		t.Fatal("no event/status frame on CLI attach")
	}
	// The run must still be killable through the normal path.
	if code, _ := do("POST", "/v1/runs/"+runID+"/kill", nil); code != 200 {
		t.Fatalf("kill %d", code)
	}
}

// TestAgentHookAdvisoryOnCLI pins G.4: hooks never mutate CLI-run state or
// open approvals; they only append timeline points.
func TestAgentHookAdvisoryOnCLI(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	enableHarnesses(t, dir)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	wsPath := filepath.Join(dir, "ws")
	writeAgentFile(t, wsPath, "ops")
	if err := os.MkdirAll(filepath.Join(wsPath, "agents", "ops", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "agents", "ops", "demo", "JOB.md"),
		[]byte("---\nharness: claude\n---\n\nJob.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, api.Options{})
	svc := s.Runs()
	svc.SetCLIDriver(agentwire.FakeDriver{Script: `read -r line; sleep 30`})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr
	client := &http.Client{Timeout: waittest.D(waittest.Default)}
	do := func(method, path string, body any) (int, map[string]any) {
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
	code, ws := do("POST", "/v1/workspaces", map[string]any{"name": "hook-ws", "path": wsPath})
	if code != 201 {
		t.Fatalf("workspace %d %v", code, ws)
	}
	wsID, _ := ws["id"].(string)
	code, run := do("POST", "/v1/jobs/agents%2Fops%2Fdemo/run", map[string]any{"workspace": wsID, "prompt_override": "work"})
	if code != 201 {
		t.Fatalf("run create %d %v", code, run)
	}
	runID, _ := run["id"].(string)
	if got := svc.SessionState(runID); got != runs.SessionRunning {
		t.Fatalf("state after launch: %s", got)
	}
	// Hook stop is the old idle signal; on a CLI run it must not flip state.
	code, _ = do("POST", "/v1/agent-hook/stop?run_id="+runID, map[string]any{"payload": "{}"})
	if code != 200 {
		t.Fatalf("hook %d", code)
	}
	if got := svc.SessionState(runID); got != runs.SessionRunning {
		t.Fatalf("hook stop flipped CLI state: %s", got)
	}
	// Hook waiting must not flip state on a CLI run, and it opens no
	// approval: the wire opens a keyed row for each real permission.
	code, _ = do("POST", "/v1/agent-hook/waiting?run_id="+runID, map[string]any{"payload": `{"question":"?"}`})
	if code != 200 {
		t.Fatalf("hook %d", code)
	}
	if got := svc.SessionState(runID); got != runs.SessionRunning {
		t.Fatalf("hook waiting flipped CLI state: %s", got)
	}
	code, _ = do("POST", "/v1/agent-hook/waiting?run_id="+runID, map[string]any{"payload": `{"question":"?"}`})
	if code != 200 {
		t.Fatalf("hook %d", code)
	}
	list, err := svc.ListApprovals(t.Context(), runs.ApprovalListOpts{State: "pending", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range list {
		if a.RunID == runID {
			n++
		}
	}
	if n != 0 {
		t.Fatalf("hook waiting approvals on CLI run: %d", n)
	}
	if code, _ := do("POST", "/v1/runs/"+runID+"/kill", nil); code != 200 {
		t.Fatalf("kill %d", code)
	}
	waittest.Until(t, "the CLI runs to end", func() bool { return svc.CLIActiveCount() == 0 })
}
