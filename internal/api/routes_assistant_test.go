package api_test

// Routes of plan 2026-09-26 Part F: assistant settings, sessions, SSE
// follow, and the workspace scope of an assistant session token.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/tools"
)

// writeFakeHarness pins the fake harness in trebi.yaml, so sessions start
// without machine probes.
func writeFakeHarness(t *testing.T, wsDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wsDir, "trebi.yaml"), []byte("assistant:\n  harness: fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls until cond holds or the deadline passes.
func waitForAsst(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAssistantDefaultsRoutes(t *testing.T) {
	_, base := startTestServer(t)

	code, body, _ := doJSON(t, "GET", base+"/v1/assistant", nil)
	if code != 200 {
		t.Fatalf("get defaults %d %v", code, body)
	}
	settings, _ := body["settings"].(map[string]any)
	if settings == nil || settings["mode"] == "" {
		t.Fatalf("settings missing: %v", body)
	}
	harnesses, _ := body["harnesses"].([]any)
	if len(harnesses) == 0 {
		t.Fatalf("harnesses missing: %v", body)
	}
	names := map[string]bool{}
	for _, h := range harnesses {
		names[fmt.Sprint(h.(map[string]any)["name"])] = true
	}
	if !names["native"] || names["fake"] {
		t.Fatalf("harness list must carry native and drop fake: %v", names)
	}

	// PUT writes the defaults; an unknown skill is a 400.
	code, body, _ = doJSON(t, "PUT", base+"/v1/assistant", map[string]any{
		"harness": "fake", "mode": "ask", "skills": []string{"nope"},
	})
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("unknown skill %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PUT", base+"/v1/assistant", map[string]any{
		"harness": "fake", "mode": "ask", "effort": "low", "context": "Be brief.",
	})
	if code != 200 {
		t.Fatalf("put defaults %d %v", code, body)
	}
	if got := settings2(t, body)["mode"]; got != "ask" {
		t.Fatalf("mode %v", got)
	}
}

func settings2(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	s, _ := body["settings"].(map[string]any)
	if s == nil {
		t.Fatalf("no settings in %v", body)
	}
	return s
}

func TestWorkspaceAssistantSettingsRoutes(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "set-ws")

	// GET answers 200 with effective, override, and hash.
	code, body, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/assistant", nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, body)
	}
	hash, _ := body["hash"].(string)
	if hash == "" {
		t.Fatalf("hash missing: %v", body)
	}
	eff, _ := body["effective"].(map[string]any)
	if eff == nil || eff["resolved"] == "" {
		t.Fatalf("effective missing: %v", body)
	}

	// A stale If-Match is 412.
	code, body, _ = doJSONHeaders(t, "PUT", base+"/v1/workspaces/"+wsID+"/assistant",
		map[string]any{"override": map[string]any{"harness": strPtr("fake")}},
		map[string]string{"If-Match": "deadbeef"})
	if code != 412 {
		t.Fatalf("stale if-match %d %v", code, body)
	}

	// The current hash writes the override and returns the new hash.
	code, body, _ = doJSONHeaders(t, "PUT", base+"/v1/workspaces/"+wsID+"/assistant",
		map[string]any{"override": map[string]any{"harness": strPtr("fake"), "name": strPtr("ellis")}},
		map[string]string{"If-Match": hash})
	if code != 200 {
		t.Fatalf("put %d %v", code, body)
	}
	nextHash, _ := body["hash"].(string)
	if nextHash == "" || nextHash == hash {
		t.Fatalf("hash %q did not advance", nextHash)
	}
	if eff = body["effective"].(map[string]any); eff["name"] != "ellis" {
		t.Fatalf("override not applied: %v", eff["name"])
	}

	// The preview returns composed instructions with the context section.
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/assistant/preview", nil)
	if code != 200 {
		t.Fatalf("preview %d %v", code, body)
	}
	if ins, _ := body["instructions"].(string); ins == "" {
		t.Fatalf("instructions empty: %v", body)
	}
}

func strPtr(s string) *string { return &s }

