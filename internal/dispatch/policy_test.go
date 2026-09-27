package dispatch_test

import (
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/dispatch"
)

func TestPolicyDebounce(t *testing.T) {
	t.Parallel()
	p := dispatch.NewPolicy()
	now := time.Now()
	key := "ws|job|0"
	if until := p.NextAllowed(key, time.Minute, nil, now); !until.IsZero() {
		t.Fatalf("first fire blocked until %s", until)
	}
	p.Record(key, nil, now)
	until := p.NextAllowed(key, time.Minute, nil, now.Add(10*time.Second))
	if want := now.Add(time.Minute); !until.Equal(want) {
		t.Fatalf("until=%s, want %s", until, want)
	}
	if until := p.NextAllowed(key, time.Minute, nil, now.Add(2*time.Minute)); !until.IsZero() {
		t.Fatalf("after the window blocked until %s", until)
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
	if until := p.NextAllowed(key, 0, rate, now.Add(2*time.Second)); !until.Equal(now.Add(time.Hour)) {
		t.Fatalf("until=%s, want the first fire to expire", until)
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
	if until := p.NextAllowed(key, 0, rate, now); !until.IsZero() {
		t.Fatalf("stale window blocked until %s", until)
	}
	if n := p.WindowLen(key); n != 0 {
		t.Fatalf("stale entries kept: window=%d", n)
	}
	p.Record(key, rate, now.Add(-2*time.Minute)) // outside window
	p.Record(key, rate, now.Add(-30*time.Second))
	p.Record(key, rate, now)
	if until := p.NextAllowed(key, 0, rate, now); !until.IsZero() {
		t.Fatalf("blocked until %s", until)
	}
	if n := p.WindowLen(key); n != 2 {
		t.Fatalf("window=%d, want 2", n)
	}

	// A debounce-only key must not grow a window at all.
	dkey := "ws|job|debounce"
	for i := 0; i < 100; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		p.Record(dkey, nil, at)
	}
	if n := p.WindowLen(dkey); n != 0 {
		t.Fatalf("debounce-only window=%d, want 0", n)
	}
}

func TestPolicyConcurrentRecord(t *testing.T) {
	t.Parallel()
	p := dispatch.NewPolicy()
	now := time.Now()
	key := "ws|job|3"
	rate := &dispatch.RateLimit{Count: 100, Per: time.Hour}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Record(key, rate, now)
		}()
	}
	wg.Wait()
	if n := p.WindowLen(key); n != 20 {
		t.Fatalf("window=%d, want 20", n)
	}
}
