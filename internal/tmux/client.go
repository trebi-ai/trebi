package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// SessionNamePrefix prefixes every trebi-managed tmux session name.
const SessionNamePrefix = "trebi-"

// cmdDeadline is how long any single tmux client invocation may run.
// send-keys has been observed to hang for minutes (client stuck in select
// waiting for the server); without a deadline the event queue claim blocks
// forever and acknowledge never fires.
const cmdDeadline = 5 * time.Second

// NewSession starts a detached tmux session at cwd; name is "trebi-<8 hex>".
func NewSession(cwd string) (string, error) {
	name, err := newSessionName()
	if err != nil {
		return "", err
	}
	args := []string{"new-session", "-d", "-s", name}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	if _, err := run(args...); err != nil {
		return "", fmt.Errorf("tmux new-session: %w", err)
	}
	applyScrollbackMouseBindings(name)
	return name, nil
}

// NewNamedSession starts a detached tmux session with a caller-chosen name.
func NewNamedSession(name, cwd string) error {
	args := []string{"new-session", "-d", "-s", name}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	if _, err := run(args...); err != nil {
		return fmt.Errorf("tmux new-session: %w", err)
	}
	applyScrollbackMouseBindings(name)
	return nil
}

// Send pastes literal text into the active pane. Multi-line or large payloads
// use load-buffer/paste-buffer (bracketed paste). Short single-line text uses
// send-keys -l, with one paste-buffer retry on timeout or other failure.
func Send(session, text string) error {
	const sendKeysSafeLimit = 4096
	// Multi-line send-keys -l has hung against busy TUI panes; paste is safer
	// and also avoids ARG_MAX for large cold-start prompts.
	if len(text) > sendKeysSafeLimit || strings.ContainsAny(text, "\n\r") {
		return sendViaBuffer(session, text)
	}
	if _, err := run("send-keys", "-t", session, "-l", text); err != nil {
		// No Client struct; stamp source inline (documented Phase 4.3 exception).
		slog.Warn("tmux send-keys failed; retrying via paste-buffer",
			"source", "tmux", "session", session, "error", err)
		if err2 := sendViaBuffer(session, text); err2 != nil {
			return fmt.Errorf("tmux send-keys: %w (paste retry: %v)", err, err2)
		}
		return nil
	}
	return nil
}

// SendKey sends a named key (e.g. "enter", "c-c") without -l.
func SendKey(session, key string) error {
	if _, err := run("send-keys", "-t", session, translateKey(key)); err != nil {
		return fmt.Errorf("tmux send-keys: %w", err)
	}
	return nil
}

func ReadScreen(session string) (string, error) {
	out, err := run("capture-pane", "-p", "-t", session)
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %w", err)
	}
	return out, nil
}

// CaptureScrollback returns the pane content including scrollback history (from
// the first scrollback line through the visible screen), for post-mortem debug.
func CaptureScrollback(session string) (string, error) {
	out, err := run("capture-pane", "-p", "-S", "-", "-t", session)
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane (scrollback): %w", err)
	}
	return out, nil
}

// RenameSession sets the window title only — not the session name (our handle).
func RenameSession(session, title string) error {
	sanitized := sanitizeWindowName(title)
	if sanitized == "" {
		return nil
	}
	if _, err := run("rename-window", "-t", session, sanitized); err != nil {
		return fmt.Errorf("tmux rename-window: %w", err)
	}
	return nil
}

// SelectSession switches the current client (requires $TMUX). Outside tmux, use attach.
func SelectSession(session string) error {
	if _, err := run("switch-client", "-t", session); err != nil {
		return fmt.Errorf("tmux switch-client: %w", err)
	}
	return nil
}

func ListSessions() (string, error) {
	return ListSessionsFormatted("#{session_name}\t#{session_path}\t#{session_created}")
}

func ListSessionsFormatted(format string) (string, error) {
	out, err := run("list-sessions", "-F", format)
	if err != nil {
		if strings.Contains(err.Error(), "no server running") {
			return "", nil
		}
		return "", fmt.Errorf("tmux list-sessions: %w", err)
	}
	return out, nil
}

