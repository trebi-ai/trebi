package assistant

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/agent-wire/native"

	"github.com/trebi-ai/trebi/internal/config"
)

// probeTimeout bounds one provider test call.
const probeTimeout = 20 * time.Second

// ModelFactory builds one provider model. Tests inject a fake.
type ModelFactory func(agentwire.Credentials, string) (native.Model, error)

// NativeProbe runs one small model call to check native provider settings
// (plan 2026-09-26 B). A test saves nothing and never logs the key.
type NativeProbe struct {
	newModel ModelFactory
	timeout  time.Duration
}

// NewNativeProbe builds a probe over a model factory.
func NewNativeProbe(newModel ModelFactory) *NativeProbe {
	return &NativeProbe{newModel: newModel, timeout: probeTimeout}
}

// Run tests cfg with model. A non-empty key wins over cfg's resolution.
// The result carries the failure code and the verbatim error text; the key
// value never appears in any field.
func (p *NativeProbe) Run(ctx context.Context, cfg config.NativeConfig, model, key string) NativeTestResult {
	model = strings.TrimSpace(model)
	res := NativeTestResult{Model: model}
	if model == "" {
		res.Code = "no_model"
		res.Error = "no model is set"
		return res
	}
	src := "typed"
	if strings.TrimSpace(key) == "" {
		resolved, source, err := cfg.ResolveKey()
		if err != nil {
			res.KeySource = config.KeySourceNone
			res.Code = "no_key"
			res.Error = err.Error()
			return res
		}
		key, src = resolved, source
	}
	res.KeySource = src
	cred := agentwire.Credentials{
		Format:  cfg.Format(),
		BaseURL: cfg.BaseURL,
		APIKey:  key,
		Headers: cfg.Headers,
	}
	m, err := p.newModel(cred, model)
	if err != nil {
		res.Code = "provider"
		res.Error = err.Error()
		return res
	}
	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req := native.Request{
		System:          "Reply with the word ok.",
		Messages:        []native.Message{userPing()},
		MaxOutputTokens: 16,
	}
	start := time.Now()
	var text strings.Builder
	var finish *native.Finish
	for part, err := range m.Stream(callCtx, req) {
		if err != nil {
			res.LatencyMS = time.Since(start).Milliseconds()
			res.Code = probeErrCode(callCtx, err)
			res.Error = err.Error()
			return res
		}
		switch part.Kind {
		case native.StreamTextDelta:
			text.WriteString(part.Delta)
		case native.StreamFinish:
			finish = part.Finish
		}
	}
	res.LatencyMS = time.Since(start).Milliseconds()
	if finish == nil {
		res.Code = "provider"
		res.Error = "native: model stream ended without a finish part"
		return res
	}
	res.OK = true
	res.Reply = text.String()
	res.InputTokens = int(finish.Usage.Input)
	res.OutputTokens = int(finish.Usage.Output)
	return res
}

// probeErrCode maps a stream error onto a code. A passed deadline is a
// timeout; anything else is the provider's fault.
func probeErrCode(ctx context.Context, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return "timeout"
	}
	return "provider"
}

// userPing builds the one user message of a probe call.
func userPing() native.Message {
	return native.Message{
		Role:  native.RoleUser,
		Parts: []native.Part{{Text: &native.TextPart{Text: "ping"}}},
	}
}
