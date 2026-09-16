package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// claudeSDKVersion is the SDK revision this wire was ported from:
// claude-agent-sdk-python @ main 2026-09 (transport/query/types). The runs
// layer records it on the run log; the fixture directory names the CLI build.
const claudeSDKVersion = "trebi-cli/1"

// claudeMinVersion is the SDK's documented CLI floor (E.1).
const claudeMinVersion = "2.0.0"

// claudeVersionTTL caches the --version probe: the pinned binary changes
// rarely and every run would otherwise pay a process spawn.
const claudeVersionTTL = 5 * time.Minute

var claudeVersionCache struct {
	mu   sync.Mutex
	seen map[string]claudeVersionEntry
}

type claudeVersionEntry struct {
	at  time.Time
	ver string
}

// claudeCheckVersion probes `claude --version` and fails closed below the SDK
// floor (E.1): an old binary may not speak stream-json or the control channel.
func claudeCheckVersion(ctx context.Context, bin string) error {
	ver, ok := cachedClaudeVersion(ctx, bin)
	if !ok || strings.TrimSpace(ver) == "" {
		return fmt.Errorf("claude: cannot determine --version for %s; pin a CLI >= %s (trebi doctor) or relaunch with runtime: tui", bin, claudeMinVersion)
	}
	if !semverAtLeast(ver, claudeMinVersion) {
		return fmt.Errorf("claude %s is below the SDK floor %s: update the binary pin (trebi doctor) or relaunch with runtime: tui", firstToken(ver), claudeMinVersion)
	}
	return nil
}

// cachedClaudeVersion returns the version line, probing at most once per TTL.
func cachedClaudeVersion(ctx context.Context, bin string) (string, bool) {
	claudeVersionCache.mu.Lock()
	if claudeVersionCache.seen == nil {
		claudeVersionCache.seen = map[string]claudeVersionEntry{}
	}
	if e, ok := claudeVersionCache.seen[bin]; ok && time.Since(e.at) < claudeVersionTTL {
		claudeVersionCache.mu.Unlock()
		return e.ver, true
	}
	claudeVersionCache.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ver := probeVersion(probeCtx, bin)
	if ver != "" {
		claudeVersionCache.mu.Lock()
		claudeVersionCache.seen[bin] = claudeVersionEntry{at: time.Now(), ver: ver}
		claudeVersionCache.mu.Unlock()
	}
	return ver, ver != ""
}

// semverAtLeast reports got >= want. Unparsable versions are not >=.
func semverAtLeast(got, want string) bool {
	gmaj, gmin, gpatch, ok := parseSemver(got)
	if !ok {
		return false
	}
	wmaj, wmin, wpatch, ok := parseSemver(want)
	if !ok {
		return false
	}
	if gmaj != wmaj {
		return gmaj > wmaj
	}
	if gmin != wmin {
		return gmin > wmin
	}
	return gpatch >= wpatch
}

func parseSemver(s string) (major, minor, patch int, ok bool) {
	s = strings.TrimPrefix(firstToken(s), "v")
	if i := strings.IndexAny(s, "-+"); i > 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return 0, 0, 0, false
	}
	for i := 0; i < len(parts) && i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return 0, 0, 0, false
		}
		switch i {
		case 0:
			major = n
		case 1:
			minor = n
		case 2:
			patch = n
		}
	}
	return major, minor, patch, true
}

