package connectors_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/oauth/oauthtest"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/waittest"
)

type fakeElicitor struct {
	values string
	ok     bool
	schema chan json.RawMessage
}

// proxyStack is a logged-in online connection behind a proxy server.
type proxyStack struct {
	svc   *connectors.Service
	srv   *oauthtest.Server
	c     connectors.Connection
	ask   *fakeAsker
	proxy *httptest.Server
}

func (e fakeElicitor) Elicit(_ context.Context, _, _, _ string, schema json.RawMessage) (json.RawMessage, bool, error) {
	e.schema <- schema
	return json.RawMessage(e.values), e.ok, nil
}

func (ps *proxyStack) post(t *testing.T, path, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ps.proxy.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestMCPProxyForward(t *testing.T) {
	ps := newProxyStack(t, connectors.MCPCall{}, nil)
	var initBody string
	ps.srv.SetMCP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"initialize"`) {
			initBody = string(b)
		}
		if r.Header.Get("Mcp-Session-Id") != "s1" && !strings.Contains(string(b), `"initialize"`) {
			t.Errorf("session id not passed: %q", r.Header.Get("Mcp-Session-Id"))
		}
		w.Header().Set("Mcp-Session-Id", "s1")
		w.Header().Set("Content-Type", "application/json")
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(b, &m)
		switch m.Method {
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"echo"},{"name":"drop"}]}}`, m.ID)
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, m.ID)
		}
	}))
	resp, _ := ps.post(t, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{}}}`, nil)
	if resp.Header.Get("Mcp-Session-Id") != "s1" || !strings.Contains(initBody, `"elicitation":{}`) {
		t.Fatalf("initialize: session %q, upstream body %s", resp.Header.Get("Mcp-Session-Id"), initBody)
	}
	ctx := context.Background()
	ops := connectors.NewOperations(ps.svc, nil, nil)
	if err := ops.SetState(ctx, ps.c.ID, "drop", manifest.OpOff); err != nil {
		t.Fatal(err)
	}
	_, body := ps.post(t, "", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, map[string]string{"Mcp-Session-Id": "s1"})
	if !strings.Contains(body, `"echo"`) || strings.Contains(body, `"drop"`) {
		t.Fatalf("tools/list = %s", body)
	}
	_, body = ps.post(t, "", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"drop"}}`, map[string]string{"Mcp-Session-Id": "s1"})
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "is off") {
		t.Fatalf("call of an off tool = %s", body)
	}
}

func TestMCPProxyRefreshAndLogin(t *testing.T) {
	ps := newProxyStack(t, connectors.MCPCall{}, nil)
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`

	ps.srv.ExpireAccess()
	before := ps.srv.Refreshes()
	_, body := ps.post(t, "", call, nil)
	if !strings.Contains(body, `"result"`) || ps.srv.Refreshes() != before+1 {
		t.Fatalf("after 401: %s (refreshes %d → %d)", body, before, ps.srv.Refreshes())
	}

	ps.srv.NeedScope("write")
	_, body = ps.post(t, "", call, nil)
	if !strings.Contains(body, "Log in again with more access to Granola") {
		t.Fatalf("after 403 = %s", body)
	}
	if got := loginState(t, ps.svc, ps.c.ID); got != "auth_pending" {
		t.Fatalf("state after step-up = %q", got)
	}
	row, _ := ps.svc.Get(ps.c.ID)
	f, ok := ps.svc.FindFlow(ps.c.ID, fmt.Sprint(row["login"].(map[string]any)["flow_id"]))
	if !ok {
		t.Fatalf("no step-up flow in %v", row["login"])
	}
	if u, _ := url.Parse(f.Current().Step.URL); !strings.Contains(u.Query().Get("scope"), "write") {
		t.Fatalf("step-up address = %s", u)
	}

	ps.srv.NeedScope("")
	ps.srv.ExpireAccess()
	ps.srv.FailRefresh(true)
	_, body = ps.post(t, "", call, nil)
	if !strings.Contains(body, "Log in again to Granola") {
		t.Fatalf("after a failed refresh = %s", body)
	}
}

func TestMCPProxyAsk(t *testing.T) {
	ps := newProxyStack(t, connectors.MCPCall{RunID: "run1", Agent: true}, nil)
	if err := connectors.NewOperations(ps.svc, nil, nil).SetState(context.Background(), ps.c.ID, "echo", manifest.OpAsk); err != nil {
		t.Fatal(err)
	}
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`
	_, body := ps.post(t, "", call, nil)
	if !strings.Contains(body, "request req_1 is open") || len(ps.ask.asked) != 1 || !strings.HasPrefix(ps.ask.asked[0], "operation:"+ps.c.ID+"/echo/") {
		t.Fatalf("ask = %s, asked %v", body, ps.ask.asked)
	}
	ps.ask.approved[ps.ask.asked[0]] = true
	if _, body = ps.post(t, "", call, nil); !strings.Contains(body, `"result"`) {
		t.Fatalf("approved call = %s", body)
	}
}

