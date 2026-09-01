package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/pricing"
)

// Client is a headless coding-agent launcher. Defined here because Runner is
// the consumer; claudeClient and codexClient implement it.
type Client interface {
	Name() string
	Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error)
}

// LaunchOpts is the isolated trial input for a client harness.
type LaunchOpts struct {
	Workdir    string
	Prompt     string
	Env        []string
	Timeout    time.Duration
	MaxTurns   int
	SkillMD    []byte
	Transcript string // path to write raw + normalized JSON
	Surface    string
	MCPURL     string
	MCPToken   string
	ConfigHome string
}

// Step is one normalized transcript turn. Content holds the tool arguments;
// Result holds what the tool answered, so a rubric rule can grade a response.
type Step struct {
	Role    string `json:"role"`
	Name    string `json:"name,omitempty"`
	Content string `json:"content,omitempty"`
	Result  string `json:"result,omitempty"`
}

// LaunchResult is the normalized client output.
type LaunchResult struct {
	Raw      []byte
	Steps    []Step
	CostUSD  float64
	Duration time.Duration
	Usage    pricing.TokenUsage
	Errors   []string
}

// AgentsMarkdown wraps SKILL.md for the client. Both surfaces get the same
// skill body, so a cli/mcp score gap measures the surface, not skill presence.
func AgentsMarkdown(skill []byte, surface string) []byte {
	var b bytes.Buffer
	b.WriteString("# Trebi\n\n")
	if surface == surfaceMCP {
		b.WriteString("Operate this workspace only through the `trebi` MCP server tools. ")
		b.WriteString("The `trebi` binary is not on PATH. Do not edit `$TREBI_HOME` and do not call `/v1` with curl.\n\n")
		b.WriteString("The reference below is written for the `trebi` CLI. Each command maps to an MCP call: ")
		b.WriteString("`trebi <tool> <action> --flag v` becomes the `<tool>` tool with ")
		b.WriteString("`{\"action\":\"<action>\",\"input\":{\"flag\":\"v\"}}`. Tool responses are already JSON, so `--json` and `jq` do not apply. ")
		b.WriteString("Call a tool's `docs` action when you need its exact action and input names.\n\n")
	} else {
		b.WriteString("Operate this workspace only through the `trebi` CLI. ")
		b.WriteString("Do not edit `$TREBI_HOME` and do not call `/v1` with curl.\n\n")
	}
	b.Write(skill)
	return b.Bytes()
}

func parseClientSpec(spec string) (name, model string, err error) {
	name, model, _ = strings.Cut(spec, ":")
	switch name {
	case clientClaude, clientCodex:
		return name, model, nil
	default:
		return "", "", fmt.Errorf("unknown client %q", spec)
	}
}

func clientLabel(name, model string) string {
	if model == "" {
		return name
	}
	return name + ":" + model
}

func newClient(spec string) (Client, error) {
	name, model, err := parseClientSpec(spec)
	if err != nil {
		return nil, err
	}
	if name == clientCodex {
		return &codexClient{model: model}, nil
	}
	return &claudeClient{model: model}, nil
}

