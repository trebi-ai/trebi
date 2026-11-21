package agentcli

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MaxDialogTries is how many times we send the same auto-answer before treating
// the dialog as stuck (waiting), not a launch timeout.
const MaxDialogTries = 2

// ErrDialogWaiting means the pane is blocked on a TUI choice. The run should
// stay live (session waiting) instead of failing launch.
var ErrDialogWaiting = fmt.Errorf("waiting on harness dialog")

// DialogWaitingError wraps ErrDialogWaiting with the scraped question.
type DialogWaitingError struct {
	Tool     string
	Question string
}

func (e *DialogWaitingError) Error() string {
	if e == nil {
		return ErrDialogWaiting.Error()
	}
	q := e.Question
	if q == "" {
		q = "harness dialog"
	}
	if e.Tool != "" {
		return ErrDialogWaiting.Error() + " — " + e.Tool + ": " + q
	}
	return ErrDialogWaiting.Error() + ": " + q
}

func (e *DialogWaitingError) Unwrap() error { return ErrDialogWaiting }

// TrustAction picks option 1 when the dialog is numbered, otherwise Enter
// (arrow lists whose first item is already highlighted).
func TrustAction(screen string) Action {
	acts := TrustYesActions(screen)
	if len(acts) == 1 {
		return acts[0]
	}
	return Action{Key: "enter"}
}

// TrustYesActions confirms the trust/yes option. Numbered lists send "1".
// Unnumbered lists move the highlight onto Yes (Claude now puts "No, exit"
// first with no 1./2. labels), then Enter.
func TrustYesActions(screen string) []Action {
	if hasNumberedOption1(screen) {
		return []Action{{Text: "1"}}
	}
	return trustMoveToYes(screen)
}

func trustMoveToYes(screen string) []Action {
	lines := strings.Split(screen, "\n")
	cursor, yes := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "❯") || strings.Contains(line, "›") {
			cursor = i
		}
		low := strings.ToLower(line)
		if strings.Contains(low, "yes") && strings.Contains(low, "trust") {
			yes = i
		}
	}
	if cursor < 0 || yes < 0 || cursor == yes {
		return []Action{{Key: "enter"}}
	}
	if yes > cursor {
		return []Action{{Key: "down"}, {Key: "enter"}}
	}
	return []Action{{Key: "up"}, {Key: "enter"}}
}

// ChoiceDialog reports an unanswered numbered TUI prompt that is not in the
// auto-answer catalog. Conservative: numbered option 1 plus confirm chrome.
// Idle composers and transcript "?" never match.
func ChoiceDialog(screen string) (question string, ok bool) {
	if screen == "" || !hasNumberedOption1(screen) {
		return "", false
	}
	flat := strings.ToLower(flattenScreen(screen))
	if !hasConfirmChrome(flat) {
		return "", false
	}
	return scrapeChoiceQuestion(screen), true
}

// DialogKey fingerprints a dialog independent of cursor position, so a
// re-render that moves ❯ onto "No" still counts as the same prompt.
func DialogKey(screen string, act Action) string {
	return act.Key + "\x00" + act.Text + "\x00" + dialogFingerprint(screen)
}

// SendAction types act.Text and/or presses act.Key in pane.
func SendAction(ctx context.Context, pane Pane, act Action) error {
	if act.Key != "" {
		if err := pane.Key(ctx, act.Key); err != nil {
			return err
		}
	}
	if act.Text != "" {
		if err := pane.Send(ctx, act.Text); err != nil {
			return err
		}
	}
	return nil
}

var option1Line = regexp.MustCompile(`(?m)^\s*[❯›●→ ]*1\.`)

func hasNumberedOption1(screen string) bool {
	return option1Line.MatchString(screen)
}

func hasConfirmChrome(flatLower string) bool {
	for _, p := range []string{
		"enter to confirm",
		"press enter to continue",
		"enter to select",
		"enter select",
	} {
		if strings.Contains(flatLower, p) {
			return true
		}
	}
	return false
}

func dialogFingerprint(screen string) string {
	n := strings.NewReplacer("❯", " ", "›", " ", "→", " ", "●", " ").Replace(screen)
	n = flattenScreen(n)
	if len(n) > 160 {
		n = n[:160]
	}
	return n
}

var choiceOptionRe = regexp.MustCompile(`^\s*[❯›●→ ]*1\.`)

func scrapeChoiceQuestion(screen string) string {
	var buf []string
	for _, line := range strings.Split(screen, "\n") {
		if choiceOptionRe.MatchString(line) {
			break
		}
		t := strings.TrimSpace(line)
		t = strings.Trim(t, "│╭╮╰╯─│ ")
		if t == "" || strings.HasPrefix(t, "─") {
			continue
		}
		buf = append(buf, t)
	}
	for i := len(buf) - 1; i >= 0; i-- {
		if strings.Contains(buf[i], "?") {
			return buf[i]
		}
	}
	if n := len(buf); n > 0 {
		return buf[n-1]
	}
	return "harness is waiting on a choice"
}

func applyDialogActions(tool Tool, screen string, io readyIO, applied map[string]int) (fired int) {
	for _, act := range tool.HandleStartup(screen) {
		key := DialogKey(screen, act)
		if applied[key] >= MaxDialogTries {
			continue
		}
		io.sleep(1 * time.Second)
		if act.Key != "" && io.sendKey != nil {
			_ = io.sendKey(act.Key)
		}
		if act.Text != "" && io.send != nil {
			_ = io.send(act.Text)
		}
		applied[key]++
		fired++
		io.sleep(1 * time.Second)
	}
	return fired
}

func dialogWaiting(tool Tool, screen string) *DialogWaitingError {
	q := scrapeChoiceQuestion(screen)
	if q == "" {
		q = "harness dialog"
	}
	return &DialogWaitingError{Tool: tool.Name(), Question: q}
}
