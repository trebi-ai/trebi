package runs

import "strings"

func splitPaneLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func linesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// paneDelta returns new lines that appeared between two pane snapshots.
// Identical redraws (TUI spinner) yield nothing. A scrolled pane yields the
// newly revealed tail. A full-screen change with little overlap yields next.
func paneDelta(prev, next string) string {
	if next == "" || next == prev {
		return ""
	}
	if prev == "" {
		return strings.TrimRight(next, "\n")
	}
	prevL := splitPaneLines(prev)
	nextL := splitPaneLines(next)
	max := len(prevL)
	if len(nextL) < max {
		max = len(nextL)
	}
	for k := max; k >= 1; k-- {
		if linesEqual(prevL[len(prevL)-k:], nextL[:k]) {
			if k == len(nextL) {
				return ""
			}
			return strings.Join(nextL[k:], "\n")
		}
	}
	if paneSimilar(prevL, nextL) {
		return ""
	}
	return strings.Join(nextL, "\n")
}

func paneSimilar(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, l := range a {
		set[l] = struct{}{}
	}
	hit := 0
	for _, l := range b {
		if _, ok := set[l]; ok {
			hit++
		}
	}
	return hit*100/len(b) >= 70
}
