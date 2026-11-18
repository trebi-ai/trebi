package waittest

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeT struct{ fatals []string }

func (f *fakeT) Helper() {}
func (f *fakeT) Fatalf(format string, args ...any) {
	f.fatals = append(f.fatals, fmt.Sprintf(format, args...))
}

func TestScale(t *testing.T) {
	base := 1.0
	if raceBuild {
		base = 2
	}
	for _, c := range []struct {
		env  string
		want float64
	}{{"", base}, {"3", 3 * base}, {"abc", base}, {"-1", base}, {"0", base}} {
		t.Setenv("TREBI_TEST_SCALE", c.env)
		if got := Scale(); got != c.want {
			t.Errorf("TREBI_TEST_SCALE=%q: Scale = %v, want %v", c.env, got, c.want)
		}
	}
	t.Setenv("TREBI_TEST_SCALE", "2")
	if got := D(time.Second); got != time.Duration(2*base*float64(time.Second)) {
		t.Errorf("D = %v", got)
	}
}

func TestUntilReturnsAtOnce(t *testing.T) {
	f := &fakeT{}
	calls := 0
	Until(f, "x", func() bool { calls++; return calls >= 2 })
	if len(f.fatals) != 0 {
		t.Fatalf("Fatalf called: %v", f.fatals)
	}
}

func TestUntilFailsAfterDeadline(t *testing.T) {
	t.Setenv("TREBI_TEST_SCALE", "0.001")
	f := &fakeT{}
	Until(f, "the thing", func() bool { return false })
	if len(f.fatals) != 1 || !strings.Contains(f.fatals[0], "the thing") {
		t.Fatalf("fatals = %v", f.fatals)
	}
}

func TestUntilWakeChecksOnWake(t *testing.T) {
	f := &fakeT{}
	var mu sync.Mutex
	var chans []chan struct{}
	ready := false
	wake := func() <-chan struct{} {
		mu.Lock()
		defer mu.Unlock()
		ch := make(chan struct{})
		chans = append(chans, ch)
		return ch
	}
	go func() {
		// Wake once with the condition false, then once with it true.
		for i := 0; i < 2; i++ {
			for {
				mu.Lock()
				n := len(chans)
				mu.Unlock()
				if n > i {
					break
				}
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			if i == 1 {
				ready = true
			}
			close(chans[i])
			mu.Unlock()
		}
	}()
	start := time.Now()
	UntilWake(f, wake, "ready", func() bool { mu.Lock(); defer mu.Unlock(); return ready })
	if len(f.fatals) != 0 {
		t.Fatalf("Fatalf called: %v", f.fatals)
	}
	if d := time.Since(start); d >= pollInterval {
		t.Errorf("returned after %v, want before the %v poll", d, pollInterval)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(chans) < 2 {
		t.Errorf("wake calls = %d, want a new channel after each wake", len(chans))
	}
}
