package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// wsAttach streams one run over a WebSocket connection. Two server modes:
// screen frames (TUI runs) and normalized events (subprocess runs).
type wsAttach struct {
	conn      *websocket.Conn
	out       io.Writer
	raw       bool // interactive TTY: clear screen per frame, forward keys
	eventMode bool // CLI runtime: one line per normalized event, never clear
}

// readMessages pumps server messages to the terminal until end or error.
func (a *wsAttach) readMessages(errCh chan<- error, done chan<- struct{}) {
	defer close(done)
	for {
		_, msg, err := a.conn.ReadMessage()
		if err != nil {
			errCh <- err
			return
		}
		var m map[string]any
		if json.Unmarshal(msg, &m) != nil {
			continue
		}
		switch fmt.Sprint(m["type"]) {
		case "screen":
			a.printScreen(fmt.Sprint(m["data"]))
		case "event":
			a.printEvent(m["data"])
		case "status":
			a.printStatus(m["data"])
		case "ended":
			fmt.Fprintln(a.out, "\n(session ended)")
			errCh <- io.EOF
			return
		case "error":
			errCh <- fmt.Errorf("%v", m["data"])
			return
		}
	}
}

// printEvent renders one normalized wire event as text (Part H: until the UI
// lands, CLI attach prints the event stream).
func (a *wsAttach) printEvent(data any) {
	m, _ := data.(map[string]any)
	if m == nil {
		fmt.Fprintln(a.out, data)
		return
	}
	kind := mapStr(m, "kind")
	text := mapStr(m, "text")
	if text == "" {
		text = mapStr(m, "message")
	}
	if text == "" {
		text = mapStr(m, "question")
	}
	if text != "" {
		fmt.Fprintf(a.out, "[%s] %s\n", kind, text)
		return
	}
	if tool := mapStr(m, "tool"); tool != "" {
		fmt.Fprintf(a.out, "[%s] %s %s\n", kind, tool, mapStr(m, "status"))
		return
	}
	fmt.Fprintf(a.out, "[%s]\n", kind)
}

// printStatus renders the periodic session status frame.
func (a *wsAttach) printStatus(data any) {
	m, _ := data.(map[string]any)
	if m == nil {
		return
	}
	line := mapStr(m, "state")
	if tool := mapStr(m, "tool"); tool != "" {
		line += " · " + tool
	}
	if last := mapStr(m, "last_event_at"); last != "" {
		line += " · last output " + last
	}
	fmt.Fprintf(a.out, "— %s\n", line)
}

func (a *wsAttach) printScreen(data string) {
	// Clear + home when interactive so each frame replaces the previous.
	if a.raw {
		fmt.Fprint(a.out, "\x1b[H\x1b[2J")
	}
	fmt.Fprint(a.out, data)
	if !strings.HasSuffix(data, "\n") {
		fmt.Fprintln(a.out)
	}
}

// forwardKeys sends raw stdin bytes as key events. Ctrl-C detaches.
func (a *wsAttach) forwardKeys(stdin io.Reader, errCh chan<- error) {
	buf := make([]byte, 64)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			// Ctrl-C in raw mode: detach without killing remote.
			if n == 1 && buf[0] == 3 {
				errCh <- nil
				return
			}
			_ = a.conn.WriteJSON(map[string]any{"type": "keys", "data": string(buf[:n])}) // disconnect makes further key delivery irrelevant
		}
		if err != nil {
			return
		}
	}
}

// attachAfterLaunch attaches after create/wake. Local prefers tmux; remote
// always uses WS /v1/runs/{id}/attach. CLI-runtime runs never open a pane:
// their attach is the normalized event stream.
func attachAfterLaunch(ctx context.Context, c *cli.Command, client *APIClient, run map[string]any) error {
	runID := mapStr(run, "id")
	if runID == "" {
		return fmt.Errorf("run has no id")
	}
	events := mapStr(run, "runtime") == "cli"
	if attach, ok := run["attach"].(map[string]any); ok && mapStr(attach, "kind") == "events" {
		events = true
	}
	if client.IsRemote() || events {
		return attachWebSocket(ctx, c, client, runID, events)
	}
	session := mapStr(run, "tmux_session")
	if session == "" {
		session = client.WaitTmuxSession(runID, 5*time.Second)
	}
	if session != "" {
		bin, err := exec.LookPath("tmux")
		if err != nil {
			return fmt.Errorf("locating tmux: %w", err)
		}
		return syscall.Exec(bin, []string{"tmux", "attach", "-t", session}, os.Environ())
	}
	// Local fallback when session is not ready: WS screen stream.
	return attachWebSocket(ctx, c, client, runID, false)
}

// attachWebSocket streams GET /v1/runs/{id}/attach (screen or events).
func attachWebSocket(ctx context.Context, c *cli.Command, client *APIClient, runID string, events bool) error {
	conn, err := client.DialAttach(runID)
	if err != nil {
		return err
	}
	defer conn.Close()

	w := outWriter(c)
	if events {
		fmt.Fprintf(w, "attached to run %s (events). Ctrl-C to detach.\n", runID)
	} else {
		fmt.Fprintf(w, "attached to run %s (WebSocket). Ctrl-C to detach.\n", runID)
	}

	// Optional raw stdin → key events when stdout is a TTY. Event-mode runs
	// render a transcript; keep the terminal cooked so output is readable.
	stdin := os.Stdin
	raw := false
	if !events && term.IsTerminal(int(stdin.Fd())) {
		if st, err := term.MakeRaw(int(stdin.Fd())); err == nil {
			raw = true
			defer func() { _ = term.Restore(int(stdin.Fd()), st) }() // restore the terminal even when attach exits with an error
		}
	}
	a := &wsAttach{conn: conn, out: w, raw: raw, eventMode: events}

	done := make(chan struct{})
	errCh := make(chan error, 2)
	go a.readMessages(errCh, done)
	if raw {
		go a.forwardKeys(stdin, errCh)
	}

	// Interrupt → detach.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-done:
		return nil
	case err := <-errCh:
		if attachEndOK(err) {
			return nil
		}
		return err
	case <-sigCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// attachEndOK reports errors that mean a normal session end.
func attachEndOK(err error) bool {
	if err == nil || err == io.EOF {
		return true
	}
	// websocket close is normal on end
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return true
	}
	return strings.Contains(err.Error(), "close")
}

func httpToWS(httpURL string) (string, error) {
	u, err := url.Parse(httpURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// ok
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	return u.String(), nil
}
