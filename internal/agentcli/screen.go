package agentcli

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// IsAuthBlocker is true for strong harness login chrome (not agent prose about website logins).
// WaitReadyTool fails fast; mid-session reaper still needs sticky hits + ProbeAuth.
func IsAuthBlocker(screen string) bool {
	ok, _ := AuthBlockerMatch(screen)
	return ok
}

// AuthBlockerMatch is IsAuthBlocker plus the matched marker for logs/timeline.
func AuthBlockerMatch(screen string) (matched bool, reason string) {
	if screen == "" {
		return false, ""
	}
	flat := strings.ToLower(flattenScreen(screen))
	// Strong chrome only; omit vague "login required" phrases (false positives mid-session).
	for _, m := range authBlockerMarkers() {
		if strings.Contains(flat, m) {
			return true, m
		}
	}
	return false, ""
}

// IsLimitBlocker is true for strong harness usage/session-limit chrome (not agent
// prose about API rate limits). WaitReadyTool fails fast; mid-session reaper sticky-hits.
func IsLimitBlocker(screen string) bool {
	ok, _ := LimitBlockerMatch(screen)
	return ok
}

// LimitBlockerMatch is IsLimitBlocker plus the matched marker for logs/timeline.
func LimitBlockerMatch(screen string) (matched bool, reason string) {
	if screen == "" {
		return false, ""
	}
	flat := strings.ToLower(flattenScreen(screen))
	for _, m := range limitBlockerMarkers() {
		if strings.Contains(flat, m) {
			return true, m
		}
	}
	return false, ""
}

// AuthMissingError reports tool and wraps ErrAuthMissing for errors.Is.
type AuthMissingError struct {
	Tool string
}

func (e *AuthMissingError) Error() string {
	if e == nil {
		return ErrAuthMissing.Error()
	}
	return ErrAuthMissing.Error() + " — trebi harness auth login " + e.Tool
}

func (e *AuthMissingError) Unwrap() error { return ErrAuthMissing }

// ErrAuthMissing is returned by WaitReadyTool when the pane shows a login prompt.
var ErrAuthMissing = fmt.Errorf("auth missing: tool shows login prompt")

// LimitExceededError reports tool and wraps ErrLimitExceeded for errors.Is.
type LimitExceededError struct {
	Tool string
}

func (e *LimitExceededError) Error() string {
	if e == nil {
		return ErrLimitExceeded.Error()
	}
	if e.Tool != "" {
		return ErrLimitExceeded.Error() + " — tool " + e.Tool
	}
	return ErrLimitExceeded.Error()
}

func (e *LimitExceededError) Unwrap() error { return ErrLimitExceeded }

// ErrLimitExceeded is returned by WaitReadyTool when the pane shows usage/session limit chrome.
var ErrLimitExceeded = fmt.Errorf("limit exceeded: tool shows usage/session limit")

// Pane is one terminal that the policy reads and types into.
type Pane interface {
	Screen(ctx context.Context) (string, error)
	Send(ctx context.Context, text string) error
	Key(ctx context.Context, key string) error
}

func WaitReadyTool(ctx context.Context, pane Pane, tool Tool, timeout time.Duration) error {
	if tool == nil || IsUnknown(tool) {
		time.Sleep(3 * time.Second)
		return nil
	}
	io := readyIO{
		read:    func() (string, error) { return pane.Screen(ctx) },
		sendKey: func(key string) error { return pane.Key(ctx, key) },
		send:    func(text string) error { return pane.Send(ctx, text) },
	}
	if err := waitReadyLoop(tool, io, timeout); err != nil {
		return fmt.Errorf("%w in pane", err)
	}
	return nil
}

// authBlockerMarkers returns phrases observed on Claude (and similar) login chrome.
func authBlockerMarkers() []string {
	return []string{
		"please run /login",
		"not logged in",
		"run /login",
	}
}

// limitBlockerMarkers returns phrases observed on harness quota/session-limit chrome.
// Prefer multi-word chrome over vague "rate limit" prose that agents often write.
func limitBlockerMarkers() []string {
	return []string{
		// Claude Code (session / subscription windows)
		"you've hit your session limit",
		"hit your session limit",
		"/upgrade to increase your usage limit",
		"to increase your usage limit",
		"you've hit your usage limit",
		"hit your usage limit",
		// Shared / other harnesses (observed or likely chrome)
		"you've hit your limit",
		"usage limit exceeded",
		"rate limit exceeded",
		"quota exceeded",
		"quota exhausted",
		"out of credits",
		"insufficient credits",
		"you've run out of credits",
		"monthly usage limit",
		"you've reached your limit",
		"limit reached — try again",
		"limit reached - try again",
	}
}