func firstToken(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// ClaudeDriver speaks Claude Code's stream-json wire over a child process.
type ClaudeDriver struct{}

// Name implements Driver.
func (ClaudeDriver) Name() string { return "claude" }

// Start implements Driver: persistent stream-json session (no -p).
func (ClaudeDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	return claudeStart(ctx, req)
}

// Resume implements Driver: same wire with the prepared --resume=<id>.
func (ClaudeDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return claudeStart(ctx, req)
}

func claudeStart(ctx context.Context, req StartRequest) (Session, error) {
	argv := SplitArgs(req.BaseCommand)
	if len(argv) == 0 {
		return nil, errors.New("claude: empty command")
	}
	bin, base := argv[0], argv[1:]
	if err := claudeCheckVersion(ctx, bin); err != nil {
		return nil, err
	}
	// Option-value flags use the equals form: the CLI parses a dash-leading
	// value as a new flag otherwise (A.9, SDK port).
	base = equalsForm(base, "--session-id")
	base = equalsForm(base, "--resume")
	base = equalsForm(base, "--permission-mode")

	wire := []string{"--output-format", "stream-json", "--verbose", "--include-partial-messages"}
	if req.OneShot {
		// Narrow one-shot path (A.7): -p plus stream-json input. Stdin stays
		// open until the result so permission control frames are still
		// answered; the runs layer closes it on terminate.
		wire = append(wire, "--print", "--input-format", "stream-json")
	} else {
		wire = append(wire, "--input-format", "stream-json")
	}
	args := append(append([]string{}, wire...), base...)
	// The SDK enables the host permission callback with `stdio`; without it
	// the CLI denies prompts itself and never sends can_use_tool (E.1).
	if !hasFlag(args, "--permission-prompt-tool") {
		args = append(args, "--permission-prompt-tool", "stdio")
	}
	args = append(args, req.ExtraArgs...)

	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	// Honest SDK-class marker: never "cli" (A.12).
	env["CLAUDE_CODE_ENTRYPOINT"] = "trebi-cli"
	env["CLAUDE_AGENT_SDK_VERSION"] = claudeSDKVersion
	if req.PermissionMode != "" && !hasFlag(base, "--permission-mode") {
		args = append(args, "--permission-mode", req.PermissionMode)
	}

	proc, err := StartProc(ctx, ProcOpts{Path: bin, Args: args, Dir: req.WorkingDir, Env: childEnv(env)})
	if err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	proto := newClaudeProtocol()
	s := startWire(ctx, "claude", proc, proto)
	proto.setWriter(&wireWriter{s: s})
	if req.SessionID != "" {
		s.setID(req.SessionID)
	}
	if req.OneShot {
		if err := s.Prompt(ctx, Prompt{Text: req.OneShotPrompt, Kind: "job"}); err != nil {
			_ = proc.gracefulClose(gracefulCloseTimeout)
			return nil, fmt.Errorf("claude: write prompt: %w", err)
		}
		// Do not close stdin here: the control channel must stay writable
		// until the terminal result lands (permission answerer, A.10).
	}
	return s, nil
}

// equalsForm rewrites `--flag value` to `--flag=value` once.
func equalsForm(args []string, flag string) []string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			out := append([]string{}, args[:i]...)
			out = append(out, flag+"="+args[i+1])
			return append(out, args[i+2:]...)
		}
	}
	return args
}

// hasFlag reports an exact flag token or its equals form.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// claudeProtocol ports the SDK's stream-json message/control shapes.
type claudeProtocol struct {
	mu        sync.Mutex
	w         *wireWriter
	pending   map[string]json.RawMessage
	sawResult bool
	sawInit   bool
}

func newClaudeProtocol() *claudeProtocol {
	return &claudeProtocol{pending: map[string]json.RawMessage{}}
}

func (p *claudeProtocol) setWriter(w *wireWriter) {
	p.mu.Lock()
	p.w = w
	p.mu.Unlock()
}

// Handshake sends the SDK's initialize control request without waiting for
// the reply: the first prompt goes out immediately (Part C).
func (p *claudeProtocol) Handshake(_ context.Context, w *wireWriter) []Event {
	frame, err := jsonLine(map[string]any{
		"type":       "control_request",
		"request_id": nextRequestID(),
		"request":    map[string]any{"subtype": "initialize", "hooks": nil},
	})
	if err != nil {
		return nil
	}
	_ = w.Write(frame)
	return nil
}

// EncodePrompt ports the SDK user-message shape exactly.
func (p *claudeProtocol) EncodePrompt(pr Prompt) ([]byte, error) {
	return jsonLine(map[string]any{
		"type":               "user",
		"session_id":         "",
		"message":            map[string]any{"role": "user", "content": pr.Text},
		"parent_tool_use_id": nil,
	})
}

func (p *claudeProtocol) EncodeInterrupt() ([]byte, error) {
	return jsonLine(map[string]any{
		"type":       "control_request",
		"request_id": nextRequestID(),
		"request":    map[string]any{"subtype": "interrupt"},
	})
}

// EncodeDecision answers a can_use_tool control request.
func (p *claudeProtocol) EncodeDecision(id string, d Decision) ([]byte, error) {
	p.mu.Lock()
	orig := p.pending[id]
	delete(p.pending, id)
	p.mu.Unlock()

	response := map[string]any{}
	if d.Allow {
		response["behavior"] = "allow"
		switch {
		case len(d.UpdatedInput) > 0:
			response["updatedInput"] = json.RawMessage(d.UpdatedInput)
		case len(orig) > 0:
			response["updatedInput"] = json.RawMessage(orig)
		default:
			response["updatedInput"] = map[string]any{}
		}
	} else {
		response["behavior"] = "deny"
		if d.Message != "" {
			response["message"] = d.Message
		}
	}
	return jsonLine(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": id,
			"response":   response,
		},
	})
}

