package assistant

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/agent-wire/native"

	"github.com/trebi-ai/trebi/internal/config"
)

// fakeModel is a ModelFactory product: it records the credentials and
// streams scripted parts, an error, or nothing until the deadline.
type fakeModel struct {
	creds agentwire.Credentials
	model string
	err   error
	delay time.Duration
	parts []native.StreamPart
}

func (f *fakeModel) Name() string { return f.model }

func (f *fakeModel) Stream(ctx context.Context, _ native.Request) native.Seq[native.StreamPart] {
	return func(yield func(native.StreamPart, error) bool) {
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-ctx.Done():
				yield(native.StreamPart{}, ctx.Err())
				return
			}
		}
		if f.err != nil {
			yield(native.StreamPart{}, f.err)
			return
		}
		for _, p := range f.parts {
			if !yield(p, nil) {
				return
			}
		}
	}
}

// okParts is one full answer: a text delta, then a finish with usage.
func okParts() []native.StreamPart {
	return []native.StreamPart{
		{Kind: native.StreamTextDelta, Delta: "ok"},
		{Kind: native.StreamFinish, Finish: &native.Finish{
			Reason: native.FinishStop,
			Usage:  native.Usage{Input: 14, Output: 2},
		}},
	}
}

func TestNativeProbeSuccess(t *testing.T) {
	fake := &fakeModel{parts: okParts()}
	p := NewNativeProbe(func(c agentwire.Credentials, model string) (native.Model, error) {
		fake.creds, fake.model = c, model
		return fake, nil
	})
	res := p.Run(t.Context(), config.NativeConfig{APIFormat: "anthropic"}, "claude-sonnet-5", "sk-typed")
	if !res.OK || res.Code != "" || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}
	if res.Reply != "ok" || res.Model != "claude-sonnet-5" {
		t.Fatalf("reply/model = %+v", res)
	}
	if res.InputTokens != 14 || res.OutputTokens != 2 {
		t.Fatalf("usage = %+v", res)
	}
	if res.KeySource != "typed" || fake.creds.APIKey != "sk-typed" {
		t.Fatalf("typed key lost: %+v creds %+v", res, fake.creds)
	}
	if fake.creds.Format != "anthropic" || fake.model != "claude-sonnet-5" {
		t.Fatalf("credentials = %+v", fake.creds)
	}
	if res.LatencyMS < 0 {
		t.Fatalf("latency = %d", res.LatencyMS)
	}
}

func TestNativeProbeStreamErrorIsVerbatim(t *testing.T) {
	p := NewNativeProbe(func(agentwire.Credentials, string) (native.Model, error) {
		return &fakeModel{err: errors.New("provider down: 529")}, nil
	})
	res := p.Run(t.Context(), config.NativeConfig{}, "m", "sk")
	if res.OK || res.Code != "provider" || res.Error != "provider down: 529" {
		t.Fatalf("result = %+v", res)
	}
}

func TestNativeProbeTimeout(t *testing.T) {
	p := NewNativeProbe(func(agentwire.Credentials, string) (native.Model, error) {
		return &fakeModel{delay: 200 * time.Millisecond, parts: okParts()}, nil
	})
	p.timeout = 50 * time.Millisecond
	res := p.Run(t.Context(), config.NativeConfig{}, "m", "sk")
	if res.OK || res.Code != "timeout" {
		t.Fatalf("result = %+v", res)
	}
}

func TestNativeProbeNoModel(t *testing.T) {
	p := NewNativeProbe(func(agentwire.Credentials, string) (native.Model, error) {
		t.Fatal("factory must not run without a model")
		return nil, nil
	})
	res := p.Run(t.Context(), config.NativeConfig{}, "  ", "")
	if res.OK || res.Code != "no_model" {
		t.Fatalf("result = %+v", res)
	}
}

func TestNativeProbeNoKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("TREBI_PROBE_KEY", "")
	p := NewNativeProbe(func(agentwire.Credentials, string) (native.Model, error) {
		t.Fatal("factory must not run without a key")
		return nil, nil
	})
	res := p.Run(t.Context(), config.NativeConfig{APIKeyEnv: "TREBI_PROBE_KEY"}, "m", "")
	if res.OK || res.Code != "no_key" || res.KeySource != config.KeySourceNone {
		t.Fatalf("result = %+v", res)
	}
	if res.Error == "" {
		t.Fatal("no_key must carry the resolver text")
	}
}

func TestNativeProbeTypedKeyWinsOverStored(t *testing.T) {
	t.Setenv("TREBI_PROBE_KEY", "sk-env")
	fake := &fakeModel{parts: okParts()}
	p := NewNativeProbe(func(c agentwire.Credentials, _ string) (native.Model, error) {
		fake.creds = c
		return fake, nil
	})
	res := p.Run(t.Context(), config.NativeConfig{APIKeyEnv: "TREBI_PROBE_KEY"}, "m", "sk-typed")
	if !res.OK || res.KeySource != "typed" || fake.creds.APIKey != "sk-typed" {
		t.Fatalf("result = %+v creds %+v", res, fake.creds)
	}
}

// TestNativeProbeAgainstScriptedEndpoint drives the probe over the real
// OpenAI provider against a scripted endpoint.
func TestNativeProbeAgainstScriptedEndpoint(t *testing.T) {
	t.Setenv("TREBI_TEST_KEY", "sk-test")
	cfg := scriptServer(t, textTurn("ok"))
	p := NewNativeProbe(NewNativeModel)
	res := p.Run(t.Context(), cfg, "test-model", "")
	if !res.OK || res.Reply != "ok" {
		t.Fatalf("result = %+v", res)
	}
	if res.InputTokens != 11 || res.OutputTokens != 7 {
		t.Fatalf("usage = %+v", res)
	}
}
