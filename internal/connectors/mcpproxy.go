package connectors

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/oauth"
)

const (
	maxMCPBody = 4 << 20
	// elicitWait bounds one elicitation ask. A run with no person gets
	// decline after it.
	elicitWait = 5 * time.Minute
	// endpointParam carries the legacy SSE post address on the proxy URL.
	endpointParam = "endpoint"
	// JSON-RPC error codes of the proxy.
	rpcBlocked = -32001
	rpcLogin   = -32002
)

// ErrMCPEndpoint is a legacy post address on another origin.
var ErrMCPEndpoint = errors.New("the MCP post address is not on the server origin")

// mcpHeaders pass through the proxy in both directions.
var mcpHeaders = []string{"Content-Type", "Accept", "Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-Id"} //nolint:gochecknoglobals // fixed list, never mutated

// Elicitor asks the person of a run and waits. A nil schema asks for a
// yes or no; ok is false for a no, a dismiss, or a timeout.
type Elicitor interface {
	Elicit(ctx context.Context, runID, title, summary string, schema json.RawMessage) (values json.RawMessage, ok bool, err error)
}

// MCPProxy forwards the MCP traffic of a run to an online server. It adds
// the OAuth token, so the run never holds it. It applies the tool states
// and turns an elicitation into a Trebi ask.
type MCPProxy struct {
	ops    *Operations
	elicit Elicitor
	hc     *http.Client
	wait   time.Duration
	log    *slog.Logger
}

// MCPCall is one request to the proxy. Self is the proxy path, for the
// legacy post address.
type MCPCall struct {
	Ref       string
	Workspace string
	RunID     string
	Agent     bool
	Self      string
}

// mcpTarget is the resolved connection of one call.
type mcpTarget struct {
	call     MCPCall
	r        Resolved
	conn     Connection
	upstream string
	headers  map[string]string // manifest headers, secrets expanded
}

// rpcMessage is one JSON-RPC message.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// rpcBatch is the parsed body of a post.
type rpcBatch struct {
	msgs  []rpcMessage
	batch bool
	lists map[string]bool // ids of tools/list requests
}

// upstreamError is a call that could not reach the server with a token.
// Text is the answer to the run.
type upstreamError struct {
	code int
	text string
}

func NewMCPProxy(ops *Operations, elicit Elicitor, hc *http.Client) *MCPProxy {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &MCPProxy{ops: ops, elicit: elicit, hc: hc, wait: elicitWait, log: slog.Default().With("source", "mcp_proxy")}
}

func (e *upstreamError) Error() string { return e.text }

// WithElicitWait sets the wait of one elicitation ask.
func (p *MCPProxy) WithElicitWait(d time.Duration) *MCPProxy {
	p.wait = d
	return p
}

// Serve forwards one request. It returns an error only before it writes
// the answer: an unknown connection, a run without access, or a
// connection with no OAuth login.
func (p *MCPProxy) Serve(w http.ResponseWriter, r *http.Request, call MCPCall) error {
	t, err := p.target(r.Context(), call, r.URL.Query().Get(endpointParam))
	if err != nil {
		return err
	}
	start := time.Now()
	var body []byte
	var in rpcBatch
	if r.Method == http.MethodPost {
		body, err = io.ReadAll(io.LimitReader(r.Body, maxMCPBody+1))
		if err != nil || len(body) > maxMCPBody {
			http.Error(w, "the MCP message is too large", http.StatusRequestEntityTooLarge)
			return nil
		}
		in = parseBatch(body)
		if p.gate(r.Context(), w, t, &in) {
			p.record(t, in, "blocked", start)
			return nil
		}
		body = in.encode(body)
	}
	status := p.forward(w, r, t, body, in)
	p.record(t, in, status, start)
	return nil
}

