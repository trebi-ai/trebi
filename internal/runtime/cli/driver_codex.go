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

// Codex app-server defaults: unattended jobs must not sit on a prompt. The
// live approval test overrides both through StartRequest.
const (
	codexDefaultApprovalPolicy = "never"
	codexDefaultSandbox        = "workspace-write"
	codexHandshakeTimeout      = 30 * time.Second
	// codexInterruptWait bounds the interrupt→new-turn fallback. A steer is
	// preferred; older pins answer method-not-found and land here.
	codexInterruptWait = 5 * time.Second
	// codexPromptAckWait bounds the turn/start ack wait: the turn is live even
	// if the pin streams notifications without an RPC response.
	codexPromptAckWait = 2 * time.Second
	// codexSteerAckWait bounds the steer ack wait before falling back.
	codexSteerAckWait = 5 * time.Second
)

// codexStart spawns `codex app-server` and performs the initialize handshake.
func codexStart(ctx context.Context, req StartRequest) (Session, error) {
	argv := SplitArgs(req.BaseCommand)
	if len(argv) == 0 {
		return nil, errors.New("codex: empty command")
	}
	bin := argv[0]
	cfgOverrides, model := codexConfig(argv[1:])
	args := append(append([]string{}, cfgOverrides...), "app-server")
	if model == "" {
		model = req.Model
	}
	proto := &codexProtocol{
		oneShot:        req.OneShot,
		model:          model,
		cwd:            req.WorkingDir,
		approvalPolicy: orDefault(req.ApprovalPolicy, codexDefaultApprovalPolicy),
		sandbox:        orDefault(req.Sandbox, codexDefaultSandbox),
		resumeID:       req.SessionID,
		waiters:        map[int]chan map[string]any{},
	}
	proc, err := StartProc(ctx, ProcOpts{Path: bin, Args: args, Dir: req.WorkingDir, Env: childEnv(req.Env)})
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	proto.timeout = firstNonZero(req.HandshakeTimeout, codexHandshakeTimeout)
	s := startWire(ctx, "codex", proc, proto)
	proto.setWriter(&wireWriter{s: s})
	if proto.threadID != "" {
		s.setID(proto.threadID)
	}
	if req.OneShot && req.OneShotPrompt != "" {
		if err := s.Prompt(ctx, Prompt{Text: req.OneShotPrompt, Kind: "job"}); err != nil {
			_ = proc.gracefulClose(gracefulCloseTimeout)
			return nil, fmt.Errorf("codex: write prompt: %w", err)
		}
		_ = proc.CloseStdin()
	}
	return s, nil
}

// codexConfig splits the prepared codex argv into `-c key=value` overrides
// and the model value. The notify helper is dropped on the CLI path (A.14).
func codexConfig(args []string) (overrides []string, model string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" && i+1 < len(args):
			i++
			if !strings.Contains(args[i], "notify=") {
				overrides = append(overrides, "-c", args[i])
			}
		case strings.HasPrefix(a, "-c="):
			if !strings.Contains(a, "notify=") {
				overrides = append(overrides, a)
			}
		case (a == "--model" || a == "-m") && i+1 < len(args):
			i++
			model = args[i]
		case strings.HasPrefix(a, "--model="):
			model = strings.TrimPrefix(a, "--model=")
		}
	}
	return overrides, model
}

func firstNonZero(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// codexProtocol ports the app-server JSON-RPC shapes. Requests it sends get
// waiters; approval requests from the server become permission events.
type codexProtocol struct {
	mu      sync.Mutex
	w       *wireWriter
	nextID  int
	waiters map[int]chan map[string]any

	timeout        time.Duration
	model          string
	cwd            string
	approvalPolicy string
	sandbox        string
	resumeID       string
	oneShot        bool

	threadID   string
	turnID     string
	turnActive bool
	sawResult  bool
	ready      bool
	approvals  map[string]string // request id → method
}

func (p *codexProtocol) setWriter(w *wireWriter) {
	p.mu.Lock()
	p.w = w
	p.mu.Unlock()
}

func (p *codexProtocol) send(frame map[string]any) error {
	p.mu.Lock()
	w := p.w
	p.mu.Unlock()
	if w == nil {
		return errors.New("codex: not connected")
	}
	b, err := jsonLine(frame)
	if err != nil {
		return err
	}
	return w.Write(b)
}

func (p *codexProtocol) request(ctx context.Context, method string, params any) (map[string]any, error) {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	ch := make(chan map[string]any, 1)
	p.waiters[id] = ch
	p.mu.Unlock()
	err := p.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		p.mu.Lock()
		delete(p.waiters, id)
		p.mu.Unlock()
		return nil, err
	}
	timeout := p.timeout
	if timeout <= 0 {
		timeout = codexHandshakeTimeout
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case res := <-ch:
		if e, ok := res["error"]; ok {
			return nil, fmt.Errorf("codex %s: %v", method, e)
		}
		out, _ := res["result"].(map[string]any)
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, fmt.Errorf("codex %s: timeout", method)
	}
}

