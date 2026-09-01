package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type codexClient struct{ model string }

func (c *codexClient) Name() string { return clientLabel(clientCodex, c.model) }

func (c *codexClient) Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error) {
	if _, err := exec.LookPath("codex"); err != nil {
		return nil, fmt.Errorf("codex not on PATH: %w", err)
	}
	if err := os.WriteFile(filepath.Join(opts.Workdir, "AGENTS.md"), AgentsMarkdown(opts.SkillMD, opts.Surface), 0o644); err != nil {
		return nil, err
	}
	if opts.Surface == surfaceMCP {
		if err := writeCodexMCPConfig(opts.ConfigHome, opts.MCPURL, opts.MCPToken); err != nil {
			return nil, err
		}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command("codex", codexLaunchArgs(opts.Prompt, c.model)...)
	cmd.Dir = opts.Workdir
	cmd.Env = opts.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := runWithGroup(ctx, cmd)
	if werr := writeBackCodexAuth(opts.ConfigHome); werr != nil {
		fmt.Fprintf(os.Stderr, "eval: skip auth write-back: %v\n", werr)
	}
	raw := stdout.Bytes()
	res := parseCodexJSON(raw)
	res.Duration = time.Since(start)
	if werr := writeTranscript(opts.Transcript, raw, res.Steps); werr != nil {
		return res, werr
	}
	if err != nil && ctx.Err() != nil {
		return res, fmt.Errorf("codex timeout: %w\n%s", ctx.Err(), stderr.Bytes())
	}
	if err != nil && len(raw) == 0 {
		return res, fmt.Errorf("codex: %w\n%s", err, stderr.Bytes())
	}
	return res, nil
}

func writeCodexMCPConfig(dir, mcpURL, token string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mcp := fmt.Sprintf("[mcp_servers.trebi]\nurl = %q\nhttp_headers = { Authorization = %q }\n",
		mcpURL, "Bearer "+token)
	body := mcp
	if carried := carryCodexModelConfig(); carried != "" {
		body = carried + "\n\n" + mcp
	}
	return os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600)
}

func carryCodexModelConfig() string {
	home := realCodexHome()
	if home == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return ""
	}
	userHome, _ := os.UserHomeDir()
	return extractCodexModelConfig(data, userHome)
}

func extractCodexModelConfig(src []byte, userHome string) string {
	var root, tables []string
	var header string
	var block []string
	flush := func() {
		if header == "" {
			return
		}
		if header == "model_providers" || strings.HasPrefix(header, "model_providers.") {
			tables = append(tables, strings.TrimRight(strings.Join(block, "\n"), "\n"))
		}
		header = ""
		block = nil
	}
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if h, ok := tomlTableHeader(line); ok {
			flush()
			header = h
			block = []string{line}
			continue
		}
		if header != "" {
			block = append(block, line)
			continue
		}
		key := tomlBareKey(line)
		if !isCodexModelKey(key) {
			continue
		}
		if key == "model_catalog_json" {
			line = rewriteCatalogTilde(line, userHome)
		}
		root = append(root, line)
	}
	flush()
	var parts []string
	if len(root) > 0 {
		parts = append(parts, strings.Join(root, "\n"))
	}
	parts = append(parts, tables...)
	return strings.Join(parts, "\n\n")
}

func isCodexModelKey(k string) bool {
	switch k {
	case "model", "model_provider", "model_reasoning_effort", "model_catalog_json":
		return true
	default:
		return false
	}
}

func tomlTableHeader(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if len(s) < 3 || s[0] != '[' {
		return "", false
	}
	i := 1
	if s[1] == '[' {
		i = 2
	}
	end := strings.IndexByte(s[i:], ']')
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(s[i : i+end]), true
}

func tomlBareKey(line string) string {
	s := strings.TrimSpace(line)
	if s == "" || s[0] == '#' || s[0] == '[' {
		return ""
	}
	key, _, ok := strings.Cut(s, "=")
	if !ok {
		return ""
	}
	return strings.Trim(strings.TrimSpace(key), `"'`)
}

func rewriteCatalogTilde(line, userHome string) string {
	if userHome == "" {
		return line
	}
	key, rest, ok := strings.Cut(line, "=")
	if !ok {
		return line
	}
	val := strings.TrimSpace(rest)
	quote := ""
	unquoted := val
	if len(val) > 0 && (val[0] == '"' || val[0] == '\'') {
		q := val[0]
		if i := strings.IndexByte(val[1:], q); i >= 0 {
			quote = string(q)
			unquoted = val[1 : 1+i]
		}
	}
	if unquoted != "~" && !strings.HasPrefix(unquoted, "~/") {
		return line
	}
	expanded := userHome
	if unquoted != "~" {
		expanded = filepath.Join(userHome, unquoted[2:])
	}
	if quote == "" {
		quote = `"`
	}
	return strings.TrimRight(key, " \t") + " = " + quote + expanded + quote
}