func (p *MCPProxy) target(ctx context.Context, call MCPCall, endpoint string) (*mcpTarget, error) {
	svc := p.ops.svc
	r, err := svc.Resolve(call.Ref, call.Workspace)
	if err != nil {
		return nil, err
	}
	if call.RunID != "" && p.ops.kit != nil && !slices.ContainsFunc(p.ops.kit.RunConnectors(ctx, call.RunID), func(n string) bool {
		return n == r.ID || n == r.Slug
	}) {
		return nil, fmt.Errorf("%w: add %s to the connectors of the job or agent", ErrConnectorNotKit, r.Slug)
	}
	oc, _, err := svc.oauthConn(r)
	if err != nil {
		return nil, err
	}
	conn, err := svc.Lookup(r.ID)
	if err != nil {
		return nil, err
	}
	srv, _, err := r.MCPServer()
	if err != nil {
		return nil, err
	}
	t := &mcpTarget{call: call, r: r, conn: conn, upstream: oc.URL, headers: srv.Headers}
	if endpoint != "" {
		if t.upstream, err = sameOrigin(oc.URL, endpoint); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// gate applies the tool states to each tools/call. It answers the run and
// returns true when a call may not go out; then nothing goes out.
func (p *MCPProxy) gate(ctx context.Context, w http.ResponseWriter, t *mcpTarget, in *rpcBatch) bool {
	var blocked error
	for _, m := range in.msgs {
		if m.Method != "tools/call" || blocked != nil {
			continue
		}
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(m.Params, &params) // a bad call goes out; the server answers it
		c := CallOpts{Operation: params.Name, RunID: t.call.RunID, Agent: t.call.Agent}
		blocked = p.ops.gate(ctx, c, t.r, t.conn.toolState(params.Name), []string{params.Name, string(params.Arguments)})
	}
	if blocked == nil {
		return false
	}
	var out []rpcMessage
	for _, m := range in.msgs {
		if len(m.ID) == 0 || m.Method == "" {
			continue
		}
		text := "Trebi did not send this call, because another call in the batch is blocked."
		if m.Method == "tools/call" {
			text = blocked.Error()
		}
		out = append(out, rpcError(m.ID, rpcBlocked, text))
	}
	writeMessages(w, out, in.batch)
	return true
}

// forward sends the request with the token and streams the answer. It
// returns the status for the log.
func (p *MCPProxy) forward(w http.ResponseWriter, r *http.Request, t *mcpTarget, body []byte, in rpcBatch) string {
	resp, err := p.send(r.Context(), r.Method, t, r.Header, body)
	if err != nil {
		if r.Context().Err() != nil {
			return "cancelled"
		}
		var ue *upstreamError
		if !errors.As(err, &ue) {
			ue = &upstreamError{code: rpcLogin, text: fmt.Sprintf("Trebi could not reach %s: %v", t.r.Title, err)}
		}
		p.answerError(w, in, ue)
		return "error"
	}
	defer resp.Body.Close() //nolint:errcheck // read to the end or cancelled
	for _, k := range []string{"Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	ct, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	switch {
	case strings.TrimSpace(ct) == "text/event-stream":
		w.WriteHeader(resp.StatusCode)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		p.stream(r.Context(), w, resp.Body, t, r.Header, in)
	case len(in.lists) > 0 && resp.StatusCode == http.StatusOK:
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPBody))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return "error"
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(t.filterTools(b, in.lists)) //nolint:errcheck // the client went away
	default:
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body) //nolint:errcheck // the client went away
	}
	return strconv.Itoa(resp.StatusCode)
}

