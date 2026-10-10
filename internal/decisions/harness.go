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
	reply, cost := stdout.Bytes(), 0.0
	if h.name == "codex" {
		if b, err := os.ReadFile(last); err == nil {
			reply = b
		}
	} else {
		reply, cost = claudeEnvelope(reply)
	}
	answers, err := ParseAnswers(reply)
	if err != nil {
		return Result{Raw: reply, CostUSD: cost}, checkErr{err}
	}
	return Result{Answers: answers, Method: MethodVerbal, Raw: reply, CostUSD: cost}, nil
}

// claudeEnvelope reads the reply text and cost from `claude -p
// --output-format json`. Another body passes through.
func claudeEnvelope(raw []byte) ([]byte, float64) {
	var env struct {
		Result string  `json:"result"`
		Cost   float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &env); err != nil || env.Result == "" {
		return raw, 0
	}
	return []byte(env.Result), env.Cost
}
