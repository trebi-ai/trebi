package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// piStart spawns `pi --mode rpc` and drives its NDJSON RPC (E.4).
func piStart(ctx context.Context, req StartRequest) (Session, error) {
	argv := SplitArgs(req.BaseCommand)
	if len(argv) == 0 {
		return nil, errors.New("pi: empty command")
	}
	bin := argv[0]
	args := append([]string{}, argv[1:]...)
	if !hasFlag(args, "--mode") && !hasFlag(args, "--rpc") {
		args = append(args, "--mode", "rpc")
	}
	proto := &piProtocol{waiters: map[string]chan map[string]any{}}
	proc, err := StartProc(ctx, ProcOpts{Path: bin, Args: args, Dir: req.WorkingDir, Env: childEnv(req.Env)})
	if err != nil {
		return nil, fmt.Errorf("pi: %w", err)
	}
	s := startWire(ctx, "pi", proc, proto)
	proto.setWriter(&wireWriter{s: s})
	if req.SessionID != "" {
		s.setID(req.SessionID)
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

// piProtocol maps pi's RPC vocabulary. Captured 2026-09-16 against pi 0.84.1:
// agent_start / turn_start / message_start|end (role, content blocks) /
// turn_end / agent_end. Deltas arrive as message_update where supported.
type piProtocol struct {
	mu       sync.Mutex
	w        *wireWriter
	waiters  map[string]chan map[string]any
	sawEnd   bool
	sawError string
}

func (p *piProtocol) setWriter(w *wireWriter) {
	p.mu.Lock()
	p.w = w
	p.mu.Unlock()
}

func (p *piProtocol) write(v any) error {
	p.mu.Lock()
	w := p.w
	p.mu.Unlock()
	if w == nil {
		return errors.New("pi: not connected")
	}
	b, err := jsonLine(v)
	if err != nil {
		return err
	}
	return w.Write(b)
}

func (p *piProtocol) Handshake(context.Context, *wireWriter) []Event { return nil }

// EncodePrompt sends the prompt command. pi echoes message events back.
func (p *piProtocol) EncodePrompt(pr Prompt) ([]byte, error) {
	return jsonLine(map[string]any{"type": "prompt", "message": pr.Text})
}

func (p *piProtocol) EncodeInterrupt() ([]byte, error) {
	return jsonLine(map[string]any{"type": "abort"})
}

func (p *piProtocol) EncodeDecision(id string, d Decision) ([]byte, error) {
	return jsonLine(map[string]any{"type": "permission_response", "id": id, "approved": d.Allow, "message": d.Message})
}

func (p *piProtocol) Parse(line []byte) []Event {
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		return nil
	}
	switch str(m["type"]) {
	case "ready", "session_start", "session_started":
		return []Event{{Type: EventInit, SessionID: str(m["session_id"])}}
	case "agent_start", "turn_start":
		return []Event{{Type: EventStatus, Status: StatusRunning}}
	case "message_start", "message_end":
		return p.parseMessage(str(m["type"]), m)
	case "turn_end":
		p.noteError(m)
		return nil
	case "message_update":
		ev, _ := m["assistantMessageEvent"].(map[string]any)
		if ev == nil {
			ev, _ = m["event"].(map[string]any)
		}
		if ev == nil {
			return nil
		}
		switch str(ev["type"]) {
		case "text_delta", "text":
			if text := orDefault(str(ev["delta"]), str(ev["text"])); text != "" {
				return []Event{{Type: EventAssistant, Text: text, Delta: str(ev["type"]) == "text_delta"}}
			}
		case "tool_use", "tool_call":
			return []Event{{Type: EventTool, Tool: &ToolEvent{
				ID: str(ev["id"]), Name: str(ev["name"]), Status: "started",
			}}}
		case "tool_result":
			return []Event{{Type: EventTool, Tool: &ToolEvent{
				ID: str(ev["id"]), Name: str(ev["name"]), Status: "completed",
			}}}
		}
	case "message":
		if text := str(m["text"]); text != "" {
			return []Event{{Type: EventAssistant, Text: text}}
		}
	case "tool_execution_start", "tool_execution_end":
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: str(m["id"]), Name: str(m["tool"]),
			Status: map[bool]string{true: "completed", false: "started"}[str(m["type"]) == "tool_execution_end"],
		}}}
	case "permission_request", "permission":
		return []Event{{Type: EventPermission, Permission: &Permission{
			ID: str(m["id"]), Tool: str(m["tool"]),
			Question: orDefault(str(m["message"]), orDefault(str(m["question"]), "pi requests permission")),
		}}}
	case "agent_end":
		p.mu.Lock()
		p.sawEnd = true
		lastErr := p.sawError
		p.mu.Unlock()
		if lastErr != "" {
			c := ClassifyError(lastErr, 0)
			return []Event{{Type: EventResult, Result: &Result{
				Subtype: "error_during_execution", IsError: true, Text: lastErr,
				EndReason: string(c.Class),
			}}}
		}
		return []Event{{Type: EventResult, Result: &Result{Subtype: "success"}}}
	case "error":
		msg := str(m["error"])
		if msg == "" {
			msg = str(m["message"])
		}
		if msg == "" {
			msg = fmt.Sprint(m["error"])
		}
		p.mu.Lock()
		p.sawError = msg
		p.mu.Unlock()
		c := ClassifyError(msg, 0)
		return []Event{{Type: EventError, Error: msg, EndReason: string(c.Class), Hint: c.Hint}}
	case "response":
		id := str(m["id"])
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
	return nil
}

