package tray

import (
	"log/slog"
	"strconv"
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
		running := "Runs: " + strconv.Itoa(s.RunningCount)
		if s.WaitingCount > 0 {
			running += " · waiting: " + strconv.Itoa(s.WaitingCount)
		}
		lines = append(lines, running)
	}
	lines = append(lines, "Quit Trebi service")
	return lines
}
