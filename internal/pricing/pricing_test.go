package pricing

import (
	"math"
	"testing"
)

const epsilon = 1e-9

func TestApplyComputesCostFromRates(t *testing.T) {
	t.Cleanup(func() { SetOverrides(nil) })
	u := &TokenUsage{
		Model:         "claude",
		Input:         1_000_000,
		Output:        1_000_000,
		CacheRead:     1_000_000,
		CacheCreation: 1_000_000,
	}
	Apply(u)
	// claude builtin: 3.0 + 15.0 + 0.30 + 3.75 per million each.
	want := 3.0 + 15.0 + 0.30 + 3.75
	if math.Abs(u.CostUSD-want) > epsilon {
		t.Fatalf("CostUSD = %v, want %v", u.CostUSD, want)
	}
}

func TestApplyUnknownModelUsesDefaultRates(t *testing.T) {
	t.Cleanup(func() { SetOverrides(nil) })
	u := &TokenUsage{Model: "no-such-model", Input: 1_000_000}
	Apply(u)
	// default builtin input rate: 3.0 per million.
	if math.Abs(u.CostUSD-3.0) > epsilon {
		t.Fatalf("CostUSD = %v, want 3.0", u.CostUSD)
	}
	e := &TokenUsage{Model: "", Output: 1_000_000}
	Apply(e)
	// default builtin output rate: 15.0 per million.
	if math.Abs(e.CostUSD-15.0) > epsilon {
		t.Fatalf("empty model CostUSD = %v, want 15.0", e.CostUSD)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	t.Cleanup(func() { SetOverrides(nil) })
	Apply(nil) // no-op
	u := &TokenUsage{Model: "claude", Input: 500_000, Output: 200_000}
	Apply(u)
	first := u.CostUSD
	Apply(u)
	if math.Abs(u.CostUSD-first) > epsilon {
		t.Fatalf("CostUSD changed on second Apply: %v != %v", u.CostUSD, first)
	}
}

func TestSetOverridesReplaces(t *testing.T) {
	t.Cleanup(func() { SetOverrides(nil) })
	SetOverrides(map[string]Rates{
		"model-x": {Input: 100.0},
	})
	u := &TokenUsage{Model: "model-x", Input: 1_000_000}
	Apply(u)
	if math.Abs(u.CostUSD-100.0) > epsilon {
		t.Fatalf("override CostUSD = %v, want 100.0", u.CostUSD)
	}
	// Replace with a map that does not contain model-x.
	SetOverrides(map[string]Rates{
		"model-y": {Input: 50.0},
	})
	Apply(u)
	// model-x falls back to builtin default: 3.0 per million input.
	if math.Abs(u.CostUSD-3.0) > epsilon {
		t.Fatalf("after replace CostUSD = %v, want 3.0", u.CostUSD)
	}
}
