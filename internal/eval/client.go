package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
}

// Step is one normalized transcript turn.
type Step struct {
	Role    string `json:"role"`
	Name    string `json:"name,omitempty"`
	Content string `json:"content,omitempty"`
}

// LaunchResult is the normalized client output.
type LaunchResult struct {
	Raw      []byte
	Steps    []Step
	CostUSD  float64
	Duration time.Duration
	Usage    pricing.TokenUsage
}

// AgentsMarkdown wraps SKILL.md so Codex reads it as AGENTS.md.
func AgentsMarkdown(skill []byte) []byte {
	var b bytes.Buffer
	b.WriteString("# Trebi\n\n")
	b.WriteString("Operate this workspace only through the `trebi` CLI. ")
	b.WriteString("Do not edit `$TREBI_HOME` and do not call `/v1` with curl.\n\n")
	b.Write(skill)
	return b.Bytes()
}

func newClient(name string) (Client, error) {
	switch name {
	case clientClaude:
		return &claudeClient{}, nil
	case clientCodex:
		return &codexClient{}, nil
	default:
		return nil, fmt.Errorf("unknown client %q", name)
	}
}

func writeTranscript(path string, raw []byte, steps []Step) error {
	if path == "" {
		return nil
	}
	if err := os.WriteFile(path+".raw", raw, 0o644); err != nil {
		return err
	}
	data, err := json.MarshalIndent(steps, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
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