func codexLaunchArgs(prompt, model string) []string {
	args := []string{
		"exec", "--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	return append(args, prompt)
}

type codexParser struct {
	res            *LaunchResult
	byID           map[string]int
	in, out        int64
	cacheR, cacheC int64
	model          string
}

func parseCodexJSON(raw []byte) *LaunchResult {
	p := &codexParser{
		res:   &LaunchResult{Raw: raw, Steps: Steps{}},
		byID:  map[string]int{},
		model: "codex",
	}
	for _, m := range ndjsonMaps(raw) {
		p.event(m)
	}
	if len(p.res.Steps) == 0 && len(p.res.Errors) == 0 && len(raw) > 0 {
		var one map[string]any
		if json.Unmarshal(raw, &one) == nil {
			p.event(one)
		}
	}
	p.res.Usage, p.res.CostUSD = usageCost(p.model, p.in, p.out, p.cacheR, p.cacheC)
	return p.res
}

func (p *codexParser) event(m map[string]any) {
	typ := asString(m["type"])
	if typ == "" {
		typ = asString(m["event"])
	}
	item := asMap(m["item"])
	p.res.collectCodexError(typ, m, item)
	switch typ {
	case "item.started", "item.completed", "agent_message", "message", "tool", "command":
		p.item(typ, m, item)
	case "turn.completed", "result", "usage":
		usage := asMap(m["usage"])
		if usage == nil {
			usage = m
		}
		p.in += jsonNumber(usage, "input_tokens", "input")
		p.out += jsonNumber(usage, "output_tokens", "output")
		p.cacheR += jsonNumber(usage, "cached_input_tokens", "cache_read_input_tokens", "cache_read")
		p.cacheC += jsonNumber(usage, "cache_write_input_tokens", "cache_creation_input_tokens", "cache_creation")
		if mname := asString(m["model"]); mname != "" {
			p.model = mname
		}
	}
}

func (p *codexParser) item(typ string, m, item map[string]any) {
	if item == nil {
		item = m
	}
	if asString(item["type"]) == "mcp_tool_call" {
		p.mcp(item)
		return
	}
	if asString(item["type"]) == "file_change" {
		p.fileChange(item)
		return
	}
	text := firstNonEmpty(asString(item["text"]), contentText(item["content"]), asString(m["text"]), contentText(m["content"]), asString(m["message"]))
	if asString(item["type"]) == "agent_message" || typ == "agent_message" || typ == "message" {
		if text != "" {
			p.res.Steps = append(p.res.Steps, Step{Role: "assistant", Content: text})
		}
		return
	}
	body, ok := codexCommandBody(typ, m, item)
	if !ok {
		return
	}
	raw, _ := json.Marshal(body)
	p.upsert(asString(item["id"]), Step{Role: "tool", Name: "command", Content: string(raw)})
}

// fileChange records a write the client made with its own file tool, not
// through trebi. The judge needs it: a task can be finished this way after
// a trebi call fails, and a write into the trebi home is itself a violation.
func (p *codexParser) fileChange(item map[string]any) {
	changes, _ := item["changes"].([]any)
	paths := make([]string, 0, len(changes))
	kinds := make([]string, 0, len(changes))
	for _, c := range changes {
		cm := asMap(c)
		if path := asString(cm["path"]); path != "" {
			paths = append(paths, path)
			kinds = append(kinds, asString(cm["kind"]))
		}
	}
	if len(paths) == 0 {
		return
	}
	body, _ := json.Marshal(map[string]any{"paths": paths, "kinds": kinds})
	p.upsert(asString(item["id"]), Step{
		Role: "tool", Name: "file_change", Content: string(body),
		Result: asString(item["status"]),
	})
}

func (p *codexParser) mcp(item map[string]any) {
	server := asString(item["server"])
	tool := asString(item["tool"])
	if server == "" || tool == "" {
		return
	}
	args := item["arguments"]
	if args == nil {
		args = map[string]any{}
	}
	raw, _ := json.Marshal(args)
	out := stepResult(item["result"])
	if e := strings.TrimSpace(asString(item["error"])); e != "" {
		out = strings.TrimSpace("error: " + e + "\n" + out)
	}
	p.upsert(asString(item["id"]), Step{
		Role: "tool", Name: "mcp__" + server + "__" + tool, Content: string(raw), Result: out,
	})
}

func (p *codexParser) upsert(id string, step Step) {
	p.res.upsert(p.byID, id, step)
}

func (res *LaunchResult) collectCodexError(typ string, m, item map[string]any) {
	if typ != "error" && typ != "turn.failed" && asString(item["type"]) != "error" {
		return
	}
	msg := firstNonEmpty(asString(m["message"]), asString(asMap(m["error"])["message"]), asString(item["message"]))
	res.Errors = appendUnique(res.Errors, stripCodexNoise(msg))
}

func stripCodexNoise(msg string) string {
	msg = strings.TrimSpace(msg)
	if strings.HasPrefix(msg, "Reconnecting") {
		if i := strings.Index(msg, "("); i >= 0 && strings.HasSuffix(msg, ")") {
			msg = msg[i+1 : len(msg)-1]
		}
	}
	for _, p := range []string{", cf-ray:", ", request id:"} {
		if i := strings.Index(msg, p); i >= 0 {
			msg = msg[:i]
		}
	}
	return strings.TrimSpace(msg)
}

func codexCommandBody(typ string, m, item map[string]any) (map[string]any, bool) {
	cmd := firstNonEmpty(asString(item["command"]), asString(m["command"]))
	out := asString(item["aggregated_output"])
	if cmd == "" && out == "" {
		return nil, false
	}
	body := map[string]any{}
	if cmd != "" {
		body["command"] = cmd
	}
	completed := typ == "item.completed" || asString(item["status"]) == "completed"
	if completed && item["exit_code"] != nil {
		body["exit_code"] = item["exit_code"]
	}
	if completed && out != "" {
		body["output"] = out
	}
	return body, true
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