func (p *codexProtocol) notify(method string, params any) error {
	return p.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// requestSoft sends a JSON-RPC request and waits up to wait for a response.
// A timeout is not an error: some pins stream turn/start notifications without
// an RPC ack. A server error is returned so the caller can fall back (steer).
func (p *codexProtocol) requestSoft(ctx context.Context, method string, params any, wait time.Duration) error {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	ch := make(chan map[string]any, 1)
	p.waiters[id] = ch
	p.mu.Unlock()
	if err := p.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		p.mu.Lock()
		delete(p.waiters, id)
		p.mu.Unlock()
		return err
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case res := <-ch:
		if e, ok := res["error"]; ok {
			return fmt.Errorf("codex %s: %v", method, e)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		p.mu.Lock()
		delete(p.waiters, id)
		p.mu.Unlock()
		return nil
	}
}

// Handshake: initialize → initialized → thread/start|resume.
func (p *codexProtocol) Handshake(ctx context.Context, _ *wireWriter) []Event {
	if _, err := p.request(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "trebi", "title": "Trebi", "version": "1.0.0"},
	}); err != nil {
		return []Event{{Type: EventError, Error: err.Error(), EndReason: string(FailProtocol)}}
	}
	_ = p.notify("initialized", map[string]any{})
	params := map[string]any{
		"cwd":            p.cwd,
		"approvalPolicy": p.approvalPolicy, "sandbox": p.sandbox,
	}
	if p.model != "" {
		params["model"] = p.model
	}
	method := "thread/start"
	if p.resumeID != "" {
		method = "thread/resume"
		params["threadId"] = p.resumeID
	}
	res, err := p.request(ctx, method, params)
	if err != nil {
		return []Event{{Type: EventError, Error: err.Error(), EndReason: string(FailProtocol)}}
	}
	p.mu.Lock()
	p.ready = true
	p.mu.Unlock()
	if id := codexThreadID(res); id != "" {
		p.mu.Lock()
		p.threadID = id
		p.mu.Unlock()
		return []Event{{Type: EventInit, SessionID: id}}
	}
	return nil
}

// EncodePrompt starts a turn on the established thread. It is the
// fire-and-forget shape; the live path goes through promptSession so the ack
// is awaited and a busy turn steers instead of erroring.
func (p *codexProtocol) EncodePrompt(pr Prompt) ([]byte, error) {
	p.mu.Lock()
	threadID := p.threadID
	p.nextID++
	id := p.nextID
	p.mu.Unlock()
	if threadID == "" {
		return nil, errors.New("codex: thread not started")
	}
	return jsonLine(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "turn/start",
		"params": map[string]any{
			"threadId": threadID,
			"input":    []map[string]any{{"type": "text", "text": pr.Text}},
		},
	})
}

// promptSession is the live prompt path: turn/steer while a turn is active
// (E.2), falling back to interrupt + new turn when the pin rejects steer.
func (p *codexProtocol) promptSession(ctx context.Context, _ *wireSession, pr Prompt) error {
	p.mu.Lock()
	threadID, turnID, active := p.threadID, p.turnID, p.turnActive
	p.mu.Unlock()
	if threadID == "" {
		return errors.New("codex: thread not started")
	}
	input := []map[string]any{{"type": "text", "text": pr.Text}}
	if active {
		params := map[string]any{"threadId": threadID, "input": input}
		if turnID != "" {
			params["expectedTurnId"] = turnID
		}
		if err := p.requestSoft(ctx, "turn/steer", params, codexSteerAckWait); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return err
		}
		interrupt := map[string]any{"threadId": threadID}
		if turnID != "" {
			interrupt["turnId"] = turnID
		}
		_, _ = p.request(ctx, "turn/interrupt", interrupt)
		p.awaitTurnEnd(ctx, turnID)
	}
	if err := p.requestSoft(ctx, "turn/start", map[string]any{
		"threadId": threadID, "input": input,
	}, codexPromptAckWait); err != nil {
		return err
	}
	p.mu.Lock()
	p.turnActive = true
	p.mu.Unlock()
	return nil
}

