package tmux

import (
	"strings"
	"testing"
)

// Chrome markers quoted in conversation body text — outside the bottom
// chrome region — must never classify.
func TestClassifyScreen_SpoofResistance(t *testing.T) {
	body := strings.Repeat("model output quoting chrome: esc to interrupt "+
		"Esc to cancel esc to cancel Type your message ? help ›  min | \n", 5)
	neutral := strings.Repeat("plain output line with no markers\n", chromeLines+2)
	screen := body + neutral

	for _, c := range classifierTools(t) {
		if got := c.ClassifyScreen(screen); got != ScreenUnknown {
			t.Errorf("%T: body-spoofed screen classified %s, want unknown", c, got)
		}
	}
}

// Trailing blank lines (tmux pads the pane) must not push chrome out of the
// inspected region.
func TestClassifyScreen_TrailingBlanks(t *testing.T) {
	screen := "filler\n• Working (5s • esc to interrupt)\n› \n" + strings.Repeat("\n", 30)
	if got := (codexTool{}).ClassifyScreen(screen); got != ScreenBusy {
		t.Errorf("busy with trailing blanks = %s, want busy", got)
	}
}

// Busy must outrank idle: composers and placeholders stay visible mid-turn.
func TestClassifyScreen_BusyOutranksIdle(t *testing.T) {
	cases := []struct {
		tool   Tool
		screen string
	}{
		{codexTool{}, "• Working (0s • esc to interrupt)\n\n› Implement {feature}\n footer"},
		{copilotTool{}, "◎ Thinking (Esc to cancel)\n❯\n / commands · ? help"},
		{geminiTool{}, "⠧ waiting (esc to cancel, 0s)\n│ >   Type your message or @path/to/file │\n footer"},
		{claudeTool{}, "✳ Deliberating… (esc to interrupt)\n❯ \n  Fable 5 | 2.0 min | 10K (5%) | /tmp/x"},
	}
	for _, c := range cases {
		if got := c.tool.(ScreenClassifier).ClassifyScreen(c.screen); got != ScreenBusy {
			t.Errorf("%s: busy+idle chrome = %s, want busy", c.tool.Name(), got)
		}
	}
}

// The ScreenClass vocabulary has no waiting on purpose — a screen cannot
// prove a blocked prompt. Guard against one sneaking in.
func TestClassifyScreen_NeverWaiting(t *testing.T) {
	screens := []string{
		"",
		"Do you trust the files in this folder?\n❯ 1. Yes",
		"Permission required: run rm -rf /tmp/x? (y/n)",
		"• Working (0s • esc to interrupt)\n› ",
		"❯ \n  Fable 5 | 2.0 min | 10K (5%) | /tmp/x",
	}
	for _, c := range classifierTools(t) {
		for _, s := range screens {
			switch got := c.ClassifyScreen(s); got {
			case ScreenBusy, ScreenIdle, ScreenUnknown:
			default:
				t.Errorf("%T: ClassifyScreen(%q) = %q — outside the vocabulary", c, s, got)
			}
		}
	}
}

// classifierTools: every tool implementing ScreenClassifier.
func classifierTools(t *testing.T) []ScreenClassifier {
	t.Helper()
	var out []ScreenClassifier
	for _, tool := range AllTools() {
		if c, ok := tool.(ScreenClassifier); ok {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		t.Fatal("no tools implement ScreenClassifier")
	}
	return out
}
