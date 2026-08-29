package cli

import (
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchParentPipeEOF(t *testing.T) {
	t.Parallel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	var cancelled atomic.Bool
	go watchParent(func() { cancelled.Store(true) }, 0, r)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cancelled.Load() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("cancel not called after pipe close")
}

func TestWatchParentDeadPID(t *testing.T) {
	t.Parallel()
	var cancelled atomic.Bool
	go watchParent(func() { cancelled.Store(true) }, 1<<30, nil)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cancelled.Load() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("cancel not called for dead pid")
}

func TestProcessAliveSelf(t *testing.T) {
	t.Parallel()
	if !processAlive(os.Getpid()) {
		t.Fatal("self should be alive")
	}
	if processAlive(1 << 30) {
		t.Fatal("bogus pid should be dead")
	}
}
