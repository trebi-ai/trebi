// Package pricing computes cost_usd from token counters (B3).
package pricing

import (
	"sync"
)

// Rates are USD per million tokens.
type Rates struct {
	Input         float64
	Output        float64
	CacheRead     float64
	CacheCreation float64
}

//nolint:gochecknoglobals // process-wide override table; the exported function API needs shared state
var (
	mu        sync.RWMutex
	overrides map[string]Rates
)

// SetOverrides replaces the override map wholesale (from config reload).
func SetOverrides(m map[string]Rates) {
	mu.Lock()
	defer mu.Unlock()
	overrides = m
}

// TokenUsage — B3: four counters + model; dollars always computed.
type TokenUsage struct {
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	CacheRead     int64   `json:"cache_read"`
	CacheCreation int64   `json:"cache_creation"`
	Model         string  `json:"model,omitempty"`
	Billed        int64   `json:"billed"`
	Cached        int64   `json:"cached"`
	CostUSD       float64 `json:"cost_usd"`
}

// FillDerived sets billed/cached from counters (cost filled by pricing map).
func (t *TokenUsage) FillDerived() {
	t.Billed = t.Input + t.Output + t.CacheCreation
	t.Cached = t.CacheRead
}

// Apply fills CostUSD on t using the model family rates.
func Apply(t *TokenUsage) {
	if t == nil {
		return
	}
	t.FillDerived()
	r := lookup(t.Model)
	t.CostUSD = (float64(t.Input)*r.Input +
		float64(t.Output)*r.Output +
		float64(t.CacheRead)*r.CacheRead +
		float64(t.CacheCreation)*r.CacheCreation) / 1_000_000
}

// builtinRates returns the immutable default rate table.
func builtinRates() map[string]Rates {
	return map[string]Rates{
		"claude":  {Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheCreation: 3.75},
		"default": {Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheCreation: 3.75},
	}
}

// lookup resolves rates: overrides → builtin → builtin default.
func lookup(model string) Rates {
	if model == "" {
		model = "default"
	}
	mu.RLock()
	defer mu.RUnlock()
	if r, ok := overrides[model]; ok {
		return r
	}
	builtin := builtinRates()
	if r, ok := builtin[model]; ok {
		return r
	}
	return builtin["default"]
}
