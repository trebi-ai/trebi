package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

type claudeClient struct{}

func (c *claudeClient) Name() string { return clientClaude }

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
	prompt := "Use the trebi skill. Drive trebi only through the `trebi` CLI.\n\n" + opts.Prompt
	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--max-turns", strconv.Itoa(maxTurns),
		"--dangerously-skip-permissions",
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = opts.Workdir
	cmd.Env = opts.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
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

func parseClaudeStream(raw []byte) *LaunchResult {
	res := &LaunchResult{Raw: raw}
	var in, out, cacheR, cacheC int64
	model := "claude"
	for _, m := range ndjsonMaps(raw) {
		switch asString(m["type"]) {
		case "assistant":
			res.Steps = append(res.Steps, claudeAssistantSteps(m)...)
		case "user":
			if s := contentText(asMap(m["message"])["content"]); s != "" {
				res.Steps = append(res.Steps, Step{Role: "user", Content: s})
			}
		case "result":
			usage := asMap(m["usage"])
			if usage == nil {
				usage = asMap(m)
			}
			in += jsonNumber(usage, "input_tokens", "input")
			out += jsonNumber(usage, "output_tokens", "output")
			cacheR += jsonNumber(usage, "cache_read_input_tokens", "cache_read")
			cacheC += jsonNumber(usage, "cache_creation_input_tokens", "cache_creation")
			if mname := asString(m["model"]); mname != "" {
				model = mname
			}
		}
	}
	res.Usage, res.CostUSD = usageCost(model, in, out, cacheR, cacheC)
	return res
}

func claudeAssistantSteps(m map[string]any) []Step {
	msg := asMap(m["message"])
	content, _ := msg["content"].([]any)
	var steps []Step
	for _, item := range content {
		cm := asMap(item)
		switch asString(cm["type"]) {
		case "text":
			if s := asString(cm["text"]); s != "" {
				steps = append(steps, Step{Role: "assistant", Content: s})
			}
		case "tool_use":
			body, _ := json.Marshal(cm["input"])
			steps = append(steps, Step{
				Role:    "tool",
				Name:    asString(cm["name"]),
				Content: string(body),
			})
		}
	}
	return steps
}
