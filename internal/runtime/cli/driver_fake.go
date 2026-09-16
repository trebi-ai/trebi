package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SplitArgs fields a prepared command string the way the tmux policy builds
// it: whitespace-separated, with single and double quotes honored so a path
// with spaces survives.
func SplitArgs(command string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
		case c == ' ' || c == '\t' || c == '\n':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// childEnv merges the process environment with the request env. Request keys
// win; CLAUDECODE is filtered (the vendor sets it inside a Claude Code parent,
// and a SDK-class child must not inherit it). Values are returned in
// "K=V" form for exec.
func childEnv(req map[string]string) []string {
	filtered := make(map[string]string, len(req)+8)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDECODE" {
			continue
		}
		filtered[k] = strings.SplitN(kv, "=", 2)[1]
	}
	for k, v := range req {
		if k == "" || k == "CLAUDECODE" {
			continue
		}
		filtered[k] = v
	}
	out := make([]string, 0, len(filtered))
	for k, v := range filtered {
		out = append(out, k+"="+v)
	}
	return out
}

// FakeDriver is the unit-test driver: it spawns a shell that emits the same
// NDJSON vocabulary the vendors do, so the runs event pump is exercised
// without a vendor binary. TREBI_CLI_FAKE_SCRIPT overrides the default.
type FakeDriver struct {
	// Script is a shell program. Empty uses the default emitter.
	Script string
	// Dir overrides the working directory (tests).
	Dir string
}

// Name implements Driver.
func (FakeDriver) Name() string { return "fake" }

// DefaultFakeScript emits a tiny assistant turn and a success result.
const DefaultFakeScript = `read -r line || true; echo '{"type":"assistant","text":"working"}'; echo '{"type":"result","subtype":"success","text":"done"}'`

// fakeProtocol parses the fake NDJSON vocabulary.
type fakeProtocol struct{}

func (fakeProtocol) Handshake(context.Context, *wireWriter) []Event { return nil }

func (fakeProtocol) EncodePrompt(p Prompt) ([]byte, error) {
	return jsonLine(map[string]any{"type": "user", "text": p.Text})
}

func (fakeProtocol) Parse(line []byte) []Event {
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		return nil
	}
	switch strings.ToLower(fmt.Sprint(m["type"])) {
	case "init":
		return []Event{{Type: EventInit, SessionID: fmt.Sprint(m["session_id"])}}
	case "assistant":
		return []Event{{Type: EventAssistant, Text: fmt.Sprint(m["text"])}}
	case "user":
		return []Event{{Type: EventUser, Text: fmt.Sprint(m["text"])}}
	case "tool":
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: fmt.Sprint(m["id"]), Name: fmt.Sprint(m["name"]),
			Input: fmt.Sprint(m["input"]), Output: fmt.Sprint(m["output"]),
			Status: orDefault(fmt.Sprint(m["status"]), "started"),
		}}}
	case "permission":
		return []Event{{Type: EventPermission, Permission: &Permission{
			ID: fmt.Sprint(m["id"]), Tool: fmt.Sprint(m["tool"]),
			Question: fmt.Sprint(m["question"]),
		}}}
	case "result":
		isErr, _ := m["is_error"].(bool)
		return []Event{{Type: EventResult, Result: &Result{
			Subtype: orDefault(fmt.Sprint(m["subtype"]), "success"),
			IsError: isErr,
			Text:    fmt.Sprint(m["text"]),
		}}}
	case "error":
		return []Event{{Type: EventError, Error: fmt.Sprint(m["error"]), EndReason: fmt.Sprint(m["end_reason"])}}
	case "status":
		return []Event{{Type: EventStatus, Status: SessionStatus(fmt.Sprint(m["status"]))}}
	}
	return nil
}

func (fakeProtocol) EncodeDecision(id string, d Decision) ([]byte, error) {
	behavior := "deny"
	if d.Allow {
		behavior = "allow"
	}
	return jsonLine(map[string]any{"type": "decision", "id": id, "behavior": behavior, "message": d.Message})
}

func (fakeProtocol) Exit(code int, err error, stderr string) []Event {
	if code == 0 {
		return nil
	}
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = fmt.Sprintf("fake harness exited with code %d", code)
	}
	return []Event{{Type: EventError, Error: msg, ExitCode: &code}}
}

func orDefault(s, def string) string {
	if s == "" || s == "<nil>" {
		return def
	}
	return s
}

// Start implements Driver.
func (f FakeDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	return f.start(ctx, req, false)
}

// Resume implements Driver.
func (f FakeDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return f.start(ctx, req, true)
}

func (f FakeDriver) start(ctx context.Context, req StartRequest, _ bool) (Session, error) {
	script := f.Script
	if script == "" {
		script = os.Getenv("TREBI_CLI_FAKE_SCRIPT")
	}
	if script == "" {
		script = DefaultFakeScript
	}
	dir := f.Dir
	if dir == "" {
		dir = req.WorkingDir
	}
	env := childEnv(req.Env)
	proc, err := StartProc(ctx, ProcOpts{
		Path: "sh", Args: []string{"-c", script}, Dir: dir, Env: env,
	})
	if err != nil {
		return nil, err
	}
	s := startWire(ctx, "fake", proc, fakeProtocol{})
	if req.SessionID != "" {
		s.setID(req.SessionID)
	}
	if req.OneShot && req.OneShotPrompt != "" {
		frame, _ := fakeProtocol{}.EncodePrompt(Prompt{Text: req.OneShotPrompt, Kind: "job"})
		if err := s.writeFrame(frame); err != nil {
			return nil, err
		}
		_ = proc.CloseStdin()
	}
	return s, nil
}

// randRequestID mirrors the SDK's request ids: req_<n>_<hex>.
var requestCounter struct {
	mu sync.Mutex
	n  int
}

func nextRequestID() string {
	requestCounter.mu.Lock()
	requestCounter.n++
	n := requestCounter.n
	requestCounter.mu.Unlock()
	return fmt.Sprintf("req_%d_%s", n, uuid.NewString()[:8])
}

// deadlineNow is the event clock (overridable in tests).
var deadlineNow = time.Now
