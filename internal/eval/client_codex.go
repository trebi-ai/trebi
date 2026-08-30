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

type codexClient struct{ model string }

func (c *codexClient) Name() string { return clientLabel(clientCodex, c.model) }

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
	cmd := exec.Command("codex", codexLaunchArgs(opts.Prompt, c.model)...)
	cmd.Dir = opts.Workdir
	cmd.Env = opts.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := runWithGroup(ctx, cmd)
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
	res := &LaunchResult{Raw: raw}
	var in, out, cacheR, cacheC int64
	model := "codex"
	byID := map[string]int{}
	for _, m := range ndjsonMaps(raw) {
		parseCodexEvent(m, res, &in, &out, &cacheR, &cacheC, &model, byID)
	}
	if len(res.Steps) == 0 && len(raw) > 0 {
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

func parseCodexItem(typ string, m, item map[string]any, res *LaunchResult, byID map[string]int) {
	if item == nil {
		item = m
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
