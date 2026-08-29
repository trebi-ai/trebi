package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urfave/cli/v2"
	"golang.org/x/term"
)

// wsAttach streams one run over a WebSocket screen connection.
type wsAttach struct {
	conn *websocket.Conn
	out  io.Writer
	raw  bool // interactive TTY: clear screen per frame, forward keys
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

// attachRun attaches to a live run: local tmux when available, else WebSocket
// screen stream (works for remote daemons).
func attachRun(c *cli.Context, client *APIClient, runID string) error {
	if runID == "" {
		return fmt.Errorf("run id required")
	}
	var run map[string]any
	if err := client.GetJSON("/v1/runs/"+runID, &run); err != nil {
		return err
	}
	return attachAfterLaunch(c, client, run)
}

// attachAfterLaunch attaches after create/wake. Local prefers tmux; remote
// always uses WS /v1/runs/{id}/attach.
func attachAfterLaunch(c *cli.Context, client *APIClient, run map[string]any) error {
	runID := mapStr(run, "id")
	if runID == "" {
		return fmt.Errorf("run has no id")
	}
	if client.IsRemote() {
		return attachWebSocket(c, client, runID)
	}
	session := mapStr(run, "tmux_session")
	if session == "" {
		session = waitTmuxSession(client, runID, 5*time.Second)
	}
	if session != "" {
		return execTmuxAttach(session)
	}
	// Local fallback when session is not ready: WS screen stream.
	return attachWebSocket(c, client, runID)
}

func waitTmuxSession(client *APIClient, runID string, maxWait time.Duration) string {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		var body map[string]any
		if err := client.GetJSON("/v1/runs/"+runID, &body); err == nil {
			if s := mapStr(body, "tmux_session"); s != "" {
				return s
			}
			st := mapStr(body, "status")
			if st != "" && st != "running" && st != "starting" {
				return ""
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return ""
}

// execTmuxAttach replaces the process with tmux attach (jobi run --attach parity).
func execTmuxAttach(session string) error {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("locating tmux: %w", err)
	}
	return syscall.Exec(bin, []string{"tmux", "attach", "-t", session}, os.Environ())
}

// attachWebSocket streams GET /v1/runs/{id}/attach (screen + optional keys).
func attachWebSocket(c *cli.Context, client *APIClient, runID string) error {
	conn, err := dialAttach(client, runID)
	if err != nil {
		return err
	}
	defer conn.Close()

	w := outWriter(c)
	fmt.Fprintf(w, "attached to run %s (WebSocket). Ctrl-C to detach.\n", runID)

	// Optional raw stdin → key events when stdout is a TTY.
	stdin := os.Stdin
	raw := false
	if term.IsTerminal(int(stdin.Fd())) {
		if st, err := term.MakeRaw(int(stdin.Fd())); err == nil {
			raw = true
			defer func() { _ = term.Restore(int(stdin.Fd()), st) }() // restore the terminal even when attach exits with an error
		}
	}
	a := &wsAttach{conn: conn, out: w, raw: raw}

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
	case <-c.Done():
		return c.Err()
	}
}

func dialAttach(client *APIClient, runID string) (*websocket.Conn, error) {
	return dialAttachPath(client, "/v1/runs/"+url.PathEscape(runID)+"/attach")
}

func dialAttachPath(client *APIClient, path string) (*websocket.Conn, error) {
	wsURL, err := httpToWS(client.Base + path)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	if client.Token != "" {
		hdr.Set("Authorization", "Bearer "+client.Token)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.Dial(wsURL, hdr)
	if err != nil {
		if resp != nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close() // response body is closed after reporting the handshake failure
			if readErr != nil {
				return nil, fmt.Errorf("attach ws: %w (read handshake response: %v)", err, readErr)
			}
			return nil, fmt.Errorf("attach ws: %w (%s: %s)", err, resp.Status, body)
		}
		return nil, fmt.Errorf("attach ws: %w", err)
	}
	return conn, nil
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

func isTerminalRunStatus(st string) bool {
	switch strings.ToLower(st) {
	case "complete", "failed", "exited", "skipped", "timed_out", "resumed":
		return true
	default:
		return false
	}
}
