// Package llm is the native-assistant provider adapters. It does not import daemon.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/trebi-ai/trebi/internal/config"
)

const (
	StopEndTurn       = "end_turn"
	StopToolUse       = "tool_use"
	StopMaxTokens     = "max_tokens"
	StopRefusal       = "refusal"
	StopContentFilter = "content_filter"
	StopUnknown       = "unknown"
)

// ErrUnauthorized is a 401 from the provider.
var ErrUnauthorized = errors.New("unauthorized")

// Provider is one Chat Completions / Messages backend.
type Provider interface {
	Chat(ctx context.Context, req Request) (*Response, error)
}

// Request is the provider-neutral chat payload.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
	Headers   map[string]string
}

// Message is one turn. Role is user, assistant, or tool.
type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	Name       string
}

// Tool is a function the model may call.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON schema object
}

// ToolCall is one model-requested function invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Usage feeds pricing.
type Usage struct {
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
}

// Response is a complete (non-streaming) model turn.
type Response struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
	Stop      string
	RawStop   string
}

// New picks an adapter from api_format. Anything other than anthropic uses OpenAI.
func New(cfg config.NativeConfig) (Provider, error) {
	return NewWithHTTP(cfg, nil)
}

// NewWithHTTP is New with an optional client (tests).
func NewWithHTTP(cfg config.NativeConfig, hc *http.Client) (Provider, error) {
	key, _, err := cfg.ResolveKey()
	if err != nil {
		return nil, err
	}
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = cfg.DefaultBaseURL()
	}
	format := cfg.Format()
	base = NormalizeBaseURL(base, format)
	if format == "anthropic" {
		return newAnthropic(key, base, cfg.Headers, cfg.WebSearchEnabled(), hc)
	}
	return newOpenAI(key, base, cfg.Headers, hc)
}

// NormalizeBaseURL applies the v1 rule: pass through a URL that already ends
// in /v1; append /v1 only when absent. Anthropic's SDK adds /v1/messages, so
// that format is left unchanged.
func NormalizeBaseURL(raw, format string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return ""
	}
	if format == "anthropic" {
		return raw
	}
	if strings.HasSuffix(raw, "/v1") {
		return raw
	}
	return raw + "/v1"
}

func mapStop(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "stop", "end_turn", "stop_sequence":
		return StopEndTurn
	case "tool_calls", "tool_use", "function_call":
		return StopToolUse
	case "length", "max_tokens":
		return StopMaxTokens
	case "refusal":
		return StopRefusal
	case "content_filter":
		return StopContentFilter
	default:
		return StopUnknown
	}
}

func wrapHTTPError(err error, status int, msg string) error {
	if status == http.StatusUnauthorized {
		if msg == "" {
			return ErrUnauthorized
		}
		return fmt.Errorf("%w: %s", ErrUnauthorized, msg)
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("provider http %d: %s", status, msg)
}

func schemaMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return m
}
