package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type codexClient struct{}

func (c *codexClient) Name() string { return clientCodex }

func (c *codexClient) Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error) {
	if _, err := exec.LookPath("codex"); err != nil {
		return nil, fmt.Errorf("codex not on PATH: %w", err)
	}
	if err := os.WriteFile(filepath.Join(opts.Workdir, "AGENTS.md"), AgentsMarkdown(opts.SkillMD), 0o644); err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{
		"exec", "--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
		opts.Prompt,
	}
	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Dir = opts.Workdir
	cmd.Env = opts.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
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

func parseCodexJSON(raw []byte) *LaunchResult {
	res := &LaunchResult{Raw: raw}
	var in, out int64
	model := "codex"
	for _, m := range ndjsonMaps(raw) {
		parseCodexEvent(m, res, &in, &out, &model)
	}
	if len(res.Steps) == 0 && len(raw) > 0 {
		var one map[string]any
		if json.Unmarshal(raw, &one) == nil {
			parseCodexEvent(one, res, &in, &out, &model)
		}
	}
	res.Usage, res.CostUSD = usageCost(model, in, out, 0, 0)
	return res
}

func parseCodexEvent(m map[string]any, res *LaunchResult, in, out *int64, model *string) {
	typ := asString(m["type"])
	if typ == "" {
		typ = asString(m["event"])
	}
	switch typ {
	case "item.completed", "agent_message", "message":
		if s := firstNonEmpty(asString(m["text"]), contentText(m["content"]), asString(m["message"])); s != "" {
			res.Steps = append(res.Steps, Step{Role: "assistant", Content: s})
		}
	case "item.started", "tool", "command":
		name := firstNonEmpty(asString(m["name"]), asString(m["tool"]), "tool")
		body, _ := json.Marshal(m)
		res.Steps = append(res.Steps, Step{Role: "tool", Name: name, Content: string(body)})
	case "turn.completed", "result", "usage":
		usage := asMap(m["usage"])
		if usage == nil {
			usage = m
		}
		*in += jsonNumber(usage, "input_tokens", "input")
		*out += jsonNumber(usage, "output_tokens", "output")
		if mname := asString(m["model"]); mname != "" {
			*model = mname
		}
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
