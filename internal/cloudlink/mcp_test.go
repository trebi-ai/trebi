package cloudlink

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/trebi-ai/trebi/internal/waittest"
)

// mcpSink answers forwarded MCP requests. A non-nil gate blocks each call
// until the gate closes.
type mcpSink struct {
	gate  chan struct{}
	reply string

	mu    sync.Mutex
	calls []McpRequest
}

func (m *mcpSink) ServeRemoteMCP(ctx context.Context, f McpRequest) McpResponse {
	m.mu.Lock()
	m.calls = append(m.calls, f)
	m.mu.Unlock()
	if m.gate != nil {
		select {
		case <-m.gate:
		case <-ctx.Done():
		}
	}
	return McpResponse{Status: http.StatusOK, Headers: map[string]string{"content-type": "application/json"}, Body: m.reply}
}

func (m *mcpSink) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func helloCaps(m map[string]any) []string {
	raw, _ := m["caps"].([]any)
	out := []string{}
	for _, c := range raw {
		s, _ := c.(string)
		out = append(out, s)
	}
	return out
}

// TestSocketHelloCapsAndToggle: remote MCP on plus a sink sends caps
// ["mcp"]. Turning it off redials, and the new hello has no caps.
func TestSocketHelloCapsAndToggle(t *testing.T) {
	testHome(t)
	st := openStore(t)
	var mu sync.Mutex
	var hellos [][]string
	fc := newFakeCloud(t, func(fc *fakeCloud, conn *websocket.Conn, session int) {
		m, ok := fc.recv(conn)
		if !ok {
			return
		}
		mu.Lock()
		hellos = append(hellos, helloCaps(m))
		mu.Unlock()
		fc.send(conn, HelloOK{V: ContractV, Type: "hello_ok", Plan: "pro_cloud", HeartbeatInterval: 60, McpURL: "https://staging-mcp.trebi.ai"})
		fc.drain(conn, time.Minute)
	})
	svc := New(st).WithMCP(&mcpSink{})
	svc.Configure(Settings{APIURLs: []string{fc.srv.URL}, Enabled: true, RemoteMCP: true})
	defer svc.Stop()
	if _, err := svc.Link(context.Background(), "ABCD-EFGH", "mac-mini"); err != nil {
		t.Fatal(err)
	}
	waittest.Until(t, "mcp url", func() bool { return svc.McpURL() == "https://staging-mcp.trebi.ai" })
	if got := svc.Status(); !got.RemoteMCP || got.McpURL != "https://staging-mcp.trebi.ai" {
		t.Fatalf("status = %+v", got)
	}
	svc.SetRemoteMCP(false)
	waittest.Until(t, "second hello", func() bool { mu.Lock(); defer mu.Unlock(); return len(hellos) >= 2 })
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(hellos[0], ",") != CapMCP {
		t.Errorf("first hello caps = %v, want [mcp]", hellos[0])
	}
	if len(hellos[1]) != 0 {
		t.Errorf("second hello caps = %v, want none", hellos[1])
	}
}

// TestSocketNoSinkNoCaps: the setting without a sink advertises nothing.
func TestSocketNoSinkNoCaps(t *testing.T) {
	svc := &Service{cfg: Settings{RemoteMCP: true}}
	if svc.remoteMCP() {
		t.Fatal("remoteMCP true without a sink")
	}
	svc.mcp = &mcpSink{}
	if !svc.remoteMCP() {
		t.Fatal("remoteMCP false with a sink and the setting on")
	}
}