// doJSONHeaders is doJSON with caller-chosen request headers.
func doJSONHeaders(t *testing.T, method, url string, body any, headers map[string]string) (int, map[string]any, http.Header) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, merr := json.Marshal(body)
		if merr != nil {
			t.Fatal(merr)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

func TestSessionRoutesLifecycle(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "sess-ws")
	writeFakeHarness(t, wsDir)

	// Create with a pinned mode; the pin lands on the session row.
	code, conv, _ := doJSON(t, "POST", base+"/v1/conversations", map[string]any{
		"workspace": wsID, "origin_channel": "ui", "mode": "ask",
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, conv)
	}
	id, _ := conv["id"].(string)

	// Interrupt before any process: 409.
	code, body, _ := doJSON(t, "POST", base+"/v1/conversations/"+id+"/interrupt", nil)
	if code != 409 {
		t.Fatalf("interrupt idle %d %v", code, body)
	}

	// Send one turn.
	code, sent, _ := doJSON(t, "POST", base+"/v1/conversations/"+id+"/messages", map[string]any{
		"text": "hello", "channel_type": "ui", "channel_ref": "ui-1",
	})
	if code != 201 || sent["delivery"] != "prompted" {
		t.Fatalf("send %d %v", code, sent)
	}
	waitForAsst(t, 5*time.Second, "finished session", func() bool {
		code, c, _ := doJSON(t, "GET", base+"/v1/conversations/"+id, nil)
		if code != 200 {
			return false
		}
		sess, _ := c["session"].(map[string]any)
		return sess != nil && (sess["status"] == assistant.StatusIdle || sess["status"] == assistant.StatusClosed)
	})

	// The stored transcript holds the user event and the assistant reply.
	code, body, _ = doJSON(t, "GET", base+"/v1/conversations/"+id+"/events", nil)
	if code != 200 {
		t.Fatalf("events %d %v", code, body)
	}
	events, _ := body["events"].([]any)
	if len(events) < 2 {
		t.Fatalf("events %v", body)
	}

	// Mode applies to the next session and says so.
	req, _ := http.NewRequest("POST", base+"/v1/conversations/"+id+"/mode",
		strings.NewReader(`{"mode":"auto"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 || resp.Header.Get("X-Trebi-Applies") != "next_session" {
		t.Fatalf("mode %d %v", resp.StatusCode, resp.Header)
	}

	// Close ends the process; the conversation stays.
	code, body, _ = doJSON(t, "POST", base+"/v1/conversations/"+id+"/close", nil)
	if code != 204 {
		t.Fatalf("close %d %v", code, body)
	}
	code, c, _ := doJSON(t, "GET", base+"/v1/conversations/"+id, nil)
	sess, _ := c["session"].(map[string]any)
	if code != 200 || sess == nil || sess["status"] != assistant.StatusClosed {
		t.Fatalf("closed session %d %v", code, c)
	}

	// Revert with no snapshot is 404.
	code, body, _ = doJSON(t, "POST", base+"/v1/conversations/"+id+"/revert",
		map[string]any{"turn": 1, "path": "jobs/x/JOB.md"})
	if code != 404 {
		t.Fatalf("revert no snapshot %d %v", code, body)
	}

	// DELETE removes the conversation, its events, and its session.
	code, body, _ = doJSON(t, "DELETE", base+"/v1/conversations/"+id, nil)
	if code != 204 {
		t.Fatalf("delete %d %v", code, body)
	}
	code, _, _ = doJSON(t, "GET", base+"/v1/conversations/"+id, nil)
	if code != 404 {
		t.Fatalf("deleted conversation %d", code)
	}
}

func TestConversationEventsFollowReplaysAndStreamsLive(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "sse-ws")
	writeFakeHarness(t, wsDir)

	code, conv, _ := doJSON(t, "POST", base+"/v1/conversations", map[string]any{
		"workspace": wsID, "origin_channel": "ui",
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, conv)
	}
	id, _ := conv["id"].(string)
	if _, err := s.Assistant().Prompt(context.Background(), id, assistant.PromptInput{
		Text: "one", ChannelType: "ui", ChannelRef: "ui-1",
	}); err != nil {
		t.Fatal(err)
	}
	waitForAsst(t, 5*time.Second, "first turn", func() bool {
		rec, err := s.Assistant().Session(context.Background(), id)
		return err == nil && (rec.Status == assistant.StatusIdle || rec.Status == assistant.StatusClosed)
	})

	// Follow from before the stored events: the replay carries them.
	req, _ := http.NewRequest("GET", base+"/v1/conversations/"+id+"/events?follow=1&after=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("follow headers %d %v", resp.StatusCode, resp.Header)
	}

	// A second prompt streams live while the follower reads.
	done := make(chan struct{})
	promptErr := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := s.Assistant().Prompt(context.Background(), id, assistant.PromptInput{
			Text: "two", ChannelType: "ui", ChannelRef: "ui-1",
		})
		promptErr <- err
	}()

	// One read covers both halves: the replay of turn one (stored seq) and
	// turn two arriving live on the same follower.
	body := readSSE(t, resp, 15*time.Second, 2)
	<-done
	if err := <-promptErr; err != nil {
		t.Fatalf("second prompt: %v", err)
	}
	if !strings.Contains(body, "event: assistant") {
		t.Fatalf("no assistant frames:\n%.400s", body)
	}
	if !strings.Contains(body, `"seq":1,`) {
		t.Fatalf("replay missed the first turn:\n%.400s", body)
	}
	if strings.Count(body, `"type":"result"`) < 2 {
		t.Fatalf("live stream missed the second turn:\n%.400s", body)
	}
}

// readSSE drains one SSE response until resultCount result frames appear or
// the deadline passes, then reports the accumulated body.
func readSSE(t *testing.T, resp *http.Response, d time.Duration, resultCount int) string {
	t.Helper()
	deadline := time.Now().Add(d)
	var buf bytes.Buffer
	one := make([]byte, 4096)
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(one)
		if n > 0 {
			buf.Write(one[:n])
			if strings.Count(buf.String(), `"type":"result"`) >= resultCount {
				return buf.String()
			}
		}
		if err != nil {
			return buf.String()
		}
	}
	return buf.String()
}

// doJSONHeader is doJSON with an X-Trebi-Run-Token header.
func doJSONHeader(t *testing.T, method, url string, body any, token string) (int, map[string]any, http.Header) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, merr := json.Marshal(body)
		if merr != nil {
			t.Fatal(merr)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(tools.HeaderRunToken, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

func TestAssistantTokenScopeOnWorkspaceRoutes(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "tok-ws")
	writeFakeHarness(t, wsDir)
	otherDir := t.TempDir()
	otherID := addWorkspace(t, base, otherDir, "other-ws")

	// The pinned harness pre-creates the session row the token belongs to.
	code, conv, _ := doJSON(t, "POST", base+"/v1/conversations", map[string]any{
		"workspace": wsID, "origin_channel": "cli", "harness": "fake",
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, conv)
	}
	id, _ := conv["id"].(string)

	// Mint a session token the way the manager does and store its hash.
	token, hash, err := assistant.MintSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AssistantStore().Update(context.Background(), id, func(r *assistant.Record) {
		r.ActorTokenHash = hash
		r.Status = assistant.StatusRunning
	}); err != nil {
		t.Fatal(err)
	}
	tokenHdr := map[string]string{tools.HeaderRunToken: token}
	otherHdr := map[string]string{tools.HeaderRunToken: token}

	// The token reaches its own workspace assistant.
	code, _, _ = doJSONHeaders(t, "GET", base+"/v1/workspaces/"+wsID+"/assistant", nil, tokenHdr)
	if code != 200 {
		t.Fatalf("own workspace %d", code)
	}

	// A foreign workspace is forbidden.
	code, body, _ := doJSONHeaders(t, "PUT", base+"/v1/workspaces/"+otherID+"/assistant",
		map[string]any{"override": map[string]any{"harness": strPtr("fake")}}, otherHdr)
	if code != 403 {
		t.Fatalf("foreign put %d %v", code, body)
	}
	code, body, _ = doJSONHeaders(t, "GET", base+"/v1/workspaces/"+otherID+"/assistant", nil, otherHdr)
	if code != 403 {
		t.Fatalf("foreign get %d %v", code, body)
	}

	// Its own conversation answers a prompt.
	code, sent, _ := doJSONHeaders(t, "POST", base+"/v1/workspaces/"+wsID+"/assistant/messages",
		map[string]any{"text": "hi"}, tokenHdr)
	if code != 201 {
		t.Fatalf("own send %d %v", code, sent)
	}

	// A garbage token is 401 everywhere.
	badHdr := map[string]string{tools.HeaderRunToken: "not-a-real-token-value-0000"}
	code, _, _ = doJSONHeaders(t, "GET", base+"/v1/workspaces/"+wsID+"/assistant", nil, badHdr)
	if code != 401 {
		t.Fatalf("bad token %d", code)
	}
}

// TestRunsListRejectsAssistantKind pins F.3: the assistant is not a run.
func TestRunsListRejectsAssistantKind(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "GET", base+"/v1/runs?kind=assistant", nil)
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("runs kind=assistant %d %v", code, body)
	}
}

// TestAssistantNativeProviderRoundTrip covers plan 2026-09-26 G: a PUT with
// a native block is stored, a GET returns it with the key masked and the
// source named, and the picker flips to ready without a restart.
func TestAssistantNativeProviderRoundTrip(t *testing.T) {
	t.Setenv("TREBI_TEST_NATIVE_KEY", "sk-live-123")
	_, base := startTestServer(t)

	code, body, _ := doJSON(t, "PUT", base+"/v1/assistant", map[string]any{
		"harness": "native",
		"model":   "claude-sonnet-5",
		"native": map[string]any{
			"api_format": "anthropic",
			"base_url":   "http://127.0.0.1:9",
			"api_key":    "sk-live-123",
			"api_key_env": "TREBI_TEST_NATIVE_KEY",
			"max_tokens": 2048,
		},
	})
	if code != 200 {
		t.Fatalf("put defaults %d %v", code, body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/assistant", nil)
	if code != 200 {
		t.Fatalf("get defaults %d %v", code, body)
	}
	native, _ := settings2(t, body)["native"].(map[string]any)
	if native == nil {
		t.Fatalf("no native block in %v", body)
	}
	if native["api_format"] != "anthropic" || native["base_url"] != "http://127.0.0.1:9" {
		t.Fatalf("native block lost fields: %v", native)
	}
	if native["api_key_present"] != true {
		t.Fatalf("api_key_present = %v", native["api_key_present"])
	}
	if native["api_key_source"] == "" || native["api_key_source"] == "none" {
		t.Fatalf("api_key_source = %v", native["api_key_source"])
	}
	if v, leaks := native["api_key"]; leaks && v != nil && v != "" {
		t.Fatalf("api_key must be absent or empty, got %v", v)
	}

	// The picker reads the live config: native is ready now.
	harnesses, _ := body["harnesses"].([]any)
	status := ""
	for _, h := range harnesses {
		m := h.(map[string]any)
		if m["name"] == "native" {
			status = fmt.Sprint(m["status"])
		}
	}
	if status != "ready" {
		t.Fatalf("native status = %q, want ready", status)
	}
}

// TestWorkspaceAssistantPutRejectsNative pins the Defaults-only rule.
func TestWorkspaceAssistantPutRejectsNative(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "native-ws")

	code, body, _ := doJSON(t, "PUT", base+"/v1/workspaces/"+wsID+"/assistant", map[string]any{
		"override": map[string]any{"harness": "fake"},
		"native":   map[string]any{"api_format": "anthropic"},
	})
	if code != 400 || !strings.Contains(fmt.Sprint(body["error"]), "Defaults only") {
		t.Fatalf("native in workspace scope = %d %v", code, body)
	}
}
