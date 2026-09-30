package dispatch_test

import (
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/dispatch"
)

func TestPolicyOnePerWindow(t *testing.T) {
	t.Parallel()
	p := dispatch.NewPolicy()
	now := time.Now()
	key := "ws|job|0"
	rate := &dispatch.RateLimit{Count: 1, Per: time.Minute}
	if until := p.NextAllowed(key, rate, now); !until.IsZero() {
		t.Fatalf("first fire blocked until %s", until)
	}
	p.Record(key, rate, now)
	until := p.NextAllowed(key, rate, now.Add(10*time.Second))
	if want := now.Add(time.Minute); !until.Equal(want) {
		t.Fatalf("until=%s, want %s", until, want)
	}
	if until := p.NextAllowed(key, rate, now.Add(2*time.Minute)); !until.IsZero() {
		t.Fatalf("after the window blocked until %s", until)
	}
}

func TestPolicyOffNeverBlocks(t *testing.T) {
	t.Parallel()
	p := dispatch.NewPolicy()
	now := time.Now()
	for i := 0; i < 100; i++ {
		p.Record("k", nil, now)
	}
	if until := p.NextAllowed("k", nil, now); !until.IsZero() {
		t.Fatalf("off blocked until %s", until)
	}
	if n := p.Keys(); n != 0 {
		t.Fatalf("keys=%d, want 0", n)
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
	if until := p.NextAllowed(key, rate, now.Add(2*time.Second)); !until.Equal(now.Add(time.Hour)) {
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
	if until := p.NextAllowed(key, rate, now); !until.IsZero() {
		t.Fatalf("stale window blocked until %s", until)
	}
	if n := p.WindowLen(key); n != 0 {
		t.Fatalf("stale entries kept: window=%d", n)
	}
	p.Record(key, rate, now.Add(-2*time.Minute)) // outside window
	p.Record(key, rate, now.Add(-30*time.Second))
	p.Record(key, rate, now)
	if until := p.NextAllowed(key, rate, now); !until.IsZero() {
		t.Fatalf("blocked until %s", until)
	}
	if n := p.WindowLen(key); n != 2 {
		t.Fatalf("window=%d, want 2", n)
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

// TestPolicyPruneKeepsLiveWindow proves PruneIdle keeps a key while its
// window still holds, and drops it after.
func TestPolicyPruneKeepsLiveWindow(t *testing.T) {
	t.Parallel()
	p := dispatch.NewPolicy()
	now := time.Now()
	long := &dispatch.RateLimit{Count: 1, Per: 48 * time.Hour}
	short := &dispatch.RateLimit{Count: 1, Per: time.Minute}
	p.Record("long", long, now)
	p.Record("short", short, now)

	p.PruneIdle(now.Add(25 * time.Hour))
	if n := p.Keys(); n != 1 {
		t.Fatalf("keys=%d after 25h, want 1", n)
	}
	if until := p.NextAllowed("long", long, now.Add(25*time.Hour)); until.IsZero() {
		t.Fatal("a 48h window lost its entry after 25h")
	}
	p.PruneIdle(now.Add(49 * time.Hour))
	if n := p.Keys(); n != 0 {
		t.Fatalf("keys=%d after 49h, want 0", n)
	}
}

func TestRateLimitString(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"15/10m": "15/10m", "1/30s": "1/30s", "5/1h": "5/1h", "2/1h30m": "2/1h30m"} {
		if got := dispatch.ParseRateLimit(in).String(); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
	var off *dispatch.RateLimit
	if off.String() != "off" {
		t.Errorf("nil: got %s", off.String())
	}
}