// awaitTurnEnd waits out an interrupted turn so the next turn/start is
// accepted. The interrupt ack is not a turn-end signal, so poll the turn
// state rather than trusting the response.
func (p *codexProtocol) awaitTurnEnd(ctx context.Context, turnID string) {
	deadline := time.Now().Add(codexInterruptWait)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		done := !p.turnActive || (turnID != "" && p.turnID != turnID)
		p.mu.Unlock()
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (p *codexProtocol) EncodeInterrupt() ([]byte, error) {
	p.mu.Lock()
	threadID := p.threadID
	p.mu.Unlock()
	if threadID == "" {
		return nil, errors.New("codex: thread not started")
	}
	return jsonLine(map[string]any{
		"jsonrpc": "2.0", "id": 0, "method": "turn/interrupt",
		"params": map[string]any{"threadId": threadID},
	})
}

// EncodeDecision answers an approval request from the server.
func (p *codexProtocol) EncodeDecision(id string, d Decision) ([]byte, error) {
	p.mu.Lock()
	method := p.approvals[id]
	delete(p.approvals, id)
	p.mu.Unlock()
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(id), &raw); err != nil {
		raw = json.RawMessage(fmt.Sprintf("%q", id))
	}
	var result map[string]any
	switch {
	case strings.Contains(method, "requestUserInput"):
		result = map[string]any{"answers": map[string]any{}}
	case strings.Contains(method, "elicitation"):
		if d.Allow {
			result = map[string]any{"action": "accept"}
		} else {
			result = map[string]any{"action": "decline"}
		}
	default:
		if d.Allow {
			result = map[string]any{"decision": "accept"}
		} else {
			result = map[string]any{"decision": "decline"}
		}
	}
	return jsonLine(map[string]any{"jsonrpc": "2.0", "id": raw, "result": result})
}

// Parse routes responses to waiters and maps notifications to events.
func (p *codexProtocol) Parse(line []byte) []Event {
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		return nil
	}
	// Response to one of our requests.
	if hasKey(m, "result") || hasKey(m, "error") {
		if id, ok := codexIntID(m["id"]); ok {
			p.mu.Lock()
			ch := p.waiters[id]
			delete(p.waiters, id)
			p.mu.Unlock()
			if ch != nil {
				select {
				case ch <- m:
				default:
				}
			}
		}
		if hasKey(m, "error") && !hasKey(m, "method") {
			e := m["error"]
			return []Event{{Type: EventError, Error: fmt.Sprint(e), EndReason: string(FailProtocol)}}
		}
		return nil
	}
	method := str(m["method"])
	params, _ := m["params"].(map[string]any)
	// Server → client request (has id + method): an approval.
	if hasKey(m, "id") && method != "" {
		return p.parseServerRequest(m, method, params)
	}
	if method == "" {
		return nil
	}
	switch method {
	case "thread/started":
		if thread, _ := params["thread"].(map[string]any); thread != nil {
			id := str(thread["id"])
			p.mu.Lock()
			p.threadID = id
			p.mu.Unlock()
			return []Event{{Type: EventInit, SessionID: id}}
		}
	case "turn/started":
		turn, _ := params["turn"].(map[string]any)
		if turn != nil {
			p.mu.Lock()
			p.turnID = str(turn["id"])
			p.turnActive = true
			p.mu.Unlock()
		}
		return []Event{{Type: EventStatus, Status: StatusRunning}}
	case "turn/completed":
		return p.parseTurnCompleted(params)
	case "item/started", "item/completed", "item/updated":
		return p.parseItem(method, params)
	case "item/agentMessage/delta", "item/agent_message/delta":
		if text := str(params["delta"]); text != "" {
			return []Event{{Type: EventAssistant, Text: text, Delta: true}}
		}
	case "thread/tokenUsage/updated", "thread/token_usage/updated":
		if u := codexUsage(params); u != nil {
			return []Event{{Type: EventUsage, Usage: u}}
		}
	case "error":
		msg := str(params["message"])
		if msg == "" {
			msg = fmt.Sprint(m["error"])
		}
		c := ClassifyError(msg, 0)
		return []Event{{Type: EventError, Error: msg, EndReason: string(c.Class), Hint: c.Hint}}
	case "warning", "configWarning", "config/warning":
		return nil
	}
	return nil
}

func (p *codexProtocol) parseServerRequest(m map[string]any, method string, params map[string]any) []Event {
	id := codexIDString(m["id"])
	p.mu.Lock()
	if p.approvals == nil {
		p.approvals = map[string]string{}
	}
	p.approvals[id] = method
	p.mu.Unlock()
	question := codexApprovalQuestion(method, params)
	return []Event{{Type: EventPermission, Permission: &Permission{
		ID: id, Tool: method, Question: question,
	}}}
}

func codexApprovalQuestion(method string, params map[string]any) string {
	switch {
	case strings.Contains(method, "commandExecution"):
		if cmd := str(params["command"]); cmd != "" {
			return "Allow command: " + cmd
		}
	case strings.Contains(method, "fileChange"):
		return "Allow file changes"
	case strings.Contains(method, "permissions"):
		return "Allow requested permissions"
	case strings.Contains(method, "requestUserInput"):
		if q := str(params["question"]); q != "" {
			return q
		}
		return "Codex needs input"
	case strings.Contains(method, "elicitation"):
		if q := str(params["message"]); q != "" {
			return q
		}
		return "Codex requests input"
	case method == "execCommandApproval":
		return "Allow command execution"
	case method == "applyPatchApproval":
		return "Allow patch application"
	}
	return "Codex requests approval"
}

