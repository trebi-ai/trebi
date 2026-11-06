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
	// HarnessUSD is the cost that the harness reports; 0 when it reports none.
	HarnessUSD float64 `json:"-"`
}

// FillDerived sets billed/cached from counters (cost filled by pricing map).
func (t *TokenUsage) FillDerived() {
	t.Billed = t.Input + t.Output + t.CacheCreation
	t.Cached = t.CacheRead
}

// ApplyRates fills CostUSD: the harness cost when there is one, else the
// builtin model-family rates (no overrides).
func (t *TokenUsage) ApplyRates() {
	if t == nil {
		return
	}
	t.FillDerived()
	if t.HarnessUSD > 0 {
		t.CostUSD = t.HarnessUSD
		return
	}
	t.CostUSD = costUSD(t, builtinLookup(t.Model))
}

// TokenTotals sums the token counters of many turns. It has no model and no cost.
type TokenTotals struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
	Billed        int64 `json:"billed"`
	Cached        int64 `json:"cached"`
}

// Add folds the counters of u into the totals.
func (t *TokenTotals) Add(u TokenUsage) {
	t.Input += u.Input
	t.Output += u.Output
	t.CacheRead += u.CacheRead
	t.CacheCreation += u.CacheCreation
	t.Billed += u.Input + u.Output + u.CacheCreation
	t.Cached += u.CacheRead
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