// Parse maps one stdout frame to normalized events.
func (p *claudeProtocol) Parse(line []byte) []Event {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return nil
	}
	if !strings.HasPrefix(trimmed, "{") {
		// The CLI writes non-JSON diagnostics such as [SandboxDebug] to
		// stdout; the SDK skips them. Keep them out of the event stream but
		// do not fail the wire.
		return []Event{{Type: EventStatus, Text: trimmed, Status: StatusRunning}}
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		return []Event{{Type: EventError, Error: "claude: malformed JSON frame: " + err.Error(), EndReason: string(FailProtocol)}}
	}
	switch str(m["type"]) {
	case "system":
		return p.parseSystem(m)
	case "assistant":
		return p.parseAssistant(m)
	case "user":
		return p.parseUser(m)
	case "stream_event":
		return p.parseStreamEvent(m)
	case "result":
		return p.parseResult(m)
	case "control_request":
		return p.parseControlRequest(m)
	case "rate_limit_event":
		return p.parseRateLimit(m)
	case "control_response", "control_cancel_request", "transcript_mirror":
		return nil
	}
	return nil
}

func (p *claudeProtocol) parseSystem(m map[string]any) []Event {
	switch str(m["subtype"]) {
	case "init":
		p.mu.Lock()
		p.sawInit = true
		p.mu.Unlock()
		ev := Event{Type: EventInit, SessionID: str(m["session_id"])}
		if model := str(m["model"]); model != "" {
			ev.Text = model
		}
		return []Event{ev}
	case "session_state_changed":
		state := str(m["state"])
		switch state {
		case "idle":
			return []Event{{Type: EventStatus, Status: StatusIdle}}
		case "running":
			return []Event{{Type: EventStatus, Status: StatusRunning}}
		}
		return nil
	case "status":
		// e.g. compacting; the turn is still active.
		return []Event{{Type: EventStatus, Status: StatusRunning, Text: str(m["status"])}}
	case "compact_boundary":
		return []Event{{Type: EventStatus, Status: StatusRunning, Text: "compacting"}}
	}
	return nil
}

func (p *claudeProtocol) parseAssistant(m map[string]any) []Event {
	msg, _ := m["message"].(map[string]any)
	if msg == nil {
		return nil
	}
	var out []Event
	for _, block := range contentBlocks(msg["content"]) {
		switch str(block["type"]) {
		case "text":
			if text := str(block["text"]); text != "" {
				out = append(out, Event{Type: EventAssistant, Text: text})
			}
		case "tool_use":
			input := "{}"
			if raw, ok := block["input"]; ok {
				if b, err := json.Marshal(raw); err == nil {
					input = string(b)
				}
			}
			out = append(out, Event{Type: EventTool, Tool: &ToolEvent{
				ID: str(block["id"]), Name: str(block["name"]), Input: input, Status: "started",
			}})
		}
	}
	if u := messageUsage(msg["usage"]); u != nil {
		out = append(out, Event{Type: EventUsage, Usage: u})
	}
	return out
}

func (p *claudeProtocol) parseUser(m map[string]any) []Event {
	msg, _ := m["message"].(map[string]any)
	if msg == nil {
		return nil
	}
	var out []Event
	for _, block := range contentBlocks(msg["content"]) {
		switch str(block["type"]) {
		case "tool_result":
			status := "completed"
			if isErr, _ := block["is_error"].(bool); isErr {
				status = "failed"
			}
			out = append(out, Event{Type: EventTool, Tool: &ToolEvent{
				ID: str(block["tool_use_id"]), Output: flattenContent(block["content"]), Status: status,
			}})
		case "text":
			if text := str(block["text"]); text != "" {
				out = append(out, Event{Type: EventUser, Text: text})
			}
		}
	}
	if len(out) == 0 {
		if text, ok := msg["content"].(string); ok && text != "" {
			out = append(out, Event{Type: EventUser, Text: text})
		}
	}
	return out
}

func (p *claudeProtocol) parseStreamEvent(m map[string]any) []Event {
	ev, _ := m["event"].(map[string]any)
	if ev == nil {
		return nil
	}
	switch str(ev["type"]) {
	case "content_block_delta":
		delta, _ := ev["delta"].(map[string]any)
		if delta == nil {
			return nil
		}
		if str(delta["type"]) == "text_delta" {
			if text := str(delta["text"]); text != "" {
				return []Event{{Type: EventAssistant, Text: text, Delta: true}}
			}
		}
	case "content_block_start":
		block, _ := ev["content_block"].(map[string]any)
		if block != nil && str(block["type"]) == "tool_use" {
			input := "{}"
			if raw, ok := block["input"]; ok {
				if b, err := json.Marshal(raw); err == nil {
					input = string(b)
				}
			}
			return []Event{{Type: EventTool, Tool: &ToolEvent{
				ID: str(block["id"]), Name: str(block["name"]), Input: input, Status: "started",
			}}}
		}
	case "message_start":
		msg, _ := ev["message"].(map[string]any)
		if msg != nil {
			if u := messageUsage(msg["usage"]); u != nil {
				return []Event{{Type: EventUsage, Usage: u}}
			}
		}
	case "message_delta":
		if u := messageUsage(ev["usage"]); u != nil {
			return []Event{{Type: EventUsage, Usage: u}}
		}
	}
	return nil
}

