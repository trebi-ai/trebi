package dispatch

import (
	"sync"
	"time"
)

// RateLimit is N events per window.
type RateLimit struct {
	Count int
	Per   time.Duration
}

// Policy tracks debounce + rate-limit per key (workspace|job|onIndex).
type Policy struct {
	mu        sync.Mutex
	lastFired map[string]time.Time
	window    map[string][]time.Time
}

// NewPolicy creates empty policy state.
func NewPolicy() *Policy {
	return &Policy{
		lastFired: make(map[string]time.Time),
		window:    make(map[string][]time.Time),
	}
}

// Check dry-runs policy without recording a fire.
func (p *Policy) Check(key string, debounce time.Duration, rate *RateLimit, now time.Time) (blocked string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checkLocked(key, debounce, rate, now)
}

// CheckAndRecord atomically checks policy and, when allowed, records the fire.
func (p *Policy) CheckAndRecord(key string, debounce time.Duration, rate *RateLimit, now time.Time) (blocked string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b := p.checkLocked(key, debounce, rate, now); b != "" {
		return b
	}
	p.recordLocked(key, rate, now)
	return ""
}

// Record marks a successful fire.
func (p *Policy) Record(key string, rate *RateLimit, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recordLocked(key, rate, now)
}

// LoadKey seeds state (migration / DB load).
func (p *Policy) LoadKey(key string, lastFired time.Time, window []time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !lastFired.IsZero() {
		p.lastFired[key] = lastFired
	}
	if len(window) > 0 {
		p.window[key] = append([]time.Time(nil), window...)
	}
}

// checkLocked evaluates policy and prunes the stored window. Caller holds mu.
func (p *Policy) checkLocked(key string, debounce time.Duration, rate *RateLimit, now time.Time) (blocked string) {
	if debounce > 0 {
		if last, ok := p.lastFired[key]; ok && now.Sub(last) < debounce {
			return "debounced"
		}
	}
	if rate != nil && rate.Count > 0 && rate.Per > 0 {
		cut := now.Add(-rate.Per)
		var kept []time.Time
		for _, t := range p.window[key] {
			if t.After(cut) {
				kept = append(kept, t)
			}
		}
		if kept == nil {
			delete(p.window, key)
		} else {
			p.window[key] = kept
		}
		if len(kept) >= rate.Count {
			return "rate_limited"
		}
	}
	return ""
}

// recordLocked stamps lastFired; the window grows only for rate-limited keys
// (debounce-only keys must not accumulate an unbounded slice).
func (p *Policy) recordLocked(key string, rate *RateLimit, now time.Time) {
	p.lastFired[key] = now
	if rate != nil && rate.Count > 0 && rate.Per > 0 {
		p.window[key] = append(p.window[key], now)
	}
}
