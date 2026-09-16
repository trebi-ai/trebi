package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ShutdownOpenCodeServers stops every daemon-owned shared server.
func ShutdownOpenCodeServers() { openCodeServers.shutdown() }

// releaseOpenCodeServer drops one refcount and stops the shared server on the
// last release after the idle window.
func releaseOpenCodeServer(srv *openCodeServer) {
	srv.release(openCodeServers.idle, func() {
		openCodeServers.mu.Lock()
		delete(openCodeServers.servers, srv.key)
		openCodeServers.mu.Unlock()
		srv.stop()
	})
}

// openCodeLive remembers which live serve helper holds each session so the
// transcript fallback can pull persisted messages after the stream is gone.
var openCodeLive struct {
	mu   sync.Mutex
	refs map[string]openCodeSessionRef
}

type openCodeSessionRef struct {
	srv *openCodeServer
	dir string
}

func openCodeRemember(sessionID string, srv *openCodeServer, dir string) {
	if sessionID == "" || srv == nil {
		return
	}
	openCodeLive.mu.Lock()
	if openCodeLive.refs == nil {
		openCodeLive.refs = map[string]openCodeSessionRef{}
	}
	openCodeLive.refs[sessionID] = openCodeSessionRef{srv: srv, dir: dir}
	openCodeLive.mu.Unlock()
}

func openCodeForgetServer(srv *openCodeServer) {
	openCodeLive.mu.Lock()
	for id, ref := range openCodeLive.refs {
		if ref.srv == srv {
			delete(openCodeLive.refs, id)
		}
	}
	openCodeLive.mu.Unlock()
}

// OpenCodeMessages returns the persisted message list for one session from a
// live daemon-owned server (Part H transcript fallback). ok=false means no
// serve helper currently holds the session.
func OpenCodeMessages(ctx context.Context, sessionID string) ([]byte, bool) {
	openCodeLive.mu.Lock()
	ref, ok := openCodeLive.refs[sessionID]
	openCodeLive.mu.Unlock()
	if !ok || ref.srv == nil {
		return nil, false
	}
	var raw json.RawMessage
	if err := ref.srv.call(ctx, "GET", "/session/"+sessionID+"/message", ref.dir, nil, &raw); err != nil {
		return nil, false
	}
	return raw, len(raw) > 0
}

// openCodeStart attaches to (or creates) an OpenCode session on a shared
// `opencode serve` helper (E.3).
func openCodeStart(ctx context.Context, req StartRequest) (Session, error) {
	argv := SplitArgs(req.BaseCommand)
	if len(argv) == 0 {
		return nil, errors.New("opencode: empty command")
	}
	bin := argv[0]
	srv, err := openCodeServers.acquireServer(ctx, bin, req.Env, nil)
	if err != nil {
		return nil, err
	}
	directory := req.WorkingDir
	sessionID := req.SessionID
	if sessionID != "" {
		var info map[string]any
		if err := srv.call(ctx, "GET", "/session/"+sessionID, directory, nil, &info); err != nil {
			releaseOpenCodeServer(srv)
			return nil, fmt.Errorf("%w: opencode session %s: %v", ErrResumeUnsupported, sessionID, err)
		}
	} else {
		var res struct {
			ID string `json:"id"`
		}
		if err := srv.call(ctx, "POST", "/session", directory, map[string]any{}, &res); err != nil {
			releaseOpenCodeServer(srv)
			return nil, fmt.Errorf("opencode session create: %w", err)
		}
		sessionID = res.ID
	}
	s := &openCodeSession{
		srv:       srv,
		id:        sessionID,
		directory: directory,
		events:    make(chan Event, 256),
		done:      make(chan struct{}),
		parts:     map[string]int{},
	}
	openCodeRemember(sessionID, srv, directory)
	streamCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	if err := s.start(streamCtx); err != nil {
		cancel()
		s.releaseServer()
		return nil, err
	}
	return s, nil
}

// openCodeSession is one OpenCode conversation over HTTP + SSE.
type openCodeSession struct {
	srv       *openCodeServer
	id        string
	directory string

	events chan Event
	done   chan struct{}
	cancel context.CancelFunc

	mu        sync.Mutex
	parts     map[string]int
	sawResult bool
	lastError string
	released  bool
}

func (s *openCodeSession) Provider() string { return "opencode" }
func (s *openCodeSession) ID() string       { return s.id }
func (s *openCodeSession) PID() int         { return s.srv.proc.PID() }
func (s *openCodeSession) Pgid() int        { return s.srv.proc.Pgid() }

