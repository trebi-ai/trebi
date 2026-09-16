package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// acpKind distinguishes the ACP agents that share this driver.
type acpKind string

const (
	acpCopilot acpKind = "copilot"
	acpCursor  acpKind = "cursor"
)

const acpHandshakeTimeout = 45 * time.Second

// acpStart spawns an ACP agent (Copilot --acp / cursor-agent acp) and runs
// initialize → (authenticate) → session/new|load (E.5/E.6).
func acpStart(ctx context.Context, req StartRequest, kind acpKind) (Session, error) {
	argv := SplitArgs(req.BaseCommand)
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s: empty command", kind)
	}
	bin := argv[0]
	var args []string
	switch kind {
	case acpCopilot:
		args = append([]string{}, argv[1:]...)
		if !hasFlag(args, "--acp") {
			args = append(args, "--acp")
		}
	case acpCursor:
		args = append([]string{argv[0]}, stripCursorArgs(argv[1:])...)
		args = append(args, "acp")
		bin = args[0]
		args = args[1:]
	}
	proto := &acpProtocol{
		kind:     kind,
		cwd:      req.WorkingDir,
		model:    req.Model,
		resumeID: req.SessionID,
		waiters:  map[int]chan map[string]any{},
	}
	proc, err := StartProc(ctx, ProcOpts{Path: bin, Args: args, Dir: req.WorkingDir, Env: childEnv(req.Env)})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", kind, err)
	}
	proto.timeout = firstNonZero(req.HandshakeTimeout, acpHandshakeTimeout)
	s := startWire(ctx, string(kind), proc, proto)
	proto.setWriter(&wireWriter{s: s})
	if proto.sessionID != "" {
		s.setID(proto.sessionID)
	}
	if req.OneShot && req.OneShotPrompt != "" {
		if err := s.Prompt(ctx, Prompt{Text: req.OneShotPrompt, Kind: "job"}); err != nil {
			_ = proc.gracefulClose(gracefulCloseTimeout)
			return nil, err
		}
		_ = proc.CloseStdin()
	}
	return s, nil
}

// stripCursorArgs removes run-mode flags that conflict with the acp subcommand.
func stripCursorArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--resume" || a == "--continue" || a == "-c":
			i++
		case strings.HasPrefix(a, "--resume=") || strings.HasPrefix(a, "--continue="):
		default:
			out = append(out, a)
		}
	}
	return out
}

// acpProtocol maps Agent Client Protocol JSON-RPC to events.
type acpProtocol struct {
	mu      sync.Mutex
	w       *wireWriter
	nextID  int
	waiters map[int]chan map[string]any
	timeout time.Duration

	kind      acpKind
	cwd       string
	model     string
	resumeID  string
	sessionID string

	authMethods []string
	promptID    int
	sawResult   bool
	approvals   map[string][]acpOption // request id → options
}

type acpOption struct {
	ID   string
	Kind string
}

func (p *acpProtocol) setWriter(w *wireWriter) {
	p.mu.Lock()
	p.w = w
	p.mu.Unlock()
}

func (p *acpProtocol) send(v any) error {
	p.mu.Lock()
	w := p.w
	p.mu.Unlock()
	if w == nil {
		return fmt.Errorf("%s: not connected", p.kind)
	}
	b, err := jsonLine(v)
	if err != nil {
		return err
	}
	return w.Write(b)
}

func (p *acpProtocol) request(ctx context.Context, method string, params any, timeout time.Duration) (map[string]any, error) {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	ch := make(chan map[string]any, 1)
	p.waiters[id] = ch
	p.mu.Unlock()
	if err := p.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = firstNonZero(p.timeout, acpHandshakeTimeout)
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case res := <-ch:
		if e, ok := res["error"]; ok {
			return nil, fmt.Errorf("%s %s: %v", p.kind, method, e)
		}
		out, _ := res["result"].(map[string]any)
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, fmt.Errorf("%s %s: timeout", p.kind, method)
	}
}

func (p *acpProtocol) notify(method string, params any) error {
	return p.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// Handshake negotiates capabilities and opens (or loads) a session.
func (p *acpProtocol) Handshake(ctx context.Context, _ *wireWriter) []Event {
	init, err := p.request(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
			"auth":     map[string]any{"terminal": false},
		},
		"clientInfo": map[string]any{"name": "trebi", "version": "1.0.0"},
	}, 0)
	if err != nil {
		return []Event{{Type: EventError, Error: err.Error(), EndReason: string(FailProtocol)}}
	}
	p.mu.Lock()
	if methods, ok := init["authMethods"].([]any); ok {
		for _, m := range methods {
			if mm, ok := m.(map[string]any); ok {
				if id := str(mm["id"]); id != "" {
					p.authMethods = append(p.authMethods, id)
				}
			}
		}
	}
	p.mu.Unlock()

	if p.resumeID != "" {
		p.sessionID = p.resumeID
		if evs, err := p.loadSession(ctx); err == nil {
			return evs
		} else if !errors.Is(err, ErrResumeUnsupported) {
			return []Event{{Type: EventError, Error: err.Error(), EndReason: string(FailProtocol)}}
		}
	}
	evs, err := p.newSession(ctx)
	if err != nil {
		if authErr := p.maybeAuthenticate(ctx); authErr == nil {
			evs, err = p.newSession(ctx)
		}
	}
	if err != nil {
		return []Event{{Type: EventError, Error: err.Error(), EndReason: string(FailProtocol)}}
	}
	return evs
}