func writeTranscript(path string, raw []byte, steps []Step) error {
	if path == "" {
		return nil
	}
	if err := os.WriteFile(path+".raw", raw, 0o644); err != nil {
		return err
	}
	if steps == nil {
		steps = []Step{}
	}
	data, err := json.MarshalIndent(steps, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func hasAssistantStep(steps []Step) bool {
	for _, s := range steps {
		if s.Role == "assistant" && strings.TrimSpace(s.Content) != "" {
			return true
		}
	}
	return false
}

func clientErrorSummary(res *LaunchResult) string {
	if res == nil {
		return "no launch result"
	}
	if len(res.Errors) > 0 {
		return strings.Join(res.Errors, "; ")
	}
	if n := toolStepCount(res.Steps); n > 0 {
		return fmt.Sprintf("no final answer after %d tool calls (timeout)", n)
	}
	return "zero assistant steps"
}

// toolStepCount counts tool calls the client actually made. A client that made
// some is alive; it ran out of time before its final message.
func toolStepCount(steps []Step) int {
	n := 0
	for _, s := range steps {
		if s.Role == "tool" {
			n++
		}
	}
	return n
}

func mcpHandshakeOK(steps []Step) bool {
	for _, s := range steps {
		if s.Role != "tool" {
			continue
		}
		if strings.HasPrefix(s.Name, "mcp__trebi") || isTrebiMCPTool(s.Name) {
			return true
		}
	}
	return false
}

func appendUnique(dst []string, msg string) []string {
	if msg == "" {
		return dst
	}
	for _, e := range dst {
		if e == msg {
			return dst
		}
	}
	return append(dst, msg)
}

func isTrebiMCPTool(name string) bool {
	switch name {
	case "assistant", "config", "conversation", "events", "file", "harness",
		"inbox", "integration", "job", "queue", "run", "secret", "skill",
		"system", "workspace":
		return true
	default:
		return false
	}
}

func mcpIsolationViolations(steps []Step, apiURL string) []string {
	host := trialHost(apiURL)
	var out []string
	for _, s := range steps {
		if s.Role != "tool" {
			continue
		}
		if looksLikeTrebiShell(s) {
			out = append(out, "trebi shell invocation in "+s.Name)
		}
		if host != "" && looksLikeCurlTo(s.Content, host) {
			out = append(out, "curl to trial port in "+s.Name)
		}
		if leak := mcpServerLeak(s.Name); leak != "" {
			out = append(out, leak)
		}
	}
	return out
}

func looksLikeTrebiShell(s Step) bool {
	if isTrebiMCPTool(s.Name) || strings.HasPrefix(s.Name, "mcp__trebi") {
		return false
	}
	cmd := jsonCommandField(s.Content)
	if cmd == "" {
		return false
	}
	return commandInvokes(cmd, "trebi")
}

func looksLikeCurlTo(content, host string) bool {
	cmd := jsonCommandField(content)
	if cmd == "" {
		return false
	}
	return commandInvokes(cmd, "curl") && strings.Contains(cmd, host)
}

func jsonCommandField(content string) string {
	var body struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(content), &body) != nil {
		return ""
	}
	return body.Command
}

func commandInvokes(cmd, name string) bool {
	return scriptInvokes(cmd, name, 2)
}

func scriptInvokes(cmd, name string, depth int) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	if depth > 0 {
		if inner, ok := peelShellC(cmd); ok {
			return scriptInvokes(inner, name, depth-1)
		}
	}
	segs := splitShellSegments(cmd)
	if len(segs) > 1 {
		for _, seg := range segs {
			if scriptInvokes(seg, name, depth) {
				return true
			}
		}
		return false
	}
	return segmentInvokes(cmd, name)
}

func peelShellC(cmd string) (string, bool) {
	toks := shellTokens(cmd)
	if len(toks) < 3 || !isShellBin(toks[0]) {
		return cmd, false
	}
	for i := 1; i < len(toks)-1; i++ {
		if isShortFlagC(toks[i]) {
			return toks[i+1], true
		}
	}
	return cmd, false
}

func isShellBin(t string) bool {
	switch cmdBase(t) {
	case "bash", "sh", "zsh", "dash", "ksh":
		return true
	default:
		return false
	}
}

func isShortFlagC(t string) bool {
	return strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "--") && strings.Contains(t, "c")
}

func splitShellSegments(s string) []string {
	var segs []string
	var b strings.Builder
	quote := byte(0)
	flush := func() {
		if t := strings.TrimSpace(b.String()); t != "" {
			segs = append(segs, t)
		}
		b.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote == '\'' {
			b.WriteByte(c)
			if c == '\'' {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			if c == '\\' && i+1 < len(s) {
				b.WriteByte(c)
				b.WriteByte(s[i+1])
				i++
				continue
			}
			b.WriteByte(c)
			if c == '"' {
				quote = 0
			}
			continue
		}
		if c == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i++
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			b.WriteByte(c)
			continue
		}
		if n := shellSepLen(s, i); n > 0 {
			flush()
			i += n - 1
			continue
		}
		b.WriteByte(c)
	}
	flush()
	return segs
}

