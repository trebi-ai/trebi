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
	if opts.Surface != surfaceMCP {
		if err := os.WriteFile(filepath.Join(opts.Workdir, "AGENTS.md"), AgentsMarkdown(opts.SkillMD), 0o644); err != nil {
			return nil, err
		}
	} else if err := writeCodexMCPConfig(opts.ConfigHome, opts.MCPURL, opts.MCPToken); err != nil {
		return nil, err
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

func parseCodexJSON(raw []byte) *LaunchResult {
	res := &LaunchResult{Raw: raw, Steps: []Step{}}
	var in, out, cacheR, cacheC int64
	model := "codex"
	byID := map[string]int{}
	for _, m := range ndjsonMaps(raw) {
		parseCodexEvent(m, res, &in, &out, &cacheR, &cacheC, &model, byID)
	}
	if len(res.Steps) == 0 && len(res.Errors) == 0 && len(raw) > 0 {
		var one map[string]any
		if json.Unmarshal(raw, &one) == nil {
			parseCodexEvent(one, res, &in, &out, &cacheR, &cacheC, &model, byID)
		}
	}
	res.Usage, res.CostUSD = usageCost(model, in, out, cacheR, cacheC)
	return res
}

func parseCodexEvent(m map[string]any, res *LaunchResult, in, out, cacheR, cacheC *int64, model *string, byID map[string]int) {
	typ := asString(m["type"])
	if typ == "" {
		typ = asString(m["event"])
	}
	item := asMap(m["item"])
	collectCodexError(typ, m, item, res)
	switch typ {
	case "item.started", "item.completed", "agent_message", "message", "tool", "command":
		parseCodexItem(typ, m, item, res, byID)
	case "turn.completed", "result", "usage":
		usage := asMap(m["usage"])
		if usage == nil {
			usage = m
		}
		*in += jsonNumber(usage, "input_tokens", "input")
		*out += jsonNumber(usage, "output_tokens", "output")
		*cacheR += jsonNumber(usage, "cached_input_tokens", "cache_read_input_tokens", "cache_read")
		*cacheC += jsonNumber(usage, "cache_write_input_tokens", "cache_creation_input_tokens", "cache_creation")
		if mname := asString(m["model"]); mname != "" {
			*model = mname
		}
	}
}

func collectCodexError(typ string, m, item map[string]any, res *LaunchResult) {
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

func parseCodexItem(typ string, m, item map[string]any, res *LaunchResult, byID map[string]int) {
	if item == nil {
		item = m
	}
	if asString(item["type"]) == "mcp_tool_call" {
		parseCodexMCP(item, res, byID)
		return
	}
	text := firstNonEmpty(asString(item["text"]), contentText(item["content"]), asString(m["text"]), contentText(m["content"]), asString(m["message"]))
	if asString(item["type"]) == "agent_message" || typ == "agent_message" || typ == "message" {
		if text != "" {
			res.Steps = append(res.Steps, Step{Role: "assistant", Content: text})
		}
		return
	}
	body, ok := codexCommandBody(typ, m, item)
	if !ok {
		return
	}
	raw, _ := json.Marshal(body)
	upsertCodexStep(res, byID, asString(item["id"]), Step{Role: "tool", Name: "command", Content: string(raw)})
}

func parseCodexMCP(item map[string]any, res *LaunchResult, byID map[string]int) {
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
	upsertCodexStep(res, byID, asString(item["id"]), Step{
		Role: "tool", Name: "mcp__" + server + "__" + tool, Content: string(raw),
	})
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

func upsertCodexStep(res *LaunchResult, byID map[string]int, id string, step Step) {
	if id != "" {
		if i, ok := byID[id]; ok {
			res.Steps[i] = step
			return
		}
		byID[id] = len(res.Steps)
	}
	res.Steps = append(res.Steps, step)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
