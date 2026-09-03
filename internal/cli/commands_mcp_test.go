package cli

import (
	"context"
	"encoding/json"
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