func shellSepLen(s string, i int) int {
	switch s[i] {
	case '`', ';', '\n':
		return 1
	case '|':
		if i+1 < len(s) && s[i+1] == '|' {
			return 2
		}
		return 1
	case '&':
		if i+1 < len(s) && s[i+1] == '&' {
			return 2
		}
	case '$':
		if i+1 < len(s) && s[i+1] == '(' {
			return 2
		}
	}
	return 0
}

func segmentInvokes(seg, name string) bool {
	toks := shellTokens(seg)
	i := 0
	for i < len(toks) {
		t := toks[i]
		if isEnvAssign(t) || isCmdWrapper(t) || strings.HasPrefix(t, "-") {
			i++
			continue
		}
		break
	}
	if i >= len(toks) {
		return false
	}
	t := toks[i]
	return t == name || strings.HasSuffix(t, "/"+name)
}

func isCmdWrapper(t string) bool {
	switch cmdBase(t) {
	case "env", "command", "nohup", "time", "xargs":
		return true
	default:
		return false
	}
}

func isEnvAssign(t string) bool {
	eq := strings.IndexByte(t, '=')
	if eq <= 0 || t[0] == '-' {
		return false
	}
	for _, c := range t[:eq] {
		if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func cmdBase(t string) string {
	if i := strings.LastIndex(t, "/"); i >= 0 {
		return t[i+1:]
	}
	return t
}

func shellTokens(s string) []string {
	var toks []string
	var b strings.Builder
	quote := byte(0)
	flush := func() {
		if b.Len() > 0 {
			toks = append(toks, b.String())
			b.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote == '\'' {
			if c == '\'' {
				quote = 0
				continue
			}
			b.WriteByte(c)
			continue
		}
		if quote == '"' {
			if c == '\\' && i+1 < len(s) {
				b.WriteByte(s[i+1])
				i++
				continue
			}
			if c == '"' {
				quote = 0
				continue
			}
			b.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case ' ', '\t':
			flush()
		case '\\':
			if i+1 < len(s) {
				b.WriteByte(s[i+1])
				i++
			}
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return toks
}

func mcpServerLeak(name string) string {
	if !strings.HasPrefix(name, "mcp__") {
		return ""
	}
	if strings.HasPrefix(name, "mcp__trebi") {
		return ""
	}
	return "unexpected MCP tool " + name
}

func trialHost(apiURL string) string {
	u, err := url.Parse(apiURL)
	if err != nil {
		return ""
	}
	return u.Host
}

func usageCost(model string, in, out, cacheRead, cacheCreate int64) (pricing.TokenUsage, float64) {
	u := pricing.TokenUsage{
		Input: in, Output: out, CacheRead: cacheRead, CacheCreation: cacheCreate, Model: model,
	}
	pricing.Apply(&u)
	return u, u.CostUSD
}

func jsonNumber(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case json.Number:
			n, _ := v.Int64()
			return n
		case int:
			return int64(v)
		case int64:
			return v
		}
	}
	return 0
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func ndjsonMaps(raw []byte) []map[string]any {
	var out []map[string]any
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// maxStepResult caps one recorded tool result. A docs action answers with
// several KB; the whole transcript must still fit the judge prompt.
const maxStepResult = 4000

// stepResult renders a tool response for the transcript. It flattens the MCP
// content envelope and keeps the head, where an error or a status field sits.
func stepResult(v any) string {
	s := contentText(v)
	if s == "" {
		if m := asMap(v); m != nil {
			s = contentText(m["content"])
		}
	}
	s = strings.TrimSpace(s)
	if len(s) > maxStepResult {
		s = s[:maxStepResult] + "\n…truncated"
	}
	return s
}

func contentText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, item := range t {
			m := asMap(item)
			if m == nil {
				continue
			}
			if s := asString(m["text"]); s != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return ""
	}
}
