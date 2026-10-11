// Package decisionstest has a scripted decider for tests.
package decisionstest

import (
	"context"
	"sync"
	"time"

	"github.com/trebi-ai/trebi/internal/decisions"
	"github.com/trebi-ai/trebi/internal/pricing"
)

// Step is one scripted reply.
type Step struct {
	Answers []decisions.Answer
	Err     error
	Delay   time.Duration
	Tokens  pricing.TokenUsage
}

// Fake replies with its steps in sequence and repeats the last step.
type Fake struct {
	mu       sync.Mutex
	steps    []Step
	Requests []decisions.Request
}

func New(steps ...Step) *Fake { return &Fake{steps: steps} }

// Values makes a step from question id and value pairs, each with confidence 0.9.
func Values(kv ...string) Step {
	var s Step
	for i := 0; i+1 < len(kv); i += 2 {
		s.Answers = append(s.Answers, decisions.Answer{ID: kv[i], Value: kv[i+1], Confidence: 0.9})
	}
	return s
}

func (f *Fake) Name() string { return "fake" }

func (f *Fake) Decide(ctx context.Context, r decisions.Request) (decisions.Result, error) {
	f.mu.Lock()
	f.Requests = append(f.Requests, r)
	var s Step
	if n := len(f.Requests); len(f.steps) > 0 {
		s = f.steps[min(n, len(f.steps))-1]
	}
	f.mu.Unlock()
	if s.Delay > 0 {
		select {
		case <-time.After(s.Delay):
		case <-ctx.Done():
			return decisions.Result{}, ctx.Err()
		}
	}
	return decisions.Result{Answers: s.Answers, Method: decisions.MethodVerbal, Raw: []byte("fake"), Tokens: s.Tokens}, s.Err
}

// Calls is the number of Decide calls.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Requests)
}
