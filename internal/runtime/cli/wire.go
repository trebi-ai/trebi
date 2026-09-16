package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// maxFrameBytes bounds one NDJSON frame (giant tool payloads fit; a runaway
// line does not). Matches the SDK's 1 MiB default buffer.
const maxFrameBytes = 1 << 20

// wireAdapter is one vendor protocol codec. The session machinery owns the
// process, framing, and event fan-out; adapters own parse/encode.
type wireAdapter interface {
	// Handshake writes protocol initialization (control initialize, thread
	// setup) and returns any immediately-known events. It must not block on
	// vendor replies: the first prompt goes out immediately (Part C).
	Handshake(ctx context.Context, w *wireWriter) []Event
	// EncodePrompt renders one user turn as one frame.
	EncodePrompt(p Prompt) ([]byte, error)
	// Parse maps one stdout line to zero or more events.
	Parse(line []byte) []Event
	// EncodeDecision answers a permission request.
	EncodeDecision(id string, d Decision) ([]byte, error)
	// Exit maps process exit (after stdout EOF) to events. It is not called
	// when the session produced its own terminal result and exited cleanly.
	Exit(code int, err error, stderr string) []Event
}

// interruptEncoder is optional: a protocol-level interrupt frame.
type interruptEncoder interface {
	EncodeInterrupt() ([]byte, error)
}

// wireWriter is the adapter's handle on the child's stdin plus session id.
type wireWriter struct {
	s *wireSession
}

func (w *wireWriter) Write(b []byte) error { return w.s.write(b) }

// SetSessionID records the vendor session id learned on the wire.
func (w *wireWriter) SetSessionID(id string) {
	if id != "" {
		w.s.setID(id)
	}
}

// wireSession is a Session over a child process speaking NDJSON or JSON-RPC.
type wireSession struct {
	tool    string
	proc    *Proc
	stdin   io.Writer
	adapter wireAdapter
	events  chan Event

	writeMu sync.Mutex
	idMu    sync.Mutex
	id      string

	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
	closing   chan struct{}
}

// writerSetter is implemented by adapters that need the stdin writer before
// Handshake runs (every JSON-RPC driver).
type writerSetter interface {
	setWriter(*wireWriter)
}

// sessionPrompter is implemented by adapters whose prompt is a request with a
// response (Codex turn/start, turn/steer). They own ack handling and the
// busy-turn fallback instead of the fire-and-forget frame path.
type sessionPrompter interface {
	promptSession(ctx context.Context, s *wireSession, p Prompt) error
}

// setUpWire attaches the writer and hands the adapter its Handshake. It runs
// before the first prompt and must not be skipped.
func setUpWire(ctx context.Context, s *wireSession, adapter wireAdapter) []Event {
	w := &wireWriter{s: s}
	if ws, ok := adapter.(writerSetter); ok {
		ws.setWriter(w)
	}
	return adapter.Handshake(ctx, w)
}

// startWire spawns the child and starts the framing loop. handshake=false
// defers adapter.Handshake to the caller (FakeDriver drives its own script).
func startWire(ctx context.Context, tool string, proc *Proc, adapter wireAdapter) *wireSession {
	s := &wireSession{
		tool:    tool,
		proc:    proc,
		stdin:   proc.Stdin(),
		adapter: adapter,
		events:  make(chan Event, 256),
		done:    make(chan struct{}),
		closing: make(chan struct{}),
	}
	go s.readLoop()
	if adapter != nil {
		if evs := setUpWire(ctx, s, adapter); len(evs) > 0 {
			for _, e := range evs {
				s.emit(e)
			}
		}
	}
	return s
}

// newTestWire builds an in-memory wire session for driver tests: stdin is the
// writer the driver uses, stdout the reader the peer writes responses into.
func newTestWire(tool string, adapter wireAdapter, stdin io.Writer, stdout io.Reader, ctx context.Context) *wireSession {
	s := &wireSession{
		tool:    tool,
		stdin:   stdin,
		adapter: adapter,
		events:  make(chan Event, 256),
		done:    make(chan struct{}),
		closing: make(chan struct{}),
	}
	go func() {
		s.scan(stdout)
		close(s.events)
		close(s.done)
	}()
	if adapter != nil {
		for _, e := range setUpWire(ctx, s, adapter) {
			s.emit(e)
		}
	}
	return s
}

func (s *wireSession) Provider() string { return s.tool }