// Argv is the daemon-owned serve command (pid ledger evidence).
func (s *openCodeSession) Argv() []string { return s.srv.argv }
func (s *openCodeSession) Events() <-chan Event {
	return s.events
}

func (s *openCodeSession) start(ctx context.Context) error {
	stream, err := s.srv.stream(ctx, s.directory)
	if err != nil {
		return err
	}
	go func() {
		defer close(s.events)
		defer close(s.done)
		defer s.releaseServer()
		for frame := range stream {
			if len(frame.Data) == 0 {
				continue
			}
			var m map[string]any
			if json.Unmarshal(frame.Data, &m) != nil {
				continue
			}
			for _, ev := range s.parse(m) {
				select {
				case s.events <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
		s.mu.Lock()
		saw := s.sawResult
		lastErr := s.lastError
		s.mu.Unlock()
		if !saw {
			msg := lastErr
			if msg == "" {
				msg = "opencode event stream ended before the session went idle"
			}
			s.emit(ctx, Event{Type: EventError, Error: msg, EndReason: string(FailProtocol)})
		}
		s.emit(ctx, Event{Type: EventExit, ExitCode: exitCodePtr(s.srv.proc.ExitCode())})
	}()
	return nil
}

func (s *openCodeSession) emit(ctx context.Context, ev Event) {
	select {
	case s.events <- ev:
	case <-ctx.Done():
	}
}

// Prompt starts a turn without waiting (prompt_async returns 204).
func (s *openCodeSession) Prompt(ctx context.Context, p Prompt) error {
	s.mu.Lock()
	s.sawResult = false
	s.lastError = ""
	s.parts = map[string]int{}
	s.mu.Unlock()
	return s.srv.call(ctx, "POST", "/session/"+s.id+"/prompt_async", s.directory, map[string]any{
		"parts": []map[string]any{{"type": "text", "text": p.Text}},
	}, nil)
}

// AnswerPermission posts the decision (allow→once, deny→reject).
func (s *openCodeSession) AnswerPermission(ctx context.Context, permissionID string, d Decision) error {
	response := "once"
	if !d.Allow {
		response = "reject"
	}
	return s.srv.call(ctx, "POST", "/session/"+s.id+"/permissions/"+permissionID, s.directory,
		map[string]any{"response": response}, nil)
}

// Interrupt aborts the current turn.
func (s *openCodeSession) Interrupt(ctx context.Context) error {
	return s.srv.call(ctx, "POST", "/session/"+s.id+"/abort", s.directory, map[string]any{}, nil)
}

// InterruptTurn is the protocol interrupt.
func (s *openCodeSession) InterruptTurn(ctx context.Context) error { return s.Interrupt(ctx) }

// Close aborts and releases the shared server refcount.
func (s *openCodeSession) Close(ctx context.Context) error {
	abortCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.Interrupt(abortCtx)
	cancel()
	s.cancel()
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	return nil
}

func (s *openCodeSession) releaseServer() {
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return
	}
	s.released = true
	s.mu.Unlock()
	releaseOpenCodeServer(s.srv)
}

// parse maps one OpenCode bus event to normalized events.
func (s *openCodeSession) parse(m map[string]any) []Event {
	typ := str(m["type"])
	props, _ := m["properties"].(map[string]any)
	if props == nil {
		return nil
	}
	sessionOf := openCodeSessionID(props)
	if sessionOf != "" && sessionOf != s.id {
		return nil
	}
	switch typ {
	case "server.connected":
		return []Event{{Type: EventInit, SessionID: s.id}}
	case "session.created", "session.updated":
		info, _ := props["info"].(map[string]any)
		if info != nil && str(info["id"]) == s.id {
			return []Event{{Type: EventInit, SessionID: s.id}}
		}
	case "session.idle":
		if sessionOf != "" && sessionOf != s.id {
			return nil
		}
		s.mu.Lock()
		s.sawResult = true
		lastErr := s.lastError
		s.mu.Unlock()
		if lastErr != "" {
			return []Event{{Type: EventResult, Result: &Result{
				Subtype: "error_during_execution", IsError: true, Text: lastErr,
				EndReason: string(ClassifyError(lastErr, 0).Class),
			}}}
		}
		return []Event{{Type: EventResult, Result: &Result{Subtype: "success", SessionID: s.id}}}
	case "session.error":
		msg := openCodeErrorText(props)
		s.mu.Lock()
		s.lastError = msg
		s.mu.Unlock()
		c := ClassifyError(msg, 0)
		return []Event{{Type: EventError, Error: msg, EndReason: string(c.Class), Hint: c.Hint}}
	case "session.deleted":
		return []Event{{Type: EventExit}}
	case "message.updated":
		info, _ := props["info"].(map[string]any)
		if info == nil {
			return nil
		}
		if errText := openCodeErrorText(info); errText != "" {
			s.mu.Lock()
			s.lastError = errText
			s.mu.Unlock()
			return []Event{{Type: EventError, Error: errText}}
		}
		if cost := num(info["cost"]); cost > 0 {
			if u := openCodeUsage(info); u != nil {
				return []Event{{Type: EventUsage, Usage: u}}
			}
		}
	case "message.part.updated":
		part, _ := props["part"].(map[string]any)
		if part == nil {
			return nil
		}
		return s.parsePart(part)
	case "permission.updated", "permission.asked", "permission.v2.asked":
		id := str(props["id"])
		q := str(props["title"])
		if q == "" {
			q = str(props["message"])
		}
		if q == "" {
			q = "OpenCode requests permission"
		}
		return []Event{{Type: EventPermission, Permission: &Permission{
			ID: id, Tool: str(props["type"]), Question: q,
		}}}
	}
	return nil
}

func (s *openCodeSession) parsePart(part map[string]any) []Event {
	switch str(part["type"]) {
	case "text":
		text := str(part["text"])
		if text == "" {
			return nil
		}
		id := str(part["id"])
		s.mu.Lock()
		prev := s.parts[id]
		if len(text) > prev {
			s.parts[id] = len(text)
		}
		s.mu.Unlock()
		if prev > len(text) {
			// Truncation/replacement: emit the whole block once.
			return []Event{{Type: EventAssistant, Text: text}}
		}
		if prev == 0 {
			return []Event{{Type: EventAssistant, Text: text}}
		}
		return []Event{{Type: EventAssistant, Text: text[prev:], Delta: true}}
	case "tool":
		state, _ := part["state"].(map[string]any)
		status := ""
		if state != nil {
			status = str(state["status"])
		}
		switch status {
		case "", "pending":
			status = "started"
		case "running":
			status = "started"
		case "completed":
			status = "completed"
		case "error":
			status = "failed"
		}
		input := ""
		output := ""
		if state != nil {
			if raw, ok := state["input"]; ok {
				if b, err := json.Marshal(raw); err == nil {
					input = string(b)
				}
			}
			output = str(state["output"])
		}
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: str(part["id"]), Name: str(part["tool"]), Input: input, Output: output, Status: status,
		}}}
	case "reasoning":
		return []Event{{Type: EventStatus, Status: StatusRunning}}
	}
	return nil
}