func TestMCPProxyAccess(t *testing.T) {
	svc, srv, c := onlineStack(t, oauthtest.Options{DCR: true})
	p := connectors.NewMCPProxy(connectors.NewOperations(svc, fakeKit{"run1": {Connectors: []string{"other"}}}, nil), nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/connectors/x/mcp", strings.NewReader(`{}`))
	if err := p.Serve(rec, req, connectors.MCPCall{Ref: c.ID, RunID: "run1"}); !errors.Is(err, connectors.ErrConnectorNotKit) {
		t.Fatalf("other run: %v", err)
	}
	// The event read grant gives no MCP access.
	p = connectors.NewMCPProxy(connectors.NewOperations(svc, fakeKit{"run1": {Trigger: c.ID}}, nil), nil, nil)
	if err := p.Serve(rec, req, connectors.MCPCall{Ref: c.ID, RunID: "run1"}); !errors.Is(err, connectors.ErrConnectorNotKit) {
		t.Fatalf("grant: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/connectors/x/mcp?endpoint="+url.QueryEscape("https://evil.example/messages"), strings.NewReader(`{}`))
	if err := p.Serve(rec, req, connectors.MCPCall{Ref: c.ID}); !errors.Is(err, connectors.ErrMCPEndpoint) {
		t.Fatalf("other origin: %v", err)
	}
	_ = srv
}

func TestMCPProxyStreamAndElicit(t *testing.T) {
	el := fakeElicitor{values: `{"name":"Ana"}`, ok: true, schema: make(chan json.RawMessage, 1)}
	ps := newProxyStack(t, connectors.MCPCall{RunID: "run1", Agent: true}, el)
	answered := make(chan string, 1)
	release := make(chan struct{})
	ps.srv.SetMCP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"result"`) {
			answered <- string(b)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		f.Flush()
		<-release
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"e1\",\"method\":\"elicitation/create\",\"params\":{\"message\":\"Your name?\",\"requestedSchema\":{\"type\":\"object\",\"properties\":{\"name\":{\"type\":\"string\",\"minLength\":1}},\"required\":[\"name\"]}}}\n\n")
		f.Flush()
		select {
		case a := <-answered:
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"answer\":%q}}\n\n", a)
		case <-time.After(waittest.D(5 * time.Second)):
		}
		f.Flush()
	}))
	req, _ := http.NewRequest(http.MethodPost, ps.proxy.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`))
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	rd := bufio.NewReader(resp.Body)
	first, err := rd.ReadString('\n')
	if err != nil || !strings.Contains(first, "event: message") {
		t.Fatalf("first line before the rest = %q, %v", first, err)
	}
	close(release)
	rest, _ := io.ReadAll(rd)
	if strings.Contains(string(rest), "elicitation/create") {
		t.Fatalf("elicitation reached the run: %s", rest)
	}
	if !strings.Contains(string(rest), `\"action\":\"accept\"`) || !strings.Contains(string(rest), `Ana`) {
		t.Fatalf("answer = %s", rest)
	}
	if s := string(<-el.schema); strings.Contains(s, "minLength") || !strings.Contains(s, `"required":["name"]`) {
		t.Fatalf("schema = %s", s)
	}
}

func TestMCPProxyElicitDecline(t *testing.T) {
	ps := newProxyStack(t, connectors.MCPCall{}, fakeElicitor{ok: true, schema: make(chan json.RawMessage, 1)})
	answered := make(chan string, 1)
	ps.srv.SetMCP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"result"`) {
			answered <- string(b)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"elicitation/create\",\"params\":{\"message\":\"ok?\"}}\n\n")
	}))
	ps.post(t, "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`, nil)
	select {
	case a := <-answered:
		if !strings.Contains(a, `"decline"`) {
			t.Fatalf("answer without a run = %s", a)
		}
	case <-time.After(waittest.D(5 * time.Second)):
		t.Fatal("no answer")
	}
}