func (p *acpProtocol) newSession(ctx context.Context) ([]Event, error) {
	res, err := p.request(ctx, "session/new", map[string]any{
		"cwd": p.cwd, "mcpServers": []any{},
	}, 0)
	if err != nil {
		return nil, err
	}
	id := str(res["sessionId"])
	if id == "" {
		return nil, fmt.Errorf("%s: session/new returned no sessionId", p.kind)
	}
	p.mu.Lock()
	p.sessionID = id
	p.mu.Unlock()
	return []Event{{Type: EventInit, SessionID: id}}, nil
}

func (p *acpProtocol) loadSession(ctx context.Context) ([]Event, error) {
	res, err := p.request(ctx, "session/resume", map[string]any{
		"sessionId": p.sessionID, "cwd": p.cwd, "mcpServers": []any{},
	}, 0)
	if err != nil {
		res, err = p.request(ctx, "session/load", map[string]any{
			"sessionId": p.sessionID, "cwd": p.cwd, "mcpServers": []any{},
		}, 0)
		if err != nil {
			return nil, fmt.Errorf("%w: %s session %s: %v", ErrResumeUnsupported, p.kind, p.sessionID, err)
		}
	}
	id := str(res["sessionId"])
	if id == "" {
		id = p.sessionID
	}
	p.mu.Lock()
	p.sessionID = id
	p.mu.Unlock()
	return []Event{{Type: EventInit, SessionID: id}}, nil
}

// maybeAuthenticate answers auth_required with the agent's own method.
func (p *acpProtocol) maybeAuthenticate(ctx context.Context) error {
	p.mu.Lock()
	methods := append([]string{}, p.authMethods...)
	p.mu.Unlock()
	if len(methods) == 0 {
		return errors.New("no auth methods")
	}
	pick := methods[0]
	for _, m := range methods {
		if p.kind == acpCursor && strings.Contains(m, "cursor_login") {
			pick = m
		}
		if p.kind == acpCopilot && (strings.Contains(m, "github") || strings.Contains(m, "device")) {
			pick = m
		}
	}
	_, err := p.request(ctx, "authenticate", map[string]any{"methodId": pick}, 0)
	return err
}

// EncodePrompt sends session/prompt; its response is the turn completion.
func (p *acpProtocol) EncodePrompt(pr Prompt) ([]byte, error) {
	p.mu.Lock()
	id := p.nextID
	p.nextID++
	p.promptID = id
	sessionID := p.sessionID
	p.mu.Unlock()
	return jsonLine(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]any{{"type": "text", "text": pr.Text}},
		},
	})
}

// EncodeInterrupt sends the session/cancel notification.
func (p *acpProtocol) EncodeInterrupt() ([]byte, error) {
	p.mu.Lock()
	sessionID := p.sessionID
	p.mu.Unlock()
	return jsonLine(map[string]any{
		"jsonrpc": "2.0", "method": "session/cancel",
		"params": map[string]any{"sessionId": sessionID},
	})
}

// EncodeDecision answers session/request_permission.
func (p *acpProtocol) EncodeDecision(id string, d Decision) ([]byte, error) {
	p.mu.Lock()
	options := p.approvals[id]
	delete(p.approvals, id)
	p.mu.Unlock()
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(id), &raw); err != nil {
		raw = json.RawMessage(fmt.Sprintf("%q", id))
	}
	outcome := map[string]any{"outcome": "cancelled"}
	if d.Allow {
		if option := pickACPOption(options, true); option != "" {
			outcome = map[string]any{"outcome": "selected", "optionId": option}
		}
	} else if option := pickACPOption(options, false); option != "" {
		outcome = map[string]any{"outcome": "selected", "optionId": option}
	}
	return jsonLine(map[string]any{"jsonrpc": "2.0", "id": raw, "result": map[string]any{"outcome": outcome}})
}

func pickACPOption(options []acpOption, allow bool) string {
	for _, o := range options {
		if allow && (o.Kind == "allow_once" || o.Kind == "allow_always") {
			return o.ID
		}
		if !allow && (o.Kind == "reject_once" || o.Kind == "reject_always") {
			return o.ID
		}
	}
	if len(options) > 0 {
		if allow {
			return options[0].ID
		}
		return options[len(options)-1].ID
	}
	return ""
}

