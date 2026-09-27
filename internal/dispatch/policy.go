package dispatch

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// RateLimit is N events per window.
type RateLimit struct {
	Count int
	Per   time.Duration
}

// Policy tracks debounce + rate-limit per key
// (workspace/job/on_index/conversation_key).
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

// NextAllowed returns the first time a fire is allowed for key, or the zero
// time when a fire is allowed now. It does not record anything.
func (p *Policy) NextAllowed(key string, debounce time.Duration, rate *RateLimit, now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	var wait time.Duration
	if debounce > 0 {
		if last, ok := p.lastFired[key]; ok {
			if d := debounce - now.Sub(last); d > wait {
				wait = d
			}
		}
	}
	if rate != nil && rate.Count > 0 && rate.Per > 0 {
		p.window[key] = pruneWindow(p.window[key], now.Add(-rate.Per))
		if kept := p.window[key]; len(kept) >= rate.Count {
			// The oldest fire above the limit must expire first.
			oldest := kept[len(kept)-rate.Count]
			if d := oldest.Add(rate.Per).Sub(now); d > wait {
				wait = d
			}
		}
	}
	if wait <= 0 {
		return time.Time{}
	}
	return now.Add(wait)
}

// Record marks a successful fire.
func (p *Policy) Record(key string, rate *RateLimit, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recordLocked(key, rate, now)
}

// pruneWindow drops fires at or before cut.
func pruneWindow(window []time.Time, cut time.Time) []time.Time {
	var kept []time.Time
	for _, t := range window {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	return kept
}

// recordLocked stamps lastFired; the window grows only for rate-limited keys
// (debounce-only keys must not accumulate an unbounded slice).
func (p *Policy) recordLocked(key string, rate *RateLimit, now time.Time) {
	p.lastFired[key] = now
	if rate != nil && rate.Count > 0 && rate.Per > 0 {
		p.window[key] = append(p.window[key], now)
	}
}

// PruneIdle drops keys whose last fire is older than idle.
func (p *Policy) PruneIdle(now time.Time, idle time.Duration) {
	if idle <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cut := now.Add(-idle)
	for k, last := range p.lastFired {
		if last.Before(cut) {
			delete(p.lastFired, k)
			delete(p.window, k)
		}
	}
}

// ParseRateLimit parses "N/duration" e.g. "10/1m".
func ParseRateLimit(s string) *RateLimit {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	var n int
	if _, err := fmt.Sscanf(parts[0], "%d", &n); err != nil || n <= 0 {
		return nil
	}
	d, err := time.ParseDuration(parts[1])
	if err != nil || d <= 0 {
		return nil
	}
	return &RateLimit{Count: n, Per: d}
}