func TestMCPProxyLegacySSE(t *testing.T) {
	ps := newProxyStack(t, connectors.MCPCall{}, nil)
	posted := make(chan string, 1)
	ps.srv.SetMCP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: endpoint\ndata: /mcp?s=1\n\n")
			return
		}
		b, _ := io.ReadAll(r.Body)
		posted <- r.URL.String() + " " + string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	resp, err := http.Get(ps.proxy.URL + "/v1/connectors/x/mcp")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck,gosec // test
	line := strings.TrimSpace(strings.TrimPrefix(strings.Split(string(b), "\n")[1], "data: "))
	if !strings.HasPrefix(line, "/v1/connectors/x/mcp?endpoint=") {
		t.Fatalf("endpoint event = %q", b)
	}
	if resp, _ := ps.post(t, line, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("post status = %d", resp.StatusCode)
	}
	if got := <-posted; !strings.HasPrefix(got, "/mcp?s=1 ") {
		t.Fatalf("upstream post = %q", got)
	}
}

func TestMCPProxyCancel(t *testing.T) {
	ps := newProxyStack(t, connectors.MCPCall{}, nil)
	gone := make(chan struct{})
	ps.srv.SetMCP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(gone)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ps.proxy.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close() //nolint:errcheck,gosec // test
	select {
	case <-gone:
	case <-time.After(waittest.D(5 * time.Second)):
		t.Fatal("the upstream request did not end")
	}
}

func TestOnlineRunModes(t *testing.T) {
	svc, srv, c := onlineStack(t, oauthtest.Options{DCR: true})
	ctx := context.Background()
	if _, err := svc.ResolveActions(ctx, "", []string{c.ID}); !errors.Is(err, runs.ErrConnectorLogin) {
		t.Fatalf("launch before login: %v", err)
	}
	_, ret := startOAuth(ctx, t, svc, srv, c.ID)
	if _, err := svc.FinishLogin(ctx, returnOf(ret)); err != nil {
		t.Fatal(err)
	}
	acts, err := svc.ResolveActions(ctx, "", []string{c.ID})
	if err != nil {
		t.Fatal(err)
	}
	a := acts[0]
	if !a.RunToken || a.MCP.URL != "http://127.0.0.1:7433/v1/connectors/"+c.ID+"/mcp" || a.MCP.Headers["Authorization"] != "" || a.Note != "" {
		t.Fatalf("proxy action = %+v, server %+v", a, a.MCP)
	}
	if _, err := svc.Patch(ctx, c.ID, connectors.ConnectionPatch{OAuth: json.RawMessage(`{"mode":"header"}`)}); err != nil {
		t.Fatal(err)
	}
	acts, err = svc.ResolveActions(ctx, "", []string{c.ID})
	if err != nil {
		t.Fatal(err)
	}
	a = acts[0]
	if a.RunToken || a.MCP.URL != srv.MCPURL() || !strings.HasPrefix(a.MCP.Headers["Authorization"], "Bearer ") || !strings.Contains(a.Note, "The token of Granola expires at ") {
		t.Fatalf("header action = %+v, server %+v", a, a.MCP)
	}
}

func newProxyStack(t *testing.T, call connectors.MCPCall, elicit connectors.Elicitor) *proxyStack {
	t.Helper()
	svc, srv, c := onlineStack(t, oauthtest.Options{DCR: true})
	ctx := context.Background()
	_, ret := startOAuth(ctx, t, svc, srv, c.ID)
	if _, err := svc.FinishLogin(ctx, returnOf(ret)); err != nil {
		t.Fatal(err)
	}
	ps := &proxyStack{svc: svc, srv: srv, c: c, ask: &fakeAsker{approved: map[string]bool{}}}
	p := connectors.NewMCPProxy(connectors.NewOperations(svc, fakeKit{"run1": {Connectors: []string{c.ID}}}, ps.ask), elicit, nil).WithElicitWait(5 * time.Second)
	if call.Ref == "" {
		call.Ref = c.ID
	}
	ps.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := call
		call.Self = r.URL.Path
		if err := p.Serve(w, r, call); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
		}
	}))
	t.Cleanup(ps.proxy.Close)
	return ps
}