func openCodeSessionID(props map[string]any) string {
	if id := str(props["sessionID"]); id != "" {
		return id
	}
	if id := str(props["sessionId"]); id != "" {
		return id
	}
	if info, _ := props["info"].(map[string]any); info != nil {
		if id := str(info["sessionID"]); id != "" {
			return id
		}
	}
	if part, _ := props["part"].(map[string]any); part != nil {
		if id := str(part["sessionID"]); id != "" {
			return id
		}
	}
	return ""
}

func openCodeErrorText(v map[string]any) string {
	if err, _ := v["error"].(map[string]any); err != nil {
		if data, _ := err["data"].(map[string]any); data != nil {
			if msg := str(data["message"]); msg != "" {
				return msg
			}
		}
		if msg := str(err["message"]); msg != "" {
			return msg
		}
		if name := str(err["name"]); name != "" {
			return name
		}
	}
	if msg := str(v["error"]); msg != "" {
		return msg
	}
	return ""
}

func openCodeUsage(info map[string]any) *Usage {
	tokens, _ := info["tokens"].(map[string]any)
	if tokens == nil {
		return nil
	}
	u := &Usage{
		Input:     int64(num(tokens["input"])),
		Output:    int64(num(tokens["output"])),
		CacheRead: int64(num(tokens["cache"])) + int64(num(tokens["cache_read"])),
		CostUSD:   num(info["cost"]),
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CostUSD == 0 {
		return nil
	}
	return u
}

func exitCodePtr(code int) *int { return &code }