// Parse routes responses, maps session/update notifications, and answers
// unhandled client-method requests so the agent never hangs.
func (p *acpProtocol) Parse(line []byte) []Event {
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		return nil
	}
	method := str(m["method"])
	if hasKey(m, "id") && method != "" {
		// Agent → client request.
		return p.parseAgentRequest(m, method)
	}
	if hasKey(m, "result") || hasKey(m, "error") {
		return p.parseResponse(m)
	}
	if method != "session/update" {
		return nil
	}
	params, _ := m["params"].(map[string]any)
	if params == nil {
		return nil
	}
	if sid := str(params["sessionId"]); sid != "" && sid != p.currentSession() {
		return nil
	}
	update, _ := params["update"].(map[string]any)
	if update == nil {
		return nil
	}
	switch str(update["sessionUpdate"]) {
	case "agent_message_chunk":
		content, _ := update["content"].(map[string]any)
		if content != nil && str(content["type"]) == "text" {
			return []Event{{Type: EventAssistant, Text: str(content["text"])}}
		}
	case "agent_thought_chunk", "plan", "plan_update", "available_commands_update", "current_mode_update":
		return []Event{{Type: EventStatus, Status: StatusRunning}}
	case "tool_call":
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: str(update["toolCallId"]), Name: orDefault(str(update["title"]), str(update["kind"])),
			Status: "started",
		}}}
	case "tool_call_update":
		status := str(update["status"])
		switch status {
		case "completed", "failed":
		default:
			status = "started"
		}
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: str(update["toolCallId"]), Name: str(update["title"]), Status: status,
		}}}
	case "user_message_chunk":
		return nil
	}
	return nil
}

func (p *acpProtocol) currentSession() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionID
}

func (p *acpProtocol) parseResponse(m map[string]any) []Event {
	id, ok := codexIntID(m["id"])
	if !ok {
		return nil
	}
	p.mu.Lock()
	ch := p.waiters[id]
	delete(p.waiters, id)
	promptID := p.promptID
	p.mu.Unlock()
	if ch != nil {
		select {
		case ch <- m:
		default:
		}
		return nil
	}
	if id != promptID {
		return nil
	}
	if e, ok := m["error"]; ok {
		p.mu.Lock()
		p.sawResult = true
		p.mu.Unlock()
		text := fmt.Sprint(e)
		c := ClassifyError(text, 0)
		return []Event{{Type: EventError, Error: text, EndReason: string(c.Class), Hint: c.Hint}}
	}
	res, _ := m["result"].(map[string]any)
	stop := str(res["stopReason"])
	p.mu.Lock()
	p.sawResult = true
	p.mu.Unlock()
	switch stop {
	case "", "end_turn":
		return []Event{{Type: EventResult, Result: &Result{Subtype: "success"}}}
	case "cancelled":
		return []Event{{Type: EventResult, Result: &Result{Subtype: "cancelled", IsError: true, Text: "turn cancelled"}}}
	default:
		return []Event{{Type: EventResult, Result: &Result{Subtype: stop, IsError: true, Text: "stop: " + stop}}}
	}
}

func (p *acpProtocol) parseAgentRequest(m map[string]any, method string) []Event {
	id := codexIDString(m["id"])
	params, _ := m["params"].(map[string]any)
	switch method {
	case "session/request_permission":
		var options []acpOption
		if raw, ok := params["options"].([]any); ok {
			for _, o := range raw {
				if om, ok := o.(map[string]any); ok {
					options = append(options, acpOption{ID: str(om["optionId"]), Kind: str(om["kind"])})
				}
			}
		}
		question := "Agent requests permission"
		if tc, _ := params["toolCall"].(map[string]any); tc != nil {
			if title := str(tc["title"]); title != "" {
				question = "Allow " + title + "?"
			}
		}
		p.mu.Lock()
		if p.approvals == nil {
			p.approvals = map[string][]acpOption{}
		}
		p.approvals[id] = options
		p.mu.Unlock()
		return []Event{{Type: EventPermission, Permission: &Permission{ID: id, Question: question}}}
	default:
		// fs/* and terminal/* are advertised unsupported; answer with an
		// explicit error rather than letting the agent hang.
		out, _ := jsonLine(map[string]any{
			"jsonrpc": "2.0", "id": json.RawMessage(id),
			"error": map[string]any{"code": -32601, "message": "trebi: client method unsupported: " + method},
		})
		p.mu.Lock()
		w := p.w
		p.mu.Unlock()
		if w != nil {
			_ = w.Write(out)
		}
		return nil
	}
}

// Exit classifies a process exit without a prompt response.
func (p *acpProtocol) Exit(code int, err error, stderr string) []Event {
	p.mu.Lock()
	saw := p.sawResult
	p.mu.Unlock()
	if saw {
		return nil
	}
	text := strings.TrimSpace(stderr)
	if text == "" {
		if err != nil {
			text = err.Error()
		} else {
			text = fmt.Sprintf("%s exited with code %d before the turn ended", p.kind, code)
		}
	}
	c := ClassifyError(text, 0)
	return []Event{{Type: EventError, Error: text, EndReason: string(c.Class), Hint: c.Hint}}
}
