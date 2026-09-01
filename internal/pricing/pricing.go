// Package pricing computes cost_usd from token counters (B3).
package pricing

// Rates are USD per million tokens.
type Rates struct {
	Input         float64
	Output        float64
	CacheRead     float64
	CacheCreation float64
}

// Table looks up rates with an optional override map. Callers that need
// overrides construct one; production uses TokenUsage.ApplyRates (builtin only).
type Table struct {
	overrides map[string]Rates
}

func NewTable(overrides map[string]Rates) *Table {
	return &Table{overrides: overrides}
}

func (tbl *Table) apply(u *TokenUsage) {
	if u == nil {
		return
	}
	u.FillDerived()
	u.CostUSD = costUSD(u, tbl.lookup(u.Model))
}

func (tbl *Table) lookup(model string) Rates {
	if model == "" {
		model = "default"
	}
	if r, ok := tbl.overrides[model]; ok {
		return r
	}
	return builtinLookup(model)
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

// ApplyRates fills CostUSD using builtin model-family rates (no overrides).
func (t *TokenUsage) ApplyRates() {
	if t == nil {
		return
	}
	t.FillDerived()
	t.CostUSD = costUSD(t, builtinLookup(t.Model))
}

func costUSD(t *TokenUsage, r Rates) float64 {
	return (float64(t.Input)*r.Input +
		float64(t.Output)*r.Output +
		float64(t.CacheRead)*r.CacheRead +
		float64(t.CacheCreation)*r.CacheCreation) / 1_000_000
}

func builtinRates() map[string]Rates {
	return map[string]Rates{
		"claude":  {Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheCreation: 3.75},
		"default": {Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheCreation: 3.75},
	}
}

func builtinLookup(model string) Rates {
	if model == "" {
		model = "default"
	}
	builtin := builtinRates()
	if r, ok := builtin[model]; ok {
		return r
	}
	return builtin["default"]
}