func (s *wireSession) ID() string {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	return s.id
}

func (s *wireSession) setID(id string) {
	s.idMu.Lock()
	s.id = id
	s.idMu.Unlock()
}

func (s *wireSession) PID() int { return s.proc.PID() }

// Pgid is the child's process group (Unix).
func (s *wireSession) Pgid() int { return s.proc.Pgid() }

func (s *wireSession) Events() <-chan Event { return s.events }

func (s *wireSession) emit(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if e.SessionID == "" {
		e.SessionID = s.ID()
	} else {
		s.setID(e.SessionID)
	}
	select {
	case s.events <- e:
	case <-s.closing:
	}
}

func (s *wireSession) write(b []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.stdin == nil {
		return errNoStdin
	}
	if _, err := s.stdin.Write(b); err != nil {
		return err
	}
	return nil
}

var errNoStdin = errors.New("cli: session stdin is closed")

// Prompt writes one user turn. The child must still be alive.
func (s *wireSession) Prompt(ctx context.Context, p Prompt) error {
	if sp, ok := s.adapter.(sessionPrompter); ok {
		return sp.promptSession(ctx, s, p)
	}
	frame, err := s.adapter.EncodePrompt(p)
	if err != nil {
		return err
	}
	return s.writeFrame(frame)
}

func (s *wireSession) writeFrame(frame []byte) error {
	if frame == nil {
		return nil
	}
	// One NDJSON frame per write, newline-terminated.
	if len(frame) == 0 || frame[len(frame)-1] != '\n' {
		frame = append(frame, '\n')
	}
	return s.write(frame)
}

// AnswerPermission routes a decision back to the vendor.
func (s *wireSession) AnswerPermission(ctx context.Context, permissionID string, d Decision) error {
	frame, err := s.adapter.EncodeDecision(permissionID, d)
	if err != nil {
		return err
	}
	return s.writeFrame(frame)
}

// InterruptTurn sends the protocol interrupt when the adapter has one.
func (s *wireSession) InterruptTurn(ctx context.Context) error {
	if ie, ok := s.adapter.(interruptEncoder); ok {
		frame, err := ie.EncodeInterrupt()
		if err != nil {
			return err
		}
		return s.writeFrame(frame)
	}
	s.proc.SignalGroup(sigTerm())
	return nil
}

// Interrupt is the Session method: a protocol interrupt where available, a
// signal otherwise.
func (s *wireSession) Interrupt(ctx context.Context) error {
	if ie, ok := s.adapter.(interruptEncoder); ok {
		frame, err := ie.EncodeInterrupt()
		if err != nil {
			return err
		}
		return s.writeFrame(frame)
	}
	return s.InterruptTurn(ctx)
}

// Close runs the graceful sequence and waits for the event channel to drain.
func (s *wireSession) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.closeErr = s.proc.gracefulClose(gracefulCloseTimeout)
		close(s.closing)
	})
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	return s.closeErr
}

// KillTree escalates straight to SIGKILL on the process group.
func (s *wireSession) KillTree() error {
	err := s.proc.KillTree()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
	return err
}

// readLoop frames stdout, parses frames into events, and closes the channel
// after the process-exit events.
func (s *wireSession) readLoop() {
	s.scan(s.proc.Stdout())
	// stdout is exhausted: the child is exiting (or gone). Reap it, then let
	// the adapter classify the exit.
	err, code := s.proc.Wait(context.Background())
	stderr := s.proc.Stderr()
	for _, e := range s.adapter.Exit(code, err, stderr) {
		s.emit(e)
	}
	s.exitEvent(code, err, stderr)
	close(s.events)
	close(s.done)
}

// scan frames NDJSON lines into events until r ends.
func (s *wireSession) scan(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		evs := s.adapter.Parse(line)
		for i := range evs {
			if len(evs[i].Raw) == 0 {
				evs[i].Raw = append([]byte(nil), line...)
			}
			s.emit(evs[i])
		}
	}
}

// exitEvent always reports the process death after any classified events.
func (s *wireSession) exitEvent(code int, err error, stderr string) {
	ev := Event{Type: EventExit, ExitCode: &code}
	if err != nil {
		ev.Error = err.Error()
	} else if code != 0 {
		ev.Error = strings.TrimSpace(stderr)
	}
	s.emit(ev)
}

// jsonLine marshals v into one NDJSON frame.
func jsonLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode frame: %w", err)
	}
	return append(b, '\n'), nil
}
