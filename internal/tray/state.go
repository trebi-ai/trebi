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
	return &State{log: slog.Default().With("logger", "tray")}
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

// BuildMenuLines returns pure text rows for tests (no menuet).
func BuildMenuLines(snap Snapshot) []string {
	status := "offline"
	if snap.Healthy {
		status = "running"
		if snap.Draining {
			status = "draining"
		}
	}
	head := "trebi · " + status
	if snap.Version != "" {
		head += " · " + snap.Version
	}
	lines := []string{head}
	if snap.Message != "" {
		lines = append(lines, snap.Message)
	}
	if snap.Healthy {
		lines = append(lines, fmtRunning(snap.RunningCount, snap.WaitingCount))
	}
	lines = append(lines, "Quit Trebi service")
	return lines
}

func fmtRunning(running, waiting int) string {
	if waiting > 0 {
		return "Runs: " + itoa(running) + " · waiting: " + itoa(waiting)
	}
	return "Runs: " + itoa(running)
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

// countWaiting walks common agent-state JSON shapes.
func countWaiting(raw any) int {
	n := 0
	switch v := raw.(type) {
	case map[string]any:
		if states, ok := v["states"].(map[string]any); ok {
			return countWaiting(states)
		}
		if agents, ok := v["agents"].([]any); ok {
			return countWaiting(agents)
		}
		for _, val := range v {
			switch s := val.(type) {
			case string:
				if s == "waiting" {
					n++
				}
			case map[string]any:
				if st, ok := s["state"].(string); ok && st == "waiting" {
					n++
				} else if st, ok := s["agent_state"].(string); ok && st == "waiting" {
					n++
				}
			}
		}
	case []any:
		for _, item := range v {
			n += countWaiting(item)
		}
	}
	return n
}