// flattenScreen collapses whitespace so multi-line TUI chrome still matches.
func flattenScreen(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// readyIO abstracts the pane I/O the ready loop performs, so fixture tests can
// replay recorded screens without a live session. Zero-value sleep/now default
// to the real clock.
type readyIO struct {
	read    func() (string, error)
	sendKey func(key string) error
	send    func(text string) error
	sleep   func(time.Duration)
	now     func() time.Time
}

// waitReadyLoop polls io.read until tool.IsReady, applying HandleStartup
// actions up to MaxDialogTries per dialog. A stuck or unknown choice dialog
// returns DialogWaitingError instead of a ready timeout.
func waitReadyLoop(tool Tool, io readyIO, timeout time.Duration) error {
	if io.sleep == nil {
		io.sleep = time.Sleep
	}
	if io.now == nil {
		io.now = time.Now
	}

	deadline := io.now().Add(timeout)
	// Track fires per dialog identity so we don't re-send on every poll.
	applied := map[string]int{}

	for io.now().Before(deadline) {
		screen, err := io.read()
		if err != nil {
			io.sleep(1 * time.Second)
			continue
		}

		// Login / limit chrome looks "ready" (input + status bar) but the session can never work.
		if IsAuthBlocker(screen) {
			return &AuthMissingError{Tool: tool.Name()}
		}
		if IsLimitBlocker(screen) {
			return &LimitExceededError{Tool: tool.Name()}
		}

		acts := tool.HandleStartup(screen)
		fired := applyDialogActions(tool, screen, io, applied)
		if len(acts) > 0 && fired == 0 {
			// Auto-answer survived MaxDialogTries — don't burn the ready timeout.
			return dialogWaiting(tool, screen)
		}
		if len(acts) == 0 {
			if _, ok := ChoiceDialog(screen); ok {
				return dialogWaiting(tool, screen)
			}
		}

		if tool.IsReady(screen) {
			return nil
		}
		io.sleep(1 * time.Second)
	}

	return fmt.Errorf("timeout waiting for %s to become ready", tool.Name())
}

// ScreenClass is the coarse screen-derived state for tasks whose harness sent
// no hook events. Deliberately has no "waiting": a screen can never prove a
// blocked prompt, only lifecycle events can.
type ScreenClass string

const (
	ScreenBusy    ScreenClass = "busy"
	ScreenIdle    ScreenClass = "idle"
	ScreenUnknown ScreenClass = "unknown"
)

// ScreenClassifier is an optional Tool capability backing screen-mode
// lifecycle fallback (goldens: testdata/screens/<tool>/mid-session).
type ScreenClassifier interface {
	ClassifyScreen(screen string) ScreenClass
}

// chromeLines bounds classification to the pane's persistent bottom chrome
// (composer + status bar) so conversation body text can never spoof a state.
// Busy markers sit ≤7 lines from the bottom in every mid-session golden.
const chromeLines = 10

func bottomChrome(screen string) string {
	lines := strings.Split(screen, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > chromeLines {
		lines = lines[len(lines)-chromeLines:]
	}
	return strings.Join(lines, "\n")
}

// classifyChrome checks busy markers before idle: the composer and its
// placeholder stay visible while a turn runs, so idle markers alone prove
// nothing.
func classifyChrome(screen string, busy, idle []string) ScreenClass {
	chrome := bottomChrome(screen)
	for _, m := range busy {
		if strings.Contains(chrome, m) {
			return ScreenBusy
		}
	}
	for _, m := range idle {
		if strings.Contains(chrome, m) {
			return ScreenIdle
		}
	}
	return ScreenUnknown
}

// Claude busy: older CLIs hint "esc to interrupt"; newer ones show an active
// "<spinner> <Verb>… (Ns · …)" line whose "… (" never appears on the
// past-tense finished line ("✻ Cooked for 36s", no parens).
func (claudeTool) ClassifyScreen(screen string) ScreenClass {
	return classifyChrome(screen, []string{"esc to interrupt", "… ("}, []string{" min | ", "auto mode on"})
}

func (codexTool) ClassifyScreen(screen string) ScreenClass {
	return classifyChrome(screen, []string{"esc to interrupt"}, []string{"› "})
}

func (copilotTool) ClassifyScreen(screen string) ScreenClass {
	return classifyChrome(screen, []string{"Esc to cancel"}, []string{"? help"})
}

func (geminiTool) ClassifyScreen(screen string) ScreenClass {
	return classifyChrome(screen, []string{"esc to cancel"}, []string{"Type your message"})
}
