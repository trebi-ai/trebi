package decisions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/pricing"
)

// Harness asks a harness CLI in print mode, with no tools, in a temp dir.
type Harness struct {
	name  string
	bin   string
	model string
	env   map[string]string // the isolation env of the harness
}

// NewHarness builds the decider. bin is the resolved binary of name.
func NewHarness(name, bin, model string, env map[string]string) *Harness {
	return &Harness{name: name, bin: bin, model: model, env: env}
}

func (h *Harness) Name() string { return "harness:" + h.name }

func (h *Harness) Decide(ctx context.Context, r Request) (Result, error) {
	dir, err := os.MkdirTemp("", "trebi-decide-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	last := filepath.Join(dir, "reply.txt")
	args, err := harness.PrintArgs(h.name, h.model, last)
	if err != nil {
		return Result{}, err
	}
	p := BuildPrompt(r, "")
	cmd := exec.CommandContext(ctx, h.bin, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(p.System + "\n\n" + p.User)
	cmd.Env = os.Environ()
	for k, v := range h.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{Raw: stdout.Bytes()}, fmt.Errorf("%s: %w: %s", h.name, err, strings.TrimSpace(stderr.String()))
	}
	// codex exec prints no token counts in this mode, so its usage stays 0.
	res := Result{Raw: stdout.Bytes()}
	if h.name == "codex" {
		if b, err := os.ReadFile(last); err == nil {
			res.Raw = b
		}
	} else {
		res = claudeEnvelope(res.Raw)
	}
	answers, err := ParseAnswers(res.Raw)
	if err != nil {
		return res, checkErr{err}
	}
	res.Answers, res.Method = answers, MethodVerbal
	return res, nil
}

// claudeEnvelope reads the reply text, cost, and token usage from `claude -p
// --output-format json`. Another body passes through.
func claudeEnvelope(raw []byte) Result {
	var env struct {
		Result string  `json:"result"`
		Cost   float64 `json:"total_cost_usd"`
		Usage  struct {
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &env); err != nil || env.Result == "" {
		return Result{Raw: raw}
	}
	u := env.Usage
	return Result{Raw: []byte(env.Result), CostUSD: env.Cost,
		Tokens: pricing.TokenUsage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheCreation: u.CacheCreation}}
}