// TestSocketMcpRoundTrip: an mcp_request gets one mcp_response with the
// same id, and heartbeats still flow while the sink is slow.
func TestSocketMcpRoundTrip(t *testing.T) {
	testHome(t)
	st := openStore(t)
	sink := &mcpSink{gate: make(chan struct{}), reply: `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`}
	got := make(chan map[string]any, 1)
	fc := newFakeCloud(t, func(fc *fakeCloud, conn *websocket.Conn, session int) {
		fc.recv(conn) // hello
		fc.send(conn, HelloOK{V: ContractV, Type: "hello_ok", Plan: "pro_cloud", HeartbeatInterval: 1})
		fc.send(conn, McpRequest{
			V: ContractV, Type: "mcp_request", ID: "req-1", Client: "https://claude.ai/x",
			Scope: []string{"mcp:read"}, Headers: map[string]string{"content-type": "application/json"},
			Body: `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		})
		released := false
		for {
			m, ok := fc.recv(conn)
			if !ok {
				return
			}
			if m["type"] == "heartbeat" && !released {
				close(sink.gate) // the read loop kept going during the slow call
				released = true
			}
			if m["type"] == "mcp_response" {
				got <- m
				fc.drain(conn, time.Minute)
				return
			}
		}
	})
	svc := New(st).WithMCP(sink)
	svc.Configure(Settings{APIURLs: []string{fc.srv.URL}, Enabled: true, RemoteMCP: true})
	defer svc.Stop()
	if _, err := svc.Link(context.Background(), "ABCD-EFGH", "mac-mini"); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m["id"] != "req-1" || m["status"] != float64(200) || !strings.Contains(m["body"].(string), `"tools"`) {
			t.Fatalf("mcp_response = %v", m)
		}
	case <-time.After(waittest.D(10 * time.Second)):
		t.Fatal("no mcp_response")
	}
}

type loginSink struct{}

func (loginSink) ReturnLogin(_ context.Context, f LoginReturn) LoginResult {
	if f.Code == "" {
		return LoginResult{Status: LoginGone}
	}
	return LoginResult{Status: LoginDone, Title: "Granola"}
}

// TestSocketLoginReturn: each login_return gets one login_result with the
// same id. A daemon with no sink answers failed.
func TestSocketLoginReturn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sink   LoginSink
		code   string
		status string
	}{
		{"done", loginSink{}, "ac_1", LoginDone},
		{"gone", loginSink{}, "", LoginGone},
		{"no sink", nil, "ac_1", LoginFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testHome(t)
			st := openStore(t)
			got := make(chan map[string]any, 1)
			fc := newFakeCloud(t, func(fc *fakeCloud, conn *websocket.Conn, session int) {
				fc.recv(conn) // hello
				fc.send(conn, HelloOK{V: ContractV, Type: "hello_ok", Plan: "pro_cloud", HeartbeatInterval: 60})
				fc.send(conn, LoginReturn{V: ContractV, Type: "login_return", ID: "lr-1", State: "dev.abc", Code: tc.code})
				for {
					m, ok := fc.recv(conn)
					if !ok {
						return
					}
					if m["type"] == "login_result" {
						got <- m
						fc.drain(conn, time.Minute)
						return
					}
				}
			})
			svc := New(st)
			if tc.sink != nil {
				svc.WithLogin(tc.sink)
			}
			svc.Configure(Settings{APIURLs: []string{fc.srv.URL}, Enabled: true})
			defer svc.Stop()
			if _, err := svc.Link(context.Background(), "ABCD-EFGH", "mac-mini"); err != nil {
				t.Fatal(err)
			}
			select {
			case m := <-got:
				if m["id"] != "lr-1" || m["status"] != tc.status {
					t.Fatalf("login_result = %v", m)
				}
			case <-time.After(waittest.D(10 * time.Second)):
				t.Fatal("no login_result")
			}
		})
	}
}

func rpcErrorCode(t *testing.T, r McpResponse) int {
	t.Helper()
	var body struct {
		ID    int `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatalf("body %q: %v", r.Body, err)
	}
	if body.ID != 7 {
		t.Fatalf("error id = %d, want 7", body.ID)
	}
	return body.Error.Code
}

// TestSocketMcpOffAndLimits covers the replies that the daemon builds.
func TestSocketMcpOffAndLimits(t *testing.T) {
	req := McpRequest{ID: "r", Body: `{"jsonrpc":"2.0","id":7,"method":"tools/call"}`}

	off := &socket{svc: &Service{mcp: &mcpSink{}}, mcpSlots: make(chan struct{}, mcpSlotCount)}
	if r := off.serveMcp(context.Background(), req); r.Status != 200 || rpcErrorCode(t, r) != mcpCodeOff {
		t.Fatalf("off reply = %+v", r)
	}
	note := McpRequest{Body: `{"jsonrpc":"2.0","method":"notifications/initialized"}`}
	if r := off.serveMcp(context.Background(), note); r.Status != http.StatusAccepted || r.Body != "" {
		t.Fatalf("off notification = %+v", r)
	}

	sink := &mcpSink{gate: make(chan struct{}), reply: strings.Repeat("x", mcpBodyCap+1)}
	sk := &socket{svc: &Service{cfg: Settings{RemoteMCP: true}, mcp: sink}, mcpSlots: make(chan struct{}, mcpSlotCount)}

	big := req
	big.Body = strings.Repeat(" ", mcpBodyCap+1)
	if r := sk.serveMcp(context.Background(), big); r.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("big request status = %d", r.Status)
	}

	var wg sync.WaitGroup
	results := make(chan McpResponse, mcpSlotCount)
	for range mcpSlotCount {
		wg.Add(1)
		go func() { defer wg.Done(); results <- sk.serveMcp(context.Background(), req) }()
	}
	waittest.Until(t, "slots full", func() bool { return sink.count() == mcpSlotCount })
	if r := sk.serveMcp(context.Background(), req); rpcErrorCode(t, r) != mcpCodeBusy {
		t.Fatalf("ninth reply = %+v", r)
	}
	close(sink.gate)
	wg.Wait()
	close(results)
	for r := range results {
		if r.Status != http.StatusBadGateway {
			t.Fatalf("oversized reply status = %d, want 502", r.Status)
		}
	}
}
