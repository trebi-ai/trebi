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

// String writes the limit in the file form, for example "15/10m".
func (r *RateLimit) String() string {
	if r == nil {
		return "off"
	}
	return fmt.Sprintf("%d/%s", r.Count, shortDuration(r.Per))
}

// Policy tracks the rate limit per key
// (workspace/job/on_index/conversation_key).
type Policy struct {
	mu     sync.Mutex
	window map[string][]time.Time
	// expires is the time after which the window of a key holds no entries.
	expires map[string]time.Time
}

// NewPolicy creates empty policy state.
func NewPolicy() *Policy {
	return &Policy{
		window:  make(map[string][]time.Time),
		expires: make(map[string]time.Time),
	}
}

// NextAllowed returns the first time a fire is allowed for key, or the zero
// time when a fire is allowed now. It does not record anything.
func (p *Policy) NextAllowed(key string, rate *RateLimit, now time.Time) time.Time {
	if !rate.active() {
		return time.Time{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.window[key] = pruneWindow(p.window[key], now.Add(-rate.Per))
	kept := p.window[key]
	if len(kept) < rate.Count {
		return time.Time{}
	}
	// The oldest fire above the limit must expire first.
	return kept[len(kept)-rate.Count].Add(rate.Per)
}

// Record marks a successful fire.
func (p *Policy) Record(key string, rate *RateLimit, now time.Time) {
	if !rate.active() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.window[key] = append(p.window[key], now)
	if exp := now.Add(rate.Per); exp.After(p.expires[key]) {
		p.expires[key] = exp
	}
}

func (r *RateLimit) active() bool {
	return r != nil && r.Count > 0 && r.Per > 0
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

// PruneIdle drops keys whose rate window holds no entries.
func (p *Policy) PruneIdle(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, exp := range p.expires {
		if !now.Before(exp) {
			delete(p.window, k)
			delete(p.expires, k)
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

// shortDuration drops the zero units of time.Duration.String: 10m0s is 10m.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