func (p *codexProtocol) parseTurnCompleted(params map[string]any) []Event {
	turn, _ := params["turn"].(map[string]any)
	status := ""
	if turn != nil {
		status = str(turn["status"])
	}
	p.mu.Lock()
	p.turnActive = false
	p.turnID = ""
	p.mu.Unlock()
	switch status {
	case "", "completed", "success":
		p.mu.Lock()
		p.sawResult = true
		p.mu.Unlock()
		return []Event{{Type: EventResult, Result: &Result{Subtype: "success"}}}
	case "interrupted", "cancelled", "canceled":
		p.mu.Lock()
		p.sawResult = true
		p.mu.Unlock()
		return []Event{{Type: EventResult, Result: &Result{Subtype: "interrupted", IsError: true, Text: "turn interrupted"}}}
	default:
		p.mu.Lock()
		p.sawResult = true
		p.mu.Unlock()
		text := ""
		if turn != nil {
			switch e := turn["error"].(type) {
			case string:
				text = e
			case map[string]any:
				text = str(e["message"])
				if text == "" {
					text = str(e["error"])
				}
			}
		}
		if text == "" {
			text = "turn " + status
		}
		return []Event{{Type: EventResult, Result: &Result{Subtype: status, IsError: true, Text: text}}}
	}
}

func (p *codexProtocol) parseItem(method string, params map[string]any) []Event {
	item, _ := params["item"].(map[string]any)
	if item == nil {
		return nil
	}
	typ := str(item["type"])
	id := str(item["id"])
	completed := method == "item/completed"
	status := "started"
	if completed {
		status = "completed"
	}
	switch typ {
	case "agentMessage":
		if text := str(item["text"]); text != "" {
			return []Event{{Type: EventAssistant, Text: text, Delta: !completed}}
		}
	case "reasoning":
		return nil
	case "commandExecution":
		output := str(item["aggregatedOutput"])
		if output == "" {
			output = str(item["output"])
		}
		if code := codexExitCode(item["exitCode"]); code != nil && *code != 0 {
			status = "failed"
		}
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: id, Name: "command", Input: str(item["command"]), Output: output, Status: status,
		}}}
	case "fileChange":
		return []Event{{Type: EventTool, Tool: &ToolEvent{ID: id, Name: "fileChange", Status: status}}}
	case "mcpToolCall":
		args := ""
		if raw, ok := item["arguments"]; ok {
			if b, err := json.Marshal(raw); err == nil {
				args = string(b)
			}
		}
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: id, Name: "mcp:" + str(item["tool"]), Input: args, Status: status,
		}}}
	case "webSearch":
		return []Event{{Type: EventTool, Tool: &ToolEvent{ID: id, Name: "web_search", Status: status}}}
	}
	return nil
}

// Exit classifies a process exit that never produced a turn/completed frame.
func (p *codexProtocol) Exit(code int, err error, stderr string) []Event {
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
			text = fmt.Sprintf("codex exited with code %d before turn/completed", code)
		}
	}
	c := ClassifyError(text, 0)
	return []Event{{Type: EventError, Error: text, EndReason: string(c.Class), Hint: c.Hint}}
}

// codexThreadID reads the thread id from a thread/start|resume response.
func codexThreadID(res map[string]any) string {
	if id := str(res["threadId"]); id != "" {
		return id
	}
	if t, _ := res["thread"].(map[string]any); t != nil {
		return str(t["id"])
	}
	return str(res["id"])
}

func codexUsage(params map[string]any) *Usage {
	tu, _ := params["tokenUsage"].(map[string]any)
	if tu == nil {
		tu = params
	}
	total, _ := tu["total"].(map[string]any)
	if total == nil {
		total = tu
	}
	u := &Usage{
		Input:         int64(num(total["inputTokens"]) + num(total["input_tokens"])),
		Output:        int64(num(total["outputTokens"]) + num(total["output_tokens"])),
		CacheRead:     int64(num(total["cachedInputTokens"]) + num(total["cached_input_tokens"])),
		CacheCreation: int64(num(total["cacheCreationInputTokens"]) + num(total["cache_creation_input_tokens"])),
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheCreation == 0 {
		return nil
	}
	return u
}

func codexIDString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%d", int64(t))
	case json.Number:
		return t.String()
	}
	return fmt.Sprint(v)
}

func codexIntID(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case json.Number:
		i, err := t.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func codexExitCode(v any) *int {
	if v == nil {
		return nil
	}
	n := int(num(v))
	return &n
}

func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}
