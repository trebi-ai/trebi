package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// asstTestServer answers the routes the assistant CLI touches and records the
// requests it saw, headers included.
type asstTestServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []recReq
}

type recReq struct {
	Method  string
	Path    string
	Query   string
	IfMatch string
	Body    map[string]any
}

func (a *asstTestServer) client() *APIClient {
	return &APIClient{base: a.srv.URL, httpClient: a.srv.Client()}
}

func (a *asstTestServer) requests() []recReq {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]recReq(nil), a.seen...)
}

func (a *asstTestServer) record(r *http.Request) map[string]any {
	body := map[string]any{}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	a.mu.Lock()
	a.seen = append(a.seen, recReq{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		IfMatch: r.Header.Get("If-Match"), Body: body,
	})
	a.mu.Unlock()
	return body
}

func newAsstTestServer(t *testing.T) *asstTestServer {
	t.Helper()
	t.Setenv("TREBI_HOME", t.TempDir())
	a := &asstTestServer{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/workspaces":
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": []any{
				map[string]any{"id": "ws1", "name": "one"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/workspaces/ws1/assistant":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"effective": map[string]any{"harness": "codex", "mode": "auto_edit"},
				"override":  map[string]any{"harness": "codex", "mode": "ask"},
				"hash":      "h1",
			})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/workspaces/ws1/assistant":
			a.record(r)
			_ = json.NewEncoder(w).Encode(map[string]any{"hash": "h2"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/assistant":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"settings":  map[string]any{"harness": "claude", "mode": "auto_edit", "skills": []any{"trebi"}},
				"harnesses": []any{},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/assistant":
			a.record(r)
			_ = json.NewEncoder(w).Encode(map[string]any{"settings": map[string]any{}})
		case r.URL.Path == "/v1/workspaces/ws1/assistant/preview":
			_ = json.NewEncoder(w).Encode(map[string]any{"instructions": "## Role\nYou are the assistant."})
		case r.URL.Path == "/v1/conversations":
			_ = json.NewEncoder(w).Encode(map[string]any{"conversations": []any{
				map[string]any{"id": "c1", "session": map[string]any{
					"harness": "codex", "status": "idle", "turn": 3, "last_message": "done", "updated_at": "t",
				}},
				map[string]any{"id": "c2", "operator": "agents/cmo"},
			}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/conversations/"):
			a.record(r)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/interrupt") || strings.HasSuffix(r.URL.Path, "/close")):
			a.record(r)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/permissions/"):
			a.record(r)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages"):
			a.record(r)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{"id": "m1"}, "delivery": "prompted", "status": "running",
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/channel-pointers/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"conversation_id": "c1"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			if r.URL.RawQuery == "limit=500" {
				_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{
					map[string]any{"seq": 4, "type": "system", "text": "earlier"},
				}})
				return
			}
			// follow=1: replay nothing, stream the live turn, then close.
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := http.NewResponseController(w)
			frames := []string{
				`{"type":"assistant","delta":true,"text":"Work"}`,
				`{"type":"tool","tool":{"name":"mcp__trebi__job","input":"{\"action\":\"list\"}","status":"started"}}`,
				`{"type":"assistant","delta":true,"text":"ing…"}`,
				`{"type":"permission","permission":{"id":"p1","tool":"mcp__trebi__file","question":"Write report.txt?"}}`,
				`{"type":"assistant","text":"Working… done"}`,
				`{"type":"result","result":{"subtype":"success"}}`,
			}
			for _, f := range frames {
				fmt.Fprintf(w, "data: %s\n\n", f)
				_ = flusher.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func TestAssistantShowWorkspaceAndDefaults(t *testing.T) {
	a := newAsstTestServer(t)
	client := a.client()

	out, err := runCLI(t, client, "assistant", "show", "--workspace", "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"hash": "h1"`) || !strings.Contains(out, "codex") {
		t.Fatalf("workspace show %q", out)
	}
	out, err = runCLI(t, client, "assistant", "show", "--defaults")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"settings"`) || !strings.Contains(out, `"harnesses"`) {
		t.Fatalf("defaults show %q", out)
	}
}

func TestAssistantSetWorkspaceOverride(t *testing.T) {
	a := newAsstTestServer(t)
	client := a.client()

	if _, err := runCLI(t, client, "assistant", "set", "--workspace", "ws1", "--harness", "claude"); err != nil {
		t.Fatal(err)
	}
	var put recReq
	for _, r := range a.requests() {
		if r.Method == "PUT" && r.Path == "/v1/workspaces/ws1/assistant" {
			put = r // keep the last one
		}
	}
	if put.Method == "" {
		t.Fatal("no workspace PUT")
	}
	if put.IfMatch != "h1" {
		t.Fatalf("If-Match %q", put.IfMatch)
	}
	ov, _ := put.Body["override"].(map[string]any)
	if ov["harness"] != "claude" {
		t.Fatalf("override body %v", put.Body)
	}
	// The override is a merge layer: keys the GET returned stay.
	if ov["mode"] != "ask" {
		t.Fatalf("kept key lost: %v", ov)
	}

	if _, err := runCLI(t, client, "assistant", "set", "--workspace", "ws1", "--unset", "harness"); err != nil {
		t.Fatal(err)
	}
	put = recReq{}
	for _, r := range a.requests() {
		if r.Method == "PUT" && r.Path == "/v1/workspaces/ws1/assistant" {
			put = r
		}
	}
	ov, _ = put.Body["override"].(map[string]any)
	if _, ok := ov["harness"]; ok {
		t.Fatalf("unset key still present: %v", ov)
	}
	if ov["mode"] != "ask" {
		t.Fatalf("kept key lost: %v", ov)
	}
	if _, err := runCLI(t, client, "assistant", "set", "--workspace", "ws1", "--unset", "bogus"); err == nil {
		t.Fatal("unknown --unset field must fail")
	}
	if _, err := runCLI(t, client, "assistant", "set", "--workspace", "ws1"); err == nil {
		t.Fatal("set with no changes must fail")
	}
}

func TestAssistantSetDefaults(t *testing.T) {
	a := newAsstTestServer(t)
	client := a.client()

	if _, err := runCLI(t, client, "assistant", "set", "--defaults", "--mode", "ask"); err != nil {
		t.Fatal(err)
	}
	var put recReq
	for _, r := range a.requests() {
		if r.Method == "PUT" && r.Path == "/v1/assistant" {
			put = r
		}
	}
	if put.Method == "" {
		t.Fatal("no defaults PUT")
	}
	if put.Body["mode"] != "ask" || put.Body["harness"] != "claude" {
		t.Fatalf("merged settings %v", put.Body)
	}

	if _, err := runCLI(t, client, "assistant", "set", "--defaults", "--unset", "skills"); err != nil {
		t.Fatal(err)
	}
	put = recReq{}
	for _, r := range a.requests() {
		if r.Method == "PUT" && r.Path == "/v1/assistant" {
			put = r // keep the last one
		}
	}
	skills, ok := put.Body["skills"].([]any)
	if !ok || len(skills) != 0 {
		t.Fatalf("unset skills must write an empty list: %v", put.Body)
	}
	if _, err := runCLI(t, client, "assistant", "set", "--defaults", "--disabled"); err == nil {
		t.Fatal("--disabled must be rejected on --defaults")
	}
	if _, err := runCLI(t, client, "assistant", "set", "--defaults", "--unset", "disabled"); err == nil {
		t.Fatal("--unset disabled must be rejected on --defaults")
	}
}

func TestAssistantSessionsFiltersAssistantRows(t *testing.T) {
	a := newAsstTestServer(t)
	client := a.client()

	out, err := runCLI(t, client, "assistant", "sessions", "--workspace", "ws1", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "c2") || strings.Contains(out, "agents/cmo") {
		t.Fatalf("agent conversation leaked: %q", out)
	}
	var env struct {
		Conversations []map[string]any `json:"conversations"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("json %v %q", err, out)
	}
	if len(env.Conversations) != 1 || env.Conversations[0]["id"] != "c1" {
		t.Fatalf("envelope %v", env)
	}
}

func TestAssistantPreviewInterruptCloseDelete(t *testing.T) {
	a := newAsstTestServer(t)
	client := a.client()

	out, err := runCLI(t, client, "assistant", "preview", "--workspace", "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "You are the assistant.") {
		t.Fatalf("preview %q", out)
	}

	if _, err := runCLI(t, client, "assistant", "interrupt", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, client, "assistant", "close", "c1"); err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, r := range a.requests() {
		paths[r.Method+" "+r.Path] = true
	}
	if !paths["POST /v1/conversations/c1/interrupt"] || !paths["POST /v1/conversations/c1/close"] {
		t.Fatalf("lifecycle calls %v", paths)
	}

	if _, err := runCLI(t, client, "conversation", "delete", "c1"); err == nil {
		t.Fatal("delete without --force must refuse")
	}
	if _, err := runCLI(t, client, "conversation", "delete", "c1", "--force"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range a.requests() {
		if r.Method == "DELETE" && r.Path == "/v1/conversations/c1" {
			found = r.Query == "confirm=1"
		}
	}
	if !found {
		t.Fatal("delete must send confirm=1")
	}
}

func TestAssistantChatStreamsTurn(t *testing.T) {
	a := newAsstTestServer(t)
	client := a.client()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })
	fmt.Fprint(w, "hello\nn\n/quit\n")
	_ = w.Close()

	out, err := runCLI(t, client, "assistant", "chat", "--workspace", "ws1")
	if err != nil {
		t.Fatal(err)
	}

	// Deltas and the closed-message remainder interleave with tool lines and
	// the permission prompt, so the pieces are asserted, not one string.
	for _, want := range []string{"Work", "ing…", "→ trebi.job list", " done", "[y]es once / [a]lways / [n]o:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	var sent, answered bool
	for _, req := range a.requests() {
		if req.Method == "POST" && req.Path == "/v1/conversations/c1/messages" && req.Body["text"] == "hello" {
			sent = true
		}
		if req.Method == "POST" && req.Path == "/v1/conversations/c1/permissions/p1" {
			answered = req.Body["decision"] == "deny"
		}
	}
	if !sent || !answered {
		t.Fatalf("sent=%v answered=%v seen=%+v", sent, answered, a.requests())
	}
}
