package api_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/cloudlink"
)

// remoteTransport sends each MCP client POST through ServeRemoteMCP, the
// way the cloud socket does. extra adds headers that a hostile client
// could send.
type remoteTransport struct {
	srv   *api.Server
	scope []string
	extra map[string]string
}

func (tr remoteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost {
		return &http.Response{StatusCode: http.StatusMethodNotAllowed, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for k := range req.Header {
		headers[strings.ToLower(k)] = req.Header.Get(k)
	}
	for k, v := range tr.extra {
		headers[k] = v
	}
	resp := tr.srv.ServeRemoteMCP(req.Context(), cloudlink.McpRequest{
		V: 1, Type: "mcp_request", ID: "t", Client: "https://claude.ai/oauth/claude-code-client-metadata",
		Scope: tr.scope, Headers: headers, Body: string(body),
	})
	h := http.Header{}
	for k, v := range resp.Headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: resp.Status, Header: h, Body: io.NopCloser(bytes.NewReader([]byte(resp.Body))), Request: req}, nil
}

func remoteSession(t *testing.T, srv *api.Server, tr remoteTransport) *mcp.ClientSession {
	t.Helper()
	tr.srv = srv
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "remote-test", Version: "1"}, nil).Connect(
		context.Background(),
		&mcp.StreamableClientTransport{
			Endpoint:             "https://staging-mcp.trebi.ai",
			HTTPClient:           &http.Client{Transport: tr},
			DisableStandaloneSSE: true,
		}, nil)
	if err != nil {
		t.Fatalf("connect remote: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

var fullScope = []string{"mcp:read", "mcp:write"}

// TestRemoteMCPInitializeAndList: initialize and tools/list over the
// forwarded path return the full catalog.
func TestRemoteMCPInitializeAndList(t *testing.T) {
	srv, _ := startTestServer(t)
	cs := remoteSession(t, srv, remoteTransport{scope: fullScope})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 26 {
		t.Fatalf("tools = %d, want 26", len(res.Tools))
	}
}

// TestRemoteMCPToolCall: a read tool works with a read-only grant.
func TestRemoteMCPToolCall(t *testing.T) {
	srv, _ := startTestServer(t)
	cs := remoteSession(t, srv, remoteTransport{scope: []string{"mcp:read"}})
	res, text := mcpCall(t, cs, "system", "health", nil)
	mcpOK(t, "system.health", res, text)
}

// TestRemoteGateReadOnly: a read-only grant refuses a write with the
// message that the model shows, and still allows reads. The refusal proves
// that the remote actor reaches the /v1 mux through Dispatch.
func TestRemoteGateReadOnly(t *testing.T) {
	srv, _ := startTestServer(t)
	cs := remoteSession(t, srv, remoteTransport{scope: []string{"mcp:read"}})
	res, text := mcpCall(t, cs, "workspace", "create", map[string]any{"path": t.TempDir(), "name": "ro", "create": true})
	if !res.IsError || !strings.Contains(text, "read only") {
		t.Fatalf("read-only create = %v %s", res.IsError, text)
	}
	res, text = mcpCall(t, cs, "workspace", "list", nil)
	mcpOK(t, "workspace.list", res, text)
}

// TestRemoteMCPHeaderAllowlist: run-token and actor headers from the
// client do not change who the caller is. A full grant still writes, and
// the forged token does not cause a 401.
func TestRemoteMCPHeaderAllowlist(t *testing.T) {
	srv, _ := startTestServer(t)
	cs := remoteSession(t, srv, remoteTransport{scope: fullScope, extra: map[string]string{
		"x-trebi-run-token": "forged", "x-trebi-actor": "human", "cookie": "a=b",
	}})
	res, text := mcpCall(t, cs, "workspace", "create", map[string]any{"path": t.TempDir(), "name": "rw", "create": true})
	mcpOK(t, "workspace.create", res, text)
}

// TestRemoteGateDenyConfig: a full grant cannot change cloud.remote_mcp.
func TestRemoteGateDenyConfig(t *testing.T) {
	srv, _ := startTestServer(t)
	cs := remoteSession(t, srv, remoteTransport{scope: fullScope})
	res, text := mcpCall(t, cs, "config", "set", map[string]any{"key": "cloud.remote_mcp", "value": "false"})
	if !res.IsError || !strings.Contains(text, "remote connection cannot") {
		t.Fatalf("config set remote_mcp = %v %s", res.IsError, text)
	}
}