func (p *claudeProtocol) parseResult(m map[string]any) []Event {
	p.mu.Lock()
	p.sawResult = true
	p.mu.Unlock()
	subtype := str(m["subtype"])
	isErr, _ := m["is_error"].(bool)
	res := &Result{
		Subtype:   subtype,
		IsError:   isErr,
		SessionID: str(m["session_id"]),
	}
	res.Text = resultFrameText(m)
	if u := messageUsage(m["usage"]); u != nil {
		res.Usage = *u
	}
	if cost, ok := m["total_cost_usd"].(float64); ok {
		res.Usage.CostUSD = cost
	}
	if isErr {
		c := ClassifyResult(res)
		res.EndReason = string(c.Class)
	}
	return []Event{{Type: EventResult, Result: res, SessionID: res.SessionID}}
}

// resultFrameText ports _error_result_text.
func resultFrameText(m map[string]any) string {
	var errs []string
	switch v := m["errors"].(type) {
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				errs = append(errs, s)
			}
		}
	case string:
		if v != "" {
			errs = append(errs, v)
		}
	}
	status := 0
	if f, ok := m["api_error_status"].(float64); ok {
		status = int(f)
	}
	return resultErrorText(errs, str(m["result"]), str(m["subtype"]), status)
}

func (p *claudeProtocol) parseRateLimit(m map[string]any) []Event {
	status := str(m["status"])
	if status == "rejected" || status == "blocked" {
		text := str(m["message"])
		if text == "" {
			text = "claude rate limit rejected"
		}
		c := ClassifyError(text, 0)
		return []Event{{Type: EventError, Error: text, EndReason: string(c.Class), Hint: c.Hint}}
	}
	return []Event{{Type: EventStatus, Status: StatusRunning, Text: "rate_limit:" + status}}
}

func (p *claudeProtocol) parseControlRequest(m map[string]any) []Event {
	request, _ := m["request"].(map[string]any)
	if request == nil {
		return nil
	}
	id := str(m["request_id"])
	switch str(request["subtype"]) {
	case "can_use_tool":
		input := "{}"
		if raw, ok := request["input"]; ok {
			if b, err := json.Marshal(raw); err == nil {
				input = string(b)
				p.mu.Lock()
				p.pending[id] = append(json.RawMessage(nil), b...)
				p.mu.Unlock()
			}
		}
		tool := str(request["tool_name"])
		question := str(request["title"])
		if question == "" {
			question = str(request["description"])
		}
		if question == "" {
			question = "Allow " + tool + "?"
		}
		return []Event{{Type: EventPermission, Permission: &Permission{
			ID: id, Tool: tool, Question: question, Input: input,
		}}}
	default:
		// A control request we do not implement must still be answered or the
		// CLI hangs (pre-2.1.259 especially). Answer with an explicit error.
		frame, _ := jsonLine(map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype":    "error",
				"request_id": id,
				"error":      "trebi: unsupported control request subtype " + str(request["subtype"]),
			},
		})
		p.mu.Lock()
		w := p.w
		p.mu.Unlock()
		if w != nil {
			_ = w.Write(frame)
		}
		return nil
	}
}

// Exit classifies a process exit that never produced a result frame.
func (p *claudeProtocol) Exit(code int, err error, stderr string) []Event {
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
			text = fmt.Sprintf("claude exited with code %d before a result", code)
		}
	}
	c := ClassifyError(text, 0)
	return []Event{{Type: EventError, Error: text, EndReason: string(c.Class), Hint: c.Hint}}
}

func contentBlocks(v any) []map[string]any {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if b, ok := item.(map[string]any); ok {
			out = append(out, b)
		}
	}
	return out
}

func messageUsage(v any) *Usage {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	u := &Usage{}
	u.Input = int64(num(m["input_tokens"]))
	u.Output = int64(num(m["output_tokens"]))
	u.CacheRead = int64(num(m["cache_read_input_tokens"]))
	u.CacheCreation = int64(num(m["cache_creation_input_tokens"]))
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheCreation == 0 {
		return nil
	}
	if model := str(m["model"]); model != "" {
		u.Model = model
	}
	return u
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// flattenContent renders a tool_result content value (string or block list).
func flattenContent(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var parts []string
		for _, item := range t {
			if b, ok := item.(map[string]any); ok {
				if s := str(b["text"]); s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
