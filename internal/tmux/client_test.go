package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetectTool(t *testing.T) {
	t.Parallel()
	// Compatibility smoke test; full matrix lives in tool_test.go.
	cases := []struct {
		command string
		want    string
	}{
		{"claude", "claude"},
		{"claude --dangerously-skip-permissions", "claude"},
		{"/usr/local/bin/claude", "claude"},
		{"codex", "codex"},
		{"copilot-cli chat", "copilot"},
		{"gemini", "gemini"},
		{"opencode", "opencode"},
		{"grok", "grok"},
		{"", ""},
		{"unknown-tool", ""},
	}
	for _, c := range cases {
		if got := detectTool(c.command); got != c.want {
			t.Errorf("detectTool(%q) = %q, want %q", c.command, got, c.want)
		}
	}
}

func TestPromptSnippet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 64, ""},
		{"   ", 64, ""},
		{"hello world", 64, "hello world"},
		{"line one\nline two", 64, "line two"},
		{"first\n\nmiddle\n\n", 64, "middle"}, // trailing blanks skipped
		{"a very long single line here", 8, "ine here"},
	}
	for _, c := range cases {
		if got := promptSnippet(c.in, c.n); got != c.want {
			t.Errorf("promptSnippet(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestNormalizeScreenMatchesWrappedBox(t *testing.T) {
	t.Parallel()
	// Simulate what tmux capture-pane shows for a Claude input box: text
	// wrapped across multiple lines inside `│ ... │` borders. The snippet
	// matches the prompt's TAIL because long prompts scroll off the top
	// of the input widget.
	screen := "╭────────────╮\n│ transactions and │\n│ balance here │\n╰────────────╯"
	snippet := normalizeScreen(promptSnippet("we want to get the latest transactions and balance here", 24))
	if !strings.Contains(normalizeScreen(screen), snippet) {
		t.Fatalf("normalized screen %q should contain snippet %q", normalizeScreen(screen), snippet)
	}
}

func TestNewWorkspaceAndExistsAndClose(t *testing.T) {
	requireTmux(t)
	name, err := NewSession("")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !strings.HasPrefix(name, SessionNamePrefix) {
		t.Errorf("session name %q should start with %q", name, SessionNamePrefix)
	}
	if !HasSession(name) {
		t.Fatalf("HasSession(%q) = false, want true", name)
	}
	if err := KillSession(name); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if HasSession(name) {
		t.Errorf("session %q still exists after KillSession", name)
	}
}

func TestSendAndReadScreen(t *testing.T) {
	name := newTestSession(t)

	// Send a unique marker string followed by Enter, then wait for it to
	// appear on the pane. We use a short sleep between send and read so
	// the shell has time to echo.
	marker := "hello-trebi-tmux-test-marker"
	if err := Send(name, "echo "+marker); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := SendKey(name, "enter"); err != nil {
		t.Fatalf("SendKey: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		screen, err := ReadScreen(name)
		if err != nil {
			t.Fatalf("ReadScreen: %v", err)
		}
		if strings.Contains(screen, marker) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("marker %q never appeared on screen within 5s", marker)
}

func TestSendLargePayloadViaBuffer(t *testing.T) {
	// Regression: prompts >~16KB tripped "command too long" on `send-keys -l`.
	// Send must transparently route large payloads through load-buffer/paste-buffer.
	name := newTestSession(t)

	// 20KB of a benign character; well past the 4KB threshold and past the
	// observed ~16KB tmux/exec failure point.
	payload := strings.Repeat("a", 20*1024)
	if err := Send(name, payload); err != nil {
		t.Fatalf("Send (large): %v", err)
	}

	// Confirm no Send-owned buffer was leaked (trebi-<8hex> from newBufferName).
	assertNoTrebiSendBuffers(t)
}

func TestSendMultilineUsesPasteBuffer(t *testing.T) {
	// Multi-line follow-ups must not use send-keys -l (can hang on busy TUIs).
	name := newTestSession(t)
	payload := "line one\nline two\nline three"
	if err := Send(name, payload); err != nil {
		t.Fatalf("Send (multiline): %v", err)
	}
	assertNoTrebiSendBuffers(t)
}

// zshPath is the interactive shell TestSendTrailingNewlineRuns needs. zsh's
// line editor asks the terminal for bracketed paste, which is the pane state
// that swallows a pasted newline. Bash readline does not do this everywhere.
const zshPath = "/bin/zsh"

func TestSendTrailingNewlineRuns(t *testing.T) {
	// Regression: a trailing newline used to ride inside the bracketed paste.
	// tmux puts a pasted newline in a bracketed-paste pane's edit buffer
	// without submitting it, so login and run launches sent "cmd\n" and the
	// command stayed typed but unrun.
	requireTmux(t)
	if _, err := os.Stat(zshPath); err != nil {
		t.Skipf("%s not installed — skipping", zshPath)
	}
	name, err := newSessionName()
	if err != nil {
		t.Fatalf("newSessionName: %v", err)
	}
	if _, err := run("new-session", "-d", "-s", name, "-c", t.TempDir(), zshPath+" -i"); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	t.Cleanup(func() {
		_ = KillSession(name)
	})
	if err := WaitForPrompt(name, "$ ", 5*time.Second); err != nil {
		t.Fatalf("waiting for the zsh prompt: %v", err)
	}

	marker := filepath.Join(t.TempDir(), "ran")
	if err := Send(name, "touch "+marker+"\n"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	screen, _ := ReadScreen(name)
	t.Fatalf("command sent with a trailing newline never ran; pane:\n%s", screen)
}

func TestRunTimeout(t *testing.T) {
	requireTmux(t)
	// wait-for with no -S blocks until signalled — must be killed by the deadline.

	start := time.Now()
	_, err := runWithDeadline(200*time.Millisecond, "wait-for", "trebi-test-never-signalled")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error from wait-for")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timed out error, got: %v", err)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("returned too fast (%v); deadline may not have applied", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took too long (%v); process may not have been killed", elapsed)
	}
}

func TestRenameWorkspace(t *testing.T) {
	name := newTestSession(t)
	// RenameSession now renames the *window*, not the session, so
	// subsequent send-keys against the original session name continue to
	// work. Verify: the session still exists and the window title changed.
	title := "renamed window " + strings.TrimPrefix(name, SessionNamePrefix)
	if err := RenameSession(name, title); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	if !HasSession(name) {
		t.Errorf("session %q should still exist after window rename", name)
	}
	out, err := run("list-windows", "-t", name, "-F", "#{window_name}")
	if err != nil {
		t.Fatalf("list-windows: %v", err)
	}
	if !strings.Contains(out, title) {
		t.Errorf("window name %q not in list-windows output %q", title, out)
	}
}

func TestListWorkspacesIncludesSession(t *testing.T) {
	name := newTestSession(t)

	out, err := ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if !strings.Contains(out, name) {
		t.Errorf("list-sessions output %q does not contain %q", out, name)
	}

	// Output should be tab-delimited: name\tpath\tcreated
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, SessionNamePrefix) {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			t.Errorf("list-sessions line %q has fewer than 3 tab-delimited fields", line)
		}
	}
}

func TestWaitForPromptFindsSnippet(t *testing.T) {
	name := newTestSession(t)

	marker := "trebi-waitforprompt-test"
	// Run waitgroup-style: send asynchronously then verify WaitForPrompt
	// returns once the text appears.
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = Send(name, "echo "+marker)
		_ = SendKey(name, "enter")
	}()

	if err := WaitForPrompt(name, "echo "+marker, 5*time.Second); err != nil {
		t.Fatalf("WaitForPrompt: %v", err)
	}
}

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed — skipping")
	}
	// Ensure a server is up so client commands don't fail-fast with "no server".
	// A detached keepalive session keeps the server alive between tests.
	_ = exec.Command("tmux", "start-server").Run()
	_ = exec.Command("tmux", "new-session", "-d", "-s", "trebi-test-keepalive").Run()
}

// newTestSession creates a throwaway tmux session and registers a cleanup
// that kills it. Test name collisions are avoided by the random suffix in
// newSessionName().
func newTestSession(t *testing.T) string {
	t.Helper()
	requireTmux(t)
	name, err := NewSession("")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() {
		_ = KillSession(name)
	})
	return name
}

// assertNoTrebiSendBuffers fails if a load-buffer name from newBufferName remains
// (trebi- + 8 hex). Ignores unrelated buffers that merely contain "trebi-".
func assertNoTrebiSendBuffers(t *testing.T) {
	t.Helper()
	out, err := run("list-buffers")
	if err != nil {
		t.Fatalf("list-buffers: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		// list-buffers format: "name: N bytes: \"…\""
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if len(name) == len("trebi-")+8 && strings.HasPrefix(name, "trebi-") {
			hexPart := name[len("trebi-"):]
			if isHex(hexPart) {
				t.Errorf("expected trebi- send buffer deleted after paste, still have: %s", line)
			}
		}
	}
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return s != ""
}
