package proposals

import (
	"fmt"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// UnifiedDiff renders a line-oriented unified diff for review. Mail bodies
// truncate it; the UI uses the proposal's stored endpoint contents directly.
func UnifiedDiff(base, proposed string) string {
	if base == proposed {
		return ""
	}
	baseLines := splitKeepEmpty(strings.SplitAfter(base, "\n"))
	nextLines := splitKeepEmpty(strings.SplitAfter(proposed, "\n"))
	diff := difflib.UnifiedDiff{
		A:        baseLines,
		FromFile: "AGENT.md (reviewed)",
		B:        nextLines,
		ToFile:   "AGENT.md (proposed)",
		Context:  3,
	}
	text, err := difflib.GetUnifiedDiffString(diff)
	if err != nil {
		return ""
	}
	return text
}

func splitKeepEmpty(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	// SplitAfter makes the final line empty when input ends in newline; that
	// is not a blank line, and keeping it confuses difflib's line numbering.
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// DiffStats summarizes a diff without materializing the entire mail body.
func DiffStats(base, proposed string) (added, removed int) {
	for _, line := range strings.Split(UnifiedDiff(base, proposed), "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			added++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			removed++
		}
	}
	return added, removed
}

func TruncateDiff(text string, maxLines int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= maxLines {
		return text
	}
	return fmt.Sprintf("%s\n... %d more lines; the full diff is in the UI",
		strings.Join(lines[:maxLines], "\n"), len(lines)-maxLines)
}
