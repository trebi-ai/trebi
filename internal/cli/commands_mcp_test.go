package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPConfigBlocks(t *testing.T) {
	t.Parallel()
	target := mcpTarget{URL: "http://127.0.0.1:7777/mcp", Token: "tok-1"}
	for _, client := range []string{"claude", "codex", "opencode", "cursor", "vscode", "json"} {
		got, err := target.block(client)
		if err != nil {
			t.Fatalf("%s: %v", client, err)
		}
		golden := filepath.Join("testdata", "mcp", client+".txt")
		if os.Getenv("UPDATE_GOLDEN") != "" {
			if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%s: %v (set UPDATE_GOLDEN=1 to write it)", client, err)
		}
		if got != string(want) {
			t.Errorf("%s config drifted:\ngot:\n%s\nwant:\n%s", client, got, want)
		}
		if !strings.Contains(got, target.URL) {
			t.Errorf("%s block omits the url", client)
		}
	}
	if _, err := target.block("nano"); err == nil {
		t.Error("an unknown client must be an error")
	}
}

func TestMCPAddCommands(t *testing.T) {
	t.Parallel()
	target := mcpTarget{URL: "http://127.0.0.1:7777/mcp", Token: "tok-1"}
	for _, client := range []string{"claude", "codex", "opencode"} {
		argv, err := target.addCommand(client)
		if err != nil || len(argv) < 4 {
			t.Fatalf("%s: %v %v", client, argv, err)
		}
		if !strings.Contains(strings.Join(argv, " "), target.URL) {
			t.Errorf("%s command omits the url: %v", client, argv)
		}
	}
	if _, err := target.addCommand("vscode"); err == nil {
		t.Error("vscode has no add command; it must say so")
	}
}

