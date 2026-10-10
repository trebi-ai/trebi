package decisions

import (
	"context"
	"errors"
	"strings"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/agent-wire/native"

	"github.com/trebi-ai/trebi/internal/config"
)

const nativeMaxTokens = 600

// NativeDeps are the parts of the native assistant that the decider uses.
// The daemon fills them, so that this package does not import assistant.
type NativeDeps struct {
	Config   func() config.NativeConfig
	Model    func() string // decisions.model, else the native default
	NewModel func(agentwire.Credentials, string) (native.Model, error)
	Guard    func(ctx context.Context) error // the daily token cap; nil is no cap
}

// Native asks the native provider in one call with no tools.
type Native struct{ deps NativeDeps }

func NewNative(deps NativeDeps) *Native { return &Native{deps: deps} }

func (n *Native) Name() string { return "native:" + n.deps.Model() }

// Available reports a native provider with a key.
func (n *Native) Available() bool {
	c := n.deps.Config()
	if !c.Enabled() || n.deps.Model() == "" {
		return false
	}
	_, _, err := c.ResolveKey()
	return err == nil
}

func (n *Native) Decide(ctx context.Context, r Request) (Result, error) {
	c := n.deps.Config()
	key, _, err := c.ResolveKey()
	if err != nil {
		return Result{}, err
	}
	if n.deps.Guard != nil {
		if err := n.deps.Guard(ctx); err != nil {
			return Result{}, err
		}
	}
	m, err := n.deps.NewModel(agentwire.Credentials{Format: c.Format(), BaseURL: c.BaseURL, APIKey: key, Headers: c.Headers},
		n.deps.Model())
	if err != nil {
		return Result{}, err
	}
	p := BuildPrompt(r, "")
	req := native.Request{System: p.System, MaxOutputTokens: nativeMaxTokens,
		Messages: []native.Message{{Role: native.RoleUser, Parts: []native.Part{{Text: &native.TextPart{Text: p.User}}}}}}
	var text strings.Builder
	finished := false
	for part, err := range m.Stream(ctx, req) {
		if err != nil {
			return Result{Raw: []byte(text.String())}, err
		}
		switch part.Kind {
		case native.StreamTextDelta:
			text.WriteString(part.Delta)
		case native.StreamFinish:
			finished = true
		}
	}
	raw := []byte(text.String())
	if !finished {
		return Result{Raw: raw}, errors.New("native: the model stream ended with no finish part")
	}
	answers, err := ParseAnswers(raw)
	if err != nil {
		return Result{Raw: raw}, checkErr{err}
	}
	return Result{Answers: answers, Method: MethodVerbal, Raw: raw}, nil
}
