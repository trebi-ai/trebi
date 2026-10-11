package pricing

import (
	"math"
	"testing"
)

const epsilon = 1e-9

func TestApplyRatesComputesCostFromRates(t *testing.T) {
	t.Parallel()
	u := &TokenUsage{
		Model:         "claude",
		Input:         1_000_000,
		Output:        1_000_000,
		CacheRead:     1_000_000,
		CacheCreation: 1_000_000,
	}
	u.ApplyRates()
	// claude builtin: 3.0 + 15.0 + 0.30 + 3.75 per million each.
	want := 3.0 + 15.0 + 0.30 + 3.75
	if math.Abs(u.CostUSD-want) > epsilon {
		t.Fatalf("CostUSD = %v, want %v", u.CostUSD, want)
	}
}

func TestApplyRatesUnknownModelUsesDefaultRates(t *testing.T) {
	t.Parallel()
	u := &TokenUsage{Model: "no-such-model", Input: 1_000_000}
	u.ApplyRates()
	// default builtin input rate: 3.0 per million.
	if math.Abs(u.CostUSD-3.0) > epsilon {
		t.Fatalf("CostUSD = %v, want 3.0", u.CostUSD)
	}
	e := &TokenUsage{Model: "", Output: 1_000_000}
	e.ApplyRates()
	// default builtin output rate: 15.0 per million.
	if math.Abs(e.CostUSD-15.0) > epsilon {
		t.Fatalf("empty model CostUSD = %v, want 15.0", e.CostUSD)
	}
}

func TestApplyRatesIsIdempotent(t *testing.T) {
	t.Parallel()
	(*TokenUsage)(nil).ApplyRates() // no-op
	u := &TokenUsage{Model: "claude", Input: 500_000, Output: 200_000}
	u.ApplyRates()
	first := u.CostUSD
	u.ApplyRates()
	if math.Abs(u.CostUSD-first) > epsilon {
		t.Fatalf("CostUSD changed on second ApplyRates: %v != %v", u.CostUSD, first)
	}
}

func TestTableApplyOverrides(t *testing.T) {
	t.Parallel()
	tbl := NewTable(map[string]Rates{
		"model-x": {Input: 100.0},
	})
	u := &TokenUsage{Model: "model-x", Input: 1_000_000}
	tbl.apply(u)
	if math.Abs(u.CostUSD-100.0) > epsilon {
		t.Fatalf("override CostUSD = %v, want 100.0", u.CostUSD)
	}
	tbl = NewTable(map[string]Rates{
		"model-y": {Input: 50.0},
	})
	tbl.apply(u)
	// model-x falls back to builtin default: 3.0 per million input.
	if math.Abs(u.CostUSD-3.0) > epsilon {
		t.Fatalf("after replace CostUSD = %v, want 3.0", u.CostUSD)
	}
}

func TestTokenTotalsAdd(t *testing.T) {
	t.Parallel()
	var tot TokenTotals
	tot.Add(TokenUsage{Input: 10, Output: 20, CacheRead: 30, CacheCreation: 40})
	tot.Add(TokenUsage{Input: 1, Output: 2, CacheRead: 3, CacheCreation: 4})
	want := TokenTotals{Input: 11, Output: 22, CacheRead: 33, CacheCreation: 44, Billed: 11 + 22 + 44, Cached: 33}
	if tot != want {
		t.Fatalf("totals = %+v, want %+v", tot, want)
	}
	if tot.Input+tot.Output+tot.CacheRead+tot.CacheCreation != tot.Billed+tot.Cached {
		t.Fatalf("split %+v does not sum to billed plus cached", tot)
	}
}