func TestMCPServeRoundTrip(t *testing.T) {
	t.Parallel()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Mcp-Session-Id", "sess-1")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[]}}\n\n"))
	}))
	defer srv.Close()

	bridge := newMCPBridge(mcpTarget{URL: srv.URL + "/mcp", Token: "tok-1"})
	var out strings.Builder
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if err := bridge.run(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok-1" {
		t.Fatalf("token not forwarded: %q", gotAuth)
	}
	if bridge.session != "sess-1" {
		t.Fatalf("session not kept: %q", bridge.session)
	}
	var reply struct {
		ID     int            `json:"id"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &reply); err != nil {
		t.Fatalf("reply %q: %v", out.String(), err)
	}
	if reply.ID != 1 || reply.Result["tools"] == nil {
		t.Fatalf("unwrapped reply: %+v", reply)
	}
}

func TestMCPServeRetargetsOnPortChange(t *testing.T) {
	t.Parallel()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Mcp-Session-Id"); got != "" {
			t.Errorf("stale session after retarget: %q", got)
		}
		w.Header().Set("Mcp-Session-Id", "sess-new")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer live.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()

	bridge := newMCPBridge(mcpTarget{URL: dead.URL + "/mcp", Token: "old"})
	bridge.session = "sess-old"
	bridge.refresh = func() (mcpTarget, error) {
		return mcpTarget{URL: live.URL + "/mcp", Token: "new"}, nil
	}
	var out strings.Builder
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if err := bridge.run(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	if bridge.target.URL != live.URL+"/mcp" || bridge.target.Token != "new" {
		t.Fatalf("target not updated: %+v", bridge.target)
	}
	if bridge.session != "sess-new" {
		t.Fatalf("session %q", bridge.session)
	}
	if !strings.Contains(out.String(), `"ok":true`) {
		t.Fatalf("reply %q", out.String())
	}
}

func TestMCPServeDoesNotRetrySameEndpoint(t *testing.T) {
	t.Parallel()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()
	refreshes := 0
	bridge := newMCPBridge(mcpTarget{URL: dead.URL + "/mcp", Token: "tok"})
	bridge.refresh = func() (mcpTarget, error) {
		refreshes++
		return mcpTarget{URL: dead.URL + "/mcp", Token: "tok"}, nil
	}
	err := bridge.run(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), io.Discard)
	if err == nil {
		t.Fatal("want transport error")
	}
	if refreshes != 1 {
		t.Fatalf("refreshes %d", refreshes)
	}
}

// The bridge runs on the client machine, so it reads value_file itself. The
// daemon receives the value and the client transcript keeps only the path.
func TestMCPServeResolvesValueFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "api-key.txt")
	if err := os.WriteFile(path, []byte("sk-do-not-leak\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bridge := newMCPBridge(mcpTarget{})
	msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"secret","arguments":{"action":"set","input":{"name":"API_KEY","value_file":%q}}}}`, path)
	out, err := bridge.resolveValueFile([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Params struct {
			Arguments struct {
				Input map[string]any `json:"input"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Params.Arguments.Input["value"] != "sk-do-not-leak" {
		t.Fatalf("value not substituted: %+v", got.Params.Arguments.Input)
	}
	if _, ok := got.Params.Arguments.Input["value_file"]; ok {
		t.Fatal("value_file must not reach the daemon")
	}
}

// A tools/call for another tool, and a message that is not a call, pass through.
func TestMCPServeLeavesOtherCallsAlone(t *testing.T) {
	t.Parallel()
	bridge := newMCPBridge(mcpTarget{})
	for _, msg := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"job","arguments":{"action":"list","input":{}}}}`,
	} {
		out, err := bridge.resolveValueFile([]byte(msg))
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != msg {
			t.Fatalf("changed %s -> %s", msg, out)
		}
	}
}

// attachmentOut strips the client-side path before the daemon sees the call.
func TestMCPBridgeAttachmentOut(t *testing.T) {
	t.Parallel()
	bridge := newMCPBridge(mcpTarget{})
	msg := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"email","arguments":{"action":"attachment","input":{"message":"01A","attachment":"a1","out":"/tmp/x.pdf"}}}}`
	path, stripped := bridge.attachmentOut([]byte(msg))
	if path != "/tmp/x.pdf" {
		t.Fatalf("path %q", path)
	}
	var got struct {
		Params struct {
			Arguments struct {
				Input map[string]any `json:"input"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(stripped, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Params.Arguments.Input["out"]; ok {
		t.Fatal("out must not reach the daemon")
	}
	if got.Params.Arguments.Input["message"] != "01A" {
		t.Fatalf("input mangled: %+v", got.Params.Arguments.Input)
	}

	// A call without out, and a non-attachment call, pass through unchanged.
	for _, m := range []string{
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"email","arguments":{"action":"attachment","input":{"message":"01A","attachment":"a1"}}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"email","arguments":{"action":"messages","input":{}}}}`,
	} {
		path, out := bridge.attachmentOut([]byte(m))
		if path != "" || string(out) != m {
			t.Fatalf("changed %s: path %q out %s", m, path, out)
		}
	}
}

// writeFileReply writes the bytes locally and answers with the path, so the
// content never rides the client transcript. An error reply passes through.
func TestMCPBridgeWriteFileReply(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "invoice.pdf")
	payload, err := json.Marshal(map[string]any{"size": 4, "content_base64": base64.StdEncoding.EncodeToString([]byte("PDF1"))})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 6,
		"result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": string(payload)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bridge := newMCPBridge(mcpTarget{})
	out, err := bridge.writeFileReply(reply, path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "PDF1" {
		t.Fatalf("file %q err %v", raw, err)
	}
	var res struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Result.Content[0].Text, "wrote "+path) {
		t.Fatalf("reply text %q", res.Result.Content[0].Text)
	}

	errReply := []byte(`{"jsonrpc":"2.0","id":7,"result":{"isError":true,"content":[{"type":"text","text":"not_found"}]}}`)
	out, err = bridge.writeFileReply(errReply, path)
	if err != nil || string(out) != string(errReply) {
		t.Fatalf("error reply changed: %s %v", out, err)
	}
}
