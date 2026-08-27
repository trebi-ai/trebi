package dispatch_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/dispatch"
)

func TestPolicyDebounce(t *testing.T) {
	t.Parallel()
	p := dispatch.NewPolicy()
	now := time.Now()
	key := "ws|job|0"
	if b := p.Check(key, time.Minute, nil, now); b != "" {
		t.Fatal(b)
	}
	p.Record(key, nil, now)
	if b := p.Check(key, time.Minute, nil, now.Add(10*time.Second)); b != "debounced" {
		t.Fatalf("got %q", b)
	}
	if b := p.Check(key, time.Minute, nil, now.Add(2*time.Minute)); b != "" {
		t.Fatalf("got %q", b)
	}
}

func TestPolicyRateLimit(t *testing.T) {
	t.Parallel()
	p := dispatch.NewPolicy()
	now := time.Now()
	key := "ws|job|1"
	rate := &dispatch.RateLimit{Count: 2, Per: time.Hour}
	p.Record(key, rate, now)
	p.Record(key, rate, now.Add(time.Second))
	if b := p.Check(key, 0, rate, now.Add(2*time.Second)); b != "rate_limited" {
		t.Fatalf("got %q", b)
	}
}

func TestPolicyWindowPruned(t *testing.T) {
	t.Parallel()
	p := dispatch.NewPolicy()
	now := time.Now()
	key := "ws|job|2"
	rate := &dispatch.RateLimit{Count: 3, Per: time.Minute}
	for i := 0; i < 100; i++ {
		p.Record(key, rate, now.Add(-2*time.Hour).Add(time.Duration(i)*time.Second))
	}
	if b := p.Check(key, 0, rate, now); b != "" {
		t.Fatal(b)
	}
	if n := p.WindowLen(key); n != 0 {
		t.Fatalf("stale entries kept: window=%d", n)
	}
	p.Record(key, rate, now.Add(-2*time.Minute)) // outside window
	p.Record(key, rate, now.Add(-30*time.Second))
	p.Record(key, rate, now)
	if b := p.Check(key, 0, rate, now); b != "" {
		t.Fatal(b)
	}
	if n := p.WindowLen(key); n != 2 {
		t.Fatalf("window=%d, want 2", n)
	}

	// A debounce-only key must not grow a window at all.
	dkey := "ws|job|debounce"
	for i := 0; i < 100; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		if b := p.CheckAndRecord(dkey, 30*time.Second, nil, at); b != "" {
			t.Fatalf("fire %d blocked: %q", i, b)
		}
	}
	if n := p.WindowLen(dkey); n != 0 {
		t.Fatalf("debounce-only window=%d, want 0", n)
	}
}

func TestPolicyRateLimitAtomicUnderConcurrency(t *testing.T) {
	t.Parallel()
	p := dispatch.NewPolicy()
	now := time.Now()
	key := "ws|job|3"
	rate := &dispatch.RateLimit{Count: 1, Per: time.Hour}
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.CheckAndRecord(key, 0, rate, now) == "" {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := allowed.Load(); n != 1 {
		t.Fatalf("allowed=%d, want 1", n)
	}
}