func ListTrebiSessions() ([]string, error) {
	out, err := ListSessions()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := line
		if idx := strings.IndexByte(line, '\t'); idx >= 0 {
			name = line[:idx]
		}
		if strings.HasPrefix(name, SessionNamePrefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

// KillSession kills the session.
func KillSession(session string) error {
	if _, err := run("kill-session", "-t", session); err != nil {
		return fmt.Errorf("tmux kill-session: %w", err)
	}
	return nil
}

func HasSession(session string) bool {
	_, err := run("has-session", "-t", session)
	return err == nil
}

// WaitForPrompt polls until a snippet of prompt appears on screen, matching
// tail or head (normalized for TUI borders/wrapping): wrapping inputs show
// the tail at the cursor; truncating inputs (grok's "…") show only the head.
func WaitForPrompt(session, prompt string, timeout time.Duration) error {
	tail := normalizeScreen(promptSnippet(prompt, 24))
	if tail == "" {
		return nil
	}
	head := normalizeScreen(promptHead(prompt, 24))

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		screen, err := ReadScreen(session)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		norm := normalizeScreen(screen)
		if strings.Contains(norm, tail) || (head != "" && strings.Contains(norm, head)) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for prompt text to appear on screen in session %s", session)
}

// applyScrollbackMouseBindings: trebi sessions get wheel→copy-mode (prefix-guarded).
func applyScrollbackMouseBindings(session string) {
	_, _ = run("set-option", "-t", session, "mouse", "on")
	prefixGuard := fmt.Sprintf("#{m:%s*,#{session_name}}", SessionNamePrefix)
	_, _ = run("bind-key", "-T", "root", "-N", "trebi scrollback", "WheelUpPane",
		"if-shell", "-F", "-t=", prefixGuard,
		"copy-mode -eu",
		"send-keys -M")
	_, _ = run("bind-key", "-T", "root", "-N", "trebi scrollback", "WheelDownPane",
		"if-shell", "-F", "-t=", prefixGuard,
		"if-shell -F -t= '#{pane_in_mode}' 'send-keys -X halfpage-down' 'send-keys -M'",
		"send-keys -M")
}

// sendViaBuffer pastes via a unique named buffer (-d deletes after paste).
func sendViaBuffer(session, text string) error {
	bufName, err := newBufferName()
	if err != nil {
		return err
	}
	if err := runStdin(text, "load-buffer", "-b", bufName, "-"); err != nil {
		return fmt.Errorf("tmux load-buffer: %w", err)
	}
	if _, err := run("paste-buffer", "-d", "-p", "-b", bufName, "-t", session); err != nil {
		_, _ = run("delete-buffer", "-b", bufName)
		return fmt.Errorf("tmux paste-buffer: %w", err)
	}
	return nil
}

func newBufferName() (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generating buffer name: %w", err)
	}
	return "trebi-" + hex.EncodeToString(buf[:]), nil
}

// promptHead returns up to n chars from the start of the prompt's first
// non-empty line.
func promptHead(prompt string, n int) string {
	for _, line := range strings.Split(prompt, "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		if len(s) > n {
			s = s[:n]
		}
		return s
	}
	return ""
}

func promptSnippet(prompt string, n int) string {
	lines := strings.Split(prompt, "\n")
	line := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			line = s
			break
		}
	}
	if line == "" {
		return ""
	}
	if len(line) > n {
		line = line[len(line)-n:]
	}
	return line
}

// normalizeScreen strips box-drawing chars and collapses whitespace.
func normalizeScreen(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		switch r {
		case '│', '┃', '┆', '┇', '┊', '┋', '|',
			'─', '━', '┄', '┅', '┈', '┉',
			'╭', '╮', '╯', '╰', '┌', '┐', '└', '┘', '├', '┤', '┬', '┴', '┼':
			r = ' '
		}
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimSpace(b.String())
}

// translateKey maps friendly key names to tmux send-keys names.
func translateKey(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "enter", "return":
		return "Enter"
	case "tab":
		return "Tab"
	case "escape", "esc":
		return "Escape"
	case "space":
		return "Space"
	case "c-c", "ctrl-c":
		return "C-c"
	case "c-d", "ctrl-d":
		return "C-d"
	}
	return key
}

// sanitizeWindowName strips tabs/newlines (tmux rejects them).
func sanitizeWindowName(in string) string {
	var b strings.Builder
	for _, r := range in {
		switch r {
		case '\t', '\n', '\r':
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func newSessionName() (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generating session name: %w", err)
	}
	return SessionNamePrefix + hex.EncodeToString(buf[:]), nil
}

// tmuxEnv forces UTF-8 locale so list-sessions -F keeps tabs as literal
// (launchd has no LANG; without this, tabs become underscores).
func tmuxEnv() []string {
	return append(os.Environ(), "LC_ALL=en_US.UTF-8")
}

func run(args ...string) (string, error) {
	return runWithDeadline(cmdDeadline, args...)
}

func runWithDeadline(deadline time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Env = tmuxEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			op := "tmux"
			if len(args) > 0 {
				op = "tmux " + args[0]
			}
			return "", fmt.Errorf("%s: timed out after %s", op, deadline)
		}
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func runStdin(stdin string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cmdDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Env = tmuxEnv()
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			op := "tmux"
			if len(args) > 0 {
				op = "tmux " + args[0]
			}
			return fmt.Errorf("%s: timed out after %s", op, cmdDeadline)
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