// send makes one upstream request with the token. It renews the token
// once on a 401, and starts a login with more access on a 403
// insufficient_scope.
func (p *MCPProxy) send(ctx context.Context, method string, t *mcpTarget, hdr http.Header, body []byte) (*http.Response, error) {
	svc := p.ops.svc
	tok, err := svc.oauthToken(ctx, t.r, false)
	for try := 0; ; try++ {
		if err != nil {
			return nil, t.loginError(err)
		}
		req, rerr := http.NewRequestWithContext(ctx, method, t.upstream, bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		for _, k := range mcpHeaders {
			if v := hdr.Get(k); v != "" {
				req.Header.Set(k, v)
			}
		}
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, rerr := p.hc.Do(req)
		if rerr != nil {
			return nil, rerr
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && try == 0:
			resp.Body.Close() //nolint:errcheck,gosec // retried
			tok, err = svc.oauthToken(ctx, t.r, true)
			continue
		case resp.StatusCode == http.StatusUnauthorized:
			resp.Body.Close() //nolint:errcheck,gosec // answered
			return nil, t.loginError(oauth.ErrLoginRequired)
		case resp.StatusCode == http.StatusForbidden:
			challenge := resp.Header.Values("WWW-Authenticate")
			if !strings.Contains(strings.Join(challenge, " "), "insufficient_scope") {
				return resp, nil
			}
			resp.Body.Close() //nolint:errcheck,gosec // answered
			if err := svc.stepUpLogin(ctx, t.r.ID, challenge); err != nil {
				p.log.Warn("connector.auth.required", "connector", t.r.ID, "err", err)
			}
			return nil, &upstreamError{code: rpcLogin, text: "Log in again with more access to " + t.r.Title + ". Ask the user to open the connection in Trebi."}
		}
		return resp, nil
	}
}

// stream copies a server-sent event stream one event at a time. It
// filters tools/list answers, rewrites the legacy endpoint event, and
// answers an elicitation without the run.
func (p *MCPProxy) stream(ctx context.Context, w http.ResponseWriter, body io.Reader, t *mcpTarget, hdr http.Header, in rpcBatch) {
	flusher, _ := w.(http.Flusher)
	post := t.upstream
	rd := bufio.NewReader(body)
	for {
		ev, err := readEvent(rd)
		if len(ev.raw) > 0 {
			out := ev.raw
			switch {
			case ev.name == "endpoint":
				if abs, perr := sameOrigin(t.upstream, strings.TrimSpace(ev.data)); perr == nil {
					post = abs
					out = []byte("event: endpoint\ndata: " + t.call.Self + "?" + endpointParam + "=" + url.QueryEscape(abs) + "\n\n")
				}
			case ev.data != "":
				out = p.event(ctx, t, hdr, post, ev, in.lists)
			}
			if out != nil {
				if _, werr := w.Write(out); werr != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// event handles one data event. It returns nil to drop the event.
func (p *MCPProxy) event(ctx context.Context, t *mcpTarget, hdr http.Header, post string, ev sseEvent, lists map[string]bool) []byte {
	var m rpcMessage
	if json.Unmarshal([]byte(ev.data), &m) != nil {
		return ev.raw
	}
	if m.Method == "elicitation/create" && len(m.ID) > 0 {
		go p.answerElicit(context.WithoutCancel(ctx), t, hdr, post, m) //nolint:gosec // ends after wait; see answerElicit
		return nil
	}
	if len(m.ID) > 0 && lists[string(m.ID)] {
		return ev.with(t.filterTools([]byte(ev.data), lists))
	}
	if len(m.ID) == 0 && m.Method == "notifications/elicitation/complete" {
		return nil
	}
	return ev.raw
}

// answerElicit turns an elicitation into a Trebi ask and posts the answer
// to the server. It stops after the ask wait.
func (p *MCPProxy) answerElicit(ctx context.Context, t *mcpTarget, hdr http.Header, post string, m rpcMessage) {
	ctx, cancel := context.WithTimeout(ctx, p.wait)
	defer cancel()
	var params struct {
		Mode    string          `json:"mode"`
		Message string          `json:"message"`
		Schema  json.RawMessage `json:"requestedSchema"`
	}
	_ = json.Unmarshal(m.Params, &params) // a bad request gets decline
	result := map[string]any{"action": "decline"}
	if t.call.RunID != "" && p.elicit != nil && params.Mode != "url" {
		if schema, ok := flatSchema(params.Schema); ok {
			title := clip(t.r.Title+" asks: "+params.Message, 120)
			values, ok, err := p.elicit.Elicit(ctx, t.call.RunID, title, params.Message, schema)
			switch {
			case err != nil:
				p.log.Info("connector.mcp", "connector", t.r.ID, "method", m.Method, "status", "ask_failed", "err", err)
			case ok:
				result = map[string]any{"action": "accept", "content": cmpRaw(values, "{}")}
			}
		}
	}
	res, _ := json.Marshal(result) //nolint:errchkjson // a fixed shape
	body, _ := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: m.ID, Result: res})
	at := *t
	at.upstream = post
	h := hdr.Clone()
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/event-stream")
	resp, err := p.send(ctx, http.MethodPost, &at, h, body)
	if err != nil {
		p.log.Info("connector.mcp", "connector", t.r.ID, "method", m.Method, "status", "answer_failed", "err", err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // the answer has no use
	resp.Body.Close()                     //nolint:errcheck,gosec // read
	p.log.Info("connector.mcp", "connector", t.r.ID, "method", m.Method, "status", result["action"], "run_id", t.call.RunID)
}

// answerError answers each request of the post with the error, as JSON-RPC
// errors, so the run can tell the user. A request with no id gets 502.
func (p *MCPProxy) answerError(w http.ResponseWriter, in rpcBatch, e *upstreamError) {
	var out []rpcMessage
	for _, m := range in.msgs {
		if len(m.ID) > 0 && m.Method != "" {
			out = append(out, rpcError(m.ID, e.code, e.text))
		}
	}
	if len(out) == 0 {
		http.Error(w, e.text, http.StatusBadGateway)
		return
	}
	writeMessages(w, out, in.batch)
}

func (p *MCPProxy) record(t *mcpTarget, in rpcBatch, status string, start time.Time) {
	method, tool := "", ""
	for _, m := range in.msgs {
		if m.Method == "" {
			continue
		}
		if method == "" {
			method = m.Method
		}
		if m.Method == "tools/call" && tool == "" {
			var params struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(m.Params, &params) // the log shows no tool
			tool = params.Name
		}
	}
	p.log.Info("connector.mcp", "connector", t.r.ID, "method", method, "tool", tool, "status", status,
		"duration_ms", time.Since(start).Milliseconds(), "run_id", t.call.RunID)
}

// toolState is the state of an MCP tool: the override, else on.
func (c Connection) toolState(name string) manifest.OpState {
	if st, ok := c.Operations[name]; ok && st.Valid() {
		return st
	}
	return manifest.OpOn
}

// loginError is the answer to a run when the connection has no token.
func (t *mcpTarget) loginError(err error) error {
	if errors.Is(err, oauth.ErrLoginRequired) {
		return &upstreamError{code: rpcLogin, text: "Log in again to " + t.r.Title + ". Ask the user to open the connection in Trebi."}
	}
	return &upstreamError{code: rpcLogin, text: fmt.Sprintf("Trebi could not get a token for %s: %v", t.r.Title, err)}
}

// filterTools drops the tools in state off from a tools/list answer. A
// body that it cannot read goes through as is.
func (t *mcpTarget) filterTools(b []byte, lists map[string]bool) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil || !lists[string(m["id"])] {
		return b
	}
	var res map[string]json.RawMessage
	if json.Unmarshal(m["result"], &res) != nil {
		return b
	}
	var tools []json.RawMessage
	if json.Unmarshal(res["tools"], &tools) != nil {
		return b
	}
	kept := make([]json.RawMessage, 0, len(tools))
	for _, raw := range tools {
		var tool struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &tool) == nil && t.conn.toolState(tool.Name) == manifest.OpOff {
			continue
		}
		kept = append(kept, raw)
	}
	var err error
	if res["tools"], err = json.Marshal(kept); err != nil {
		return b
	}
	if m["result"], err = json.Marshal(res); err != nil {
		return b
	}
	out, err := json.Marshal(m)
	if err != nil {
		return b
	}
	return out
}

// encode writes the batch back after the proxy changed initialize. It
// returns the original body when nothing changed.
func (in *rpcBatch) encode(orig []byte) []byte {
	changed := false
	for i, m := range in.msgs {
		if m.Method != "initialize" {
			continue
		}
		var params map[string]json.RawMessage
		if json.Unmarshal(m.Params, &params) != nil {
			continue
		}
		var caps map[string]json.RawMessage
		_ = json.Unmarshal(params["capabilities"], &caps) // absent capabilities start empty
		if caps == nil {
			caps = map[string]json.RawMessage{}
		}
		if _, ok := caps["elicitation"]; ok {
			continue
		}
		caps["elicitation"] = json.RawMessage(`{}`)
		params["capabilities"], _ = json.Marshal(caps) //nolint:errchkjson // raw values
		in.msgs[i].Params, _ = json.Marshal(params)    //nolint:errchkjson // raw values
		changed = true
	}
	if !changed {
		return orig
	}
	if !in.batch {
		out, _ := json.Marshal(in.msgs[0]) //nolint:errchkjson // raw values
		return out
	}
	out, _ := json.Marshal(in.msgs) //nolint:errchkjson // raw values
	return out
}

// sseEvent is one server-sent event.
type sseEvent struct {
	name string
	data string
	raw  []byte
}

// with returns the event with new data and the other fields kept.
func (e sseEvent) with(data []byte) []byte {
	var b bytes.Buffer
	for line := range strings.SplitSeq(strings.TrimRight(string(e.raw), "\n"), "\n") {
		if !strings.HasPrefix(line, "data:") {
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("data: ")
	b.Write(data)
	b.WriteString("\n\n")
	return b.Bytes()
}

// readEvent reads one event up to its blank line.
func readEvent(rd *bufio.Reader) (sseEvent, error) {
	var ev sseEvent
	var data []string
	for {
		line, err := rd.ReadString('\n')
		ev.raw = append(ev.raw, line...)
		trim := strings.TrimRight(line, "\r\n")
		switch {
		case trim == "" && line != "":
			ev.data = strings.Join(data, "\n")
			return ev, nil
		case strings.HasPrefix(trim, "event:"):
			ev.name = strings.TrimSpace(strings.TrimPrefix(trim, "event:"))
		case strings.HasPrefix(trim, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(trim, "data:"), " "))
		}
		if err != nil {
			ev.data = strings.Join(data, "\n")
			return ev, err
		}
	}
}

// parseBatch reads a post body. A body that is not JSON-RPC goes out as is,
// so the server answers it.
func parseBatch(body []byte) rpcBatch {
	in := rpcBatch{lists: map[string]bool{}}
	trim := bytes.TrimSpace(body)
	if len(trim) > 0 && trim[0] == '[' {
		in.batch = true
		_ = json.Unmarshal(trim, &in.msgs) // see above
	} else {
		var m rpcMessage
		if json.Unmarshal(trim, &m) == nil {
			in.msgs = []rpcMessage{m}
		}
	}
	for _, m := range in.msgs {
		if m.Method == "tools/list" && len(m.ID) > 0 {
			in.lists[string(m.ID)] = true
		}
	}
	return in
}

func rpcError(id json.RawMessage, code int, text string) rpcMessage {
	e, _ := json.Marshal(map[string]any{"code": code, "message": text}) //nolint:errchkjson // a fixed shape
	return rpcMessage{JSONRPC: "2.0", ID: id, Error: e}
}

func writeMessages(w http.ResponseWriter, out []rpcMessage, batch bool) {
	w.Header().Set("Content-Type", "application/json")
	var v any = out
	if !batch && len(out) == 1 {
		v = out[0]
	}
	_ = json.NewEncoder(w).Encode(v) //nolint:errcheck // the client went away
}

// sameOrigin resolves a post address against the server URL and refuses
// another origin, so the token never goes to another host.
func sameOrigin(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u, err := b.Parse(ref)
	if err != nil {
		return "", err
	}
	if u.Scheme != b.Scheme || u.Host != b.Host {
		return "", fmt.Errorf("%w: %s", ErrMCPEndpoint, u.Host)
	}
	return u.String(), nil
}

// flatSchema makes the requested schema of an elicitation fit a Trebi form:
// one level of string, number, integer, boolean, and enum fields. A schema
// with no field asks for a yes or no (nil). ok is false for a field that
// a form cannot show.
func flatSchema(raw json.RawMessage) (json.RawMessage, bool) {
	var in struct {
		Properties map[string]struct {
			Type        string   `json:"type"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &in) != nil {
		return nil, false
	}
	if len(in.Properties) == 0 {
		return nil, true
	}
	props := map[string]any{}
	for name, f := range in.Properties {
		switch f.Type {
		case "string", "number", "integer", "boolean":
		default:
			return nil, false
		}
		field := map[string]any{"type": f.Type}
		for k, v := range map[string]string{"title": f.Title, "description": f.Description} {
			if v != "" {
				field[k] = v
			}
		}
		if len(f.Enum) > 0 {
			field["enum"] = f.Enum
		}
		props[name] = field
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(in.Required) > 0 {
		schema["required"] = in.Required
	}
	out, err := json.Marshal(schema)
	return out, err == nil
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func cmpRaw(v json.RawMessage, def string) json.RawMessage {
	if len(bytes.TrimSpace(v)) == 0 {
		return json.RawMessage(def)
	}
	return v
}