// parseMessage maps pi's message_start/message_end frames: assistant text into
// assistant events, toolCall blocks into tool events, and the final usage on
// message_end into a usage event (message_start usage is all zeros).
func (p *piProtocol) parseMessage(kind string, m map[string]any) []Event {
	msg, _ := m["message"].(map[string]any)
	if msg == nil {
		return nil
	}
	p.noteError(msg)
	if role := str(msg["role"]); role == "user" {
		return nil
	}
	var out []Event
	for _, block := range contentBlocks(msg["content"]) {
		switch str(block["type"]) {
		case "text":
			if text := str(block["text"]); text != "" {
				out = append(out, Event{Type: EventAssistant, Text: text})
			}
		case "toolCall":
			input := ""
			if raw, ok := block["arguments"]; ok {
				if b, err := json.Marshal(raw); err == nil {
					input = string(b)
				}
			}
			out = append(out, Event{Type: EventTool, Tool: &ToolEvent{
				ID: str(block["id"]), Name: str(block["name"]), Input: input, Status: "started",
			}})
		case "toolResult":
			out = append(out, Event{Type: EventTool, Tool: &ToolEvent{
				ID: str(block["id"]), Name: str(block["name"]), Output: flattenContent(block["content"]),
				Status: "completed",
			}})
		}
	}
	if kind == "message_end" {
		if u := piUsage(msg["usage"]); u != nil {
			out = append(out, Event{Type: EventUsage, Usage: u})
		}
	}
	return out
}

// noteError records a message-level provider failure so agent_end can report
// a failed result instead of a silent success.
func (p *piProtocol) noteError(m map[string]any) {
	msg, _ := m["message"].(map[string]any)
	key := m
	if msg != nil {
		key = msg
	}
	if text := str(key["errorMessage"]); text != "" {
		p.mu.Lock()
		p.sawError = text
		p.mu.Unlock()
	}
	if stop := str(key["stopReason"]); stop == "error" {
		p.mu.Lock()
		if p.sawError == "" {
			p.sawError = "pi turn ended with stopReason error"
		}
		p.mu.Unlock()
	}
}

func piUsage(v any) *Usage {
	m, _ := v.(map[string]any)
	if m == nil {
		return nil
	}
	u := &Usage{
		Input:     int64(num(m["input"])),
		Output:    int64(num(m["output"])),
		CacheRead: int64(num(m["cacheRead"])),
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 {
		return nil
	}
	if cost, _ := m["cost"].(map[string]any); cost != nil {
		u.CostUSD = num(cost["total"])
	}
	return u
}

func (p *piProtocol) Exit(code int, err error, stderr string) []Event {
	p.mu.Lock()
	saw := p.sawEnd
	sawErr := p.sawError
	p.mu.Unlock()
	if saw {
		return nil
	}
	text := strings.TrimSpace(stderr)
	if text == "" {
		text = sawErr
	}
	if text == "" && err != nil {
		text = err.Error()
	}
	if text == "" {
		text = fmt.Sprintf("pi exited with code %d before agent_end", code)
	}
	c := ClassifyError(text, 0)
	return []Event{{Type: EventError, Error: text, EndReason: string(c.Class), Hint: c.Hint}}
}
