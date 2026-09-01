package tray

import (
	"log/slog"
	"sync"
)

// Snapshot is what the tray menu renders.
type Snapshot struct {
	Healthy      bool
	Version      string
	Draining     bool
	RunningCount int
	Message      string // short status line
	WaitingCount int    // agent waiting for human (if known)
}

// State is concurrent tray view.
type State struct {
	mu  sync.Mutex
	cur Snapshot
	log *slog.Logger
}

// NewState builds a tray state with its own logger.
func NewState() *State {
	return &State{log: slog.Default().With("source", "tray")}
}

func (s *State) Set(snap Snapshot) {
	s.mu.Lock()
	s.cur = snap
	s.mu.Unlock()
}

func (s *State) Get() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// MenuLines returns pure text rows for tests (no menuet).
func (s Snapshot) MenuLines() []string {
	status := "offline"
	if s.Healthy {
		status = "running"
		if s.Draining {
			status = "draining"
		}
	}
	head := "trebi · " + status
	if s.Version != "" {
		head += " · " + s.Version
	}
	lines := []string{head}
	if s.Message != "" {
		lines = append(lines, s.Message)
	}
	if s.Healthy {
		running := "Runs: " + itoa(s.RunningCount)
		if s.WaitingCount > 0 {
			running += " · waiting: " + itoa(s.WaitingCount)
		}
		lines = append(lines, running)
	}
	lines = append(lines, "Quit Trebi service")
	return lines
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	// small int without strconv import cycle worries
	const digits = "0123456789"
	if n < 0 {
		return "-" + itoa(-n)
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	return string(b[i:])
}
