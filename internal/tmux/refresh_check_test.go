package tmux

import (
	"strings"
	"testing"
)

// Diagnostic: prints what RefreshRunning would see.
func TestDiagListSessions(t *testing.T) {
	out, err := ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	t.Logf("raw output:\n%s\n", out)
	active := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := line
		if idx := strings.IndexByte(line, '\t'); idx >= 0 {
			name = line[:idx]
		}
		active[name] = true
	}
	t.Logf("parsed names: %v", keys(active))
	if active["trebi-2d4143f5"] {
		t.Logf("trebi-2d4143f5 IS active (RefreshRunning would NOT mark it exited)")
	} else {
		t.Logf("trebi-2d4143f5 NOT found (RefreshRunning WOULD mark it exited)")
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
