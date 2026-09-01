package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type claudeClient struct{ model string }

func (c *claudeClient) Name() string { return clientLabel(clientClaude, c.model) }

func (c *claudeClient) Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return nil, fmt.Errorf("claude not on PATH: %w", err)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	maxTurns := opts.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 40
	}
	// The scratch MCP home has no installed skill, so ship it as project CLAUDE.md.
	if err := os.WriteFile(filepath.Join(opts.Workdir, "CLAUDE.md"), AgentsMarkdown(opts.SkillMD, opts.Surface), 0o644); err != nil {
		return nil, err
	}
	prompt := "Use the trebi skill in CLAUDE.md. Drive trebi only through the `trebi` CLI.\n\n" + opts.Prompt
	if opts.Surface == surfaceMCP {
		prompt = "Trebi is available as the MCP server named trebi. Use its tools and the skill in CLAUDE.md. Do not shell out to trebi or curl the API.\n\n" + opts.Prompt
	}
	args := claudeLaunchArgs(prompt, maxTurns, c.model)
	if opts.Surface == surfaceMCP {
		cfg, err := writeClaudeMCPConfig(opts.ConfigHome, opts.MCPURL, opts.MCPToken)
		if err != nil {
			return nil, err
		}
		args = append(args, "--mcp-config", cfg)
	}
	cmd := exec.Command("claude", args...)
	cmd.Dir = opts.Workdir
	cmd.Env = opts.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := runWithGroup(ctx, cmd)
	raw := stdout.Bytes()
	res := parseClaudeStream(raw)
	res.Duration = time.Since(start)
	if werr := writeTranscript(opts.Transcript, raw, res.Steps); werr != nil {
		return res, werr
	}
	if err != nil && ctx.Err() != nil {
		return res, fmt.Errorf("claude timeout: %w\n%s", ctx.Err(), stderr.Bytes())
	}
	if err != nil && len(raw) == 0 {
		return res, fmt.Errorf("claude: %w\n%s", err, stderr.Bytes())
	}
	return res, nil
}

func writeClaudeMCPConfig(dir, mcpURL, token string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "mcp.json")
	doc := map[string]any{
		"mcpServers": map[string]any{
			"trebi": map[string]any{
				"type": "http",
				"url":  mcpURL,
				"headers": map[string]string{
					"Authorization": "Bearer " + token,
				},
			},
		},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func claudeLaunchArgs(prompt string, maxTurns int, model string) []string {
	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--max-turns", strconv.Itoa(maxTurns),
		"--dangerously-skip-permissions",
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

type claudeParser struct {
	res            *LaunchResult
	byID           map[string]int
	in, out        int64
	cacheR, cacheC int64
	model          string
}

func parseClaudeStream(raw []byte) *LaunchResult {
	p := &claudeParser{
		res:   &LaunchResult{Raw: raw, Steps: Steps{}},
		byID:  map[string]int{},
		model: "claude",
	}
	for _, m := range ndjsonMaps(raw) {
		p.event(m)
	}
	p.res.Usage, p.res.CostUSD = usageCost(p.model, p.in, p.out, p.cacheR, p.cacheC)
	return p.res
}

func (p *claudeParser) event(m map[string]any) {
	switch asString(m["type"]) {
	case "assistant":
		p.res.appendClaudeAssistant(m, p.byID)
	case "user":
		if p.res.attachClaudeResults(m, p.byID) {
			return
		}
		if s := contentText(asMap(m["message"])["content"]); s != "" {
			p.res.Steps = append(p.res.Steps, Step{Role: "user", Content: s})
		}
	case "result":
		p.res.collectClaudeResult(m)
		usage := asMap(m["usage"])
		if usage == nil {
			usage = asMap(m)
		}
		p.in += jsonNumber(usage, "input_tokens", "input")
		p.out += jsonNumber(usage, "output_tokens", "output")
		p.cacheR += jsonNumber(usage, "cache_read_input_tokens", "cache_read")
		p.cacheC += jsonNumber(usage, "cache_creation_input_tokens", "cache_creation")
		if mname := asString(m["model"]); mname != "" {
			p.model = mname
		}
	}
}

func (res *LaunchResult) collectClaudeResult(m map[string]any) {
	errish := m["is_error"] == true || asString(m["subtype"]) == "error"
	if !errish {
		return
	}
	msg := firstNonEmpty(asString(m["result"]), asString(m["error"]))
	res.Errors = appendUnique(res.Errors, msg)
}

// appendClaudeAssistant appends one assistant message and indexes each tool
// call by id, so the later tool_result can find the step it belongs to.
func (res *LaunchResult) appendClaudeAssistant(m map[string]any, byID map[string]int) {
	msg := asMap(m["message"])
	content, _ := msg["content"].([]any)
	for _, item := range content {
		cm := asMap(item)
		switch asString(cm["type"]) {
		case "text":
			if s := asString(cm["text"]); s != "" {
				res.Steps = append(res.Steps, Step{Role: "assistant", Content: s})
			}
		case "tool_use":
			body, _ := json.Marshal(cm["input"])
			if id := asString(cm["id"]); id != "" {
				byID[id] = len(res.Steps)
			}
			res.Steps = append(res.Steps, Step{
				Role:    "tool",
				Name:    asString(cm["name"]),
				Content: string(body),
			})
		}
	}
}

// attachClaudeResults writes each tool_result onto the step that called it.
// It reports whether the message was only tool results, so the caller does not
// also record it as user prose.
func (res *LaunchResult) attachClaudeResults(m map[string]any, byID map[string]int) bool {
	content, _ := asMap(m["message"])["content"].([]any)
	if len(content) == 0 {
		return false
	}
	all := true
	for _, item := range content {
		cm := asMap(item)
		if asString(cm["type"]) != "tool_result" {
			all = false
			continue
		}
		i, ok := byID[asString(cm["tool_use_id"])]
		if !ok || i >= len(res.Steps) {
			continue
		}
		out := stepResult(cm["content"])
		if cm["is_error"] == true {
			out = strings.TrimSpace("error: " + out)
		}
		res.Steps[i].Result = out
	}
	return all
}
