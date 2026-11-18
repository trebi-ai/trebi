// Package waittest gives tests one deadline policy for waits and timeouts.
package waittest

import (
	"os"
	"strconv"
	"time"
)

// Default is the base deadline of a wait.
const Default = 30 * time.Second

const pollInterval = 20 * time.Millisecond

// Fataler is the part of *testing.T that a wait needs.
type Fataler interface {
	Helper()
	Fatalf(format string, args ...any)
}

// Scale returns 1, doubled in a race build, times TREBI_TEST_SCALE when set to a positive number.
func Scale() float64 {
	s := 1.0
	if raceBuild {
		s = 2
	}
	if v, err := strconv.ParseFloat(os.Getenv("TREBI_TEST_SCALE"), 64); err == nil && v > 0 {
		s *= v
	}
	return s
}

// D scales a duration by Scale.
func D(d time.Duration) time.Duration { return time.Duration(float64(d) * Scale()) }

// Until polls cond until it is true. It fails the test after D(Default).
func Until(t Fataler, what string, cond func() bool) {
	t.Helper()
	UntilWake(t, nil, what, cond)
}

// UntilWake is Until, and it also checks cond when the channel from wake closes.
// It calls wake again after each wake, because a bus wake channel closes and never sends.
func UntilWake(t Fataler, wake func() <-chan struct{}, what string, cond func() bool) {
	t.Helper()
	limit := D(Default)
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		var ch <-chan struct{}
		if wake != nil {
			ch = wake()
		}
		if cond() {
			return
		}
		select {
		case <-deadline.C:
			if cond() {
				return
			}
			t.Fatalf("timed out after %v waiting for %s", limit, what)
			return
		case <-tick.C:
		case <-ch:
		}
	}
}
