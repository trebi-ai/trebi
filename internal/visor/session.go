// Package visor reads agent-visor sessions as agent-wire events, so that
// runs and the assistant keep their event pumps.
package visor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/agent-visor/holder"
	agentwire "github.com/trebi-ai/agent-wire"
)

// Host is the part of agent-visor that trebi uses. *agentvisor.Visor and
// *agentvisor.Client both have it.
type Host interface {
	Start(ctx context.Context, spec agentvisor.StartSpec) (agentvisor.Session, error)
	Session(ctx context.Context, id string) (agentvisor.Session, error)
	Send(ctx context.Context, id string, in agentvisor.Input) (agentvisor.Delivery, error)
	Interrupt(ctx context.Context, id string) error
	Answer(ctx context.Context, id, requestID string, a agentvisor.Answer) error
	Stop(ctx context.Context, id string) error
	Events(ctx context.Context, f agentvisor.Filter, after uint64) (*agentvisor.EventStream, error)
	Sessions(ctx context.Context, f agentvisor.Filter) ([]agentvisor.Session, error)
	Screen(ctx context.Context, id string) (agentvisor.Screen, error)
	Output(ctx context.Context, id string) (string, error)
	Keys(ctx context.Context, id string, data []byte) error
	Paste(ctx context.Context, id, text string) error
}

// Configure points the holder and hook commands of o at "trebi visor", so
// that a holder or a vendor hook runs this program.
func Configure(o *agentvisor.Options) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if len(o.HolderCommand) == 0 {
		o.HolderCommand = []string{exe, "visor", "hold"}
	}
	if len(o.HookCommand) == 0 {
		o.HookCommand = []string{exe, "visor", "--home", o.Home, "hook"}
	}
}

// Hold runs this process as a holder when args are "visor hold <session>".
// A test binary calls it in TestMain, because Configure points the holders
// of a visor in a test at the test binary.
func Hold(args []string) (int, bool) {
	if len(args) != 4 || args[1] != "visor" || args[2] != "hold" {
		return 0, false
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := holder.ReadConfig(os.Stdin, args[3])
	if err == nil {
		err = holder.Run(ctx, cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}
	return 0, true
}

// Followable reports if a session in state st can give more events. A
// suspended session starts again at the next send.
func Followable(st agentvisor.State) bool {
	return st.Live() || st == agentvisor.StateSuspended
}

// Event is one session event. Seq is the visor seq, or 0 for a session in
// this process and for the exit that a lost session gets.
type Event struct {
	Seq uint64
	agentwire.Event
}

// Session is one agent session: a visor session, or an agent-wire session in
// this process (the native harness).
type Session struct {
	host  Host
	local agentwire.Session
	id    string
	pid   int
	ch    chan Event
	// end is the last state of a visor session when its events end.
	end    atomic.Value
	err    atomic.Value
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// Start starts a visor session and follows its events.
func Start(ctx context.Context, host Host, spec agentvisor.StartSpec) (*Session, error) {
	sess, err := host.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	s, err := Follow(ctx, host, sess, 0)
	if err != nil {
		_ = host.Stop(context.WithoutCancel(ctx), sess.ID) // the follow error is the one to report
		return nil, err
	}
	return s, nil
}

// Follow reads the events of a visor session after seq after.
func Follow(ctx context.Context, host Host, sess agentvisor.Session, after uint64) (*Session, error) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	st, err := host.Events(ctx, agentvisor.Filter{Session: sess.ID}, after)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("read the events of session %s: %w", sess.ID, err)
	}
	s := &Session{host: host, id: sess.ID, pid: sess.PID, ch: make(chan Event, 64), cancel: cancel, done: make(chan struct{})}
	go s.pump(ctx, st)
	return s, nil
}

// Quiet gives session id of host with no events. A test uses it to see what
// a reader sends.
func Quiet(host Host, id string) *Session {
	return &Session{host: host, id: id, pid: 1, ch: make(chan Event), cancel: func() {}, done: make(chan struct{})}
}

// Local wraps an agent-wire session that runs in this process.
func Local(sess agentwire.Session) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{local: sess, pid: sess.PID(), ch: make(chan Event, 64), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer close(s.ch)
		for e := range sess.Events() {
			select {
			case s.ch <- Event{Event: e}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return s
}

// ID is the visor session id. It is empty for a session in this process.
func (s *Session) ID() string { return s.id }

func (s *Session) PID() int { return s.pid }

// VendorID is the vendor session id of a session in this process.
func (s *Session) VendorID() string {
	if s.local != nil {
		return s.local.ID()
	}
	return ""
}

func (s *Session) Events() <-chan Event { return s.ch }

// End is the last state of a visor session, or empty while it lives.
func (s *Session) End() agentvisor.State {
	st, _ := s.end.Load().(agentvisor.State)
	return st
}

// Err is the failure that ended the session, if any.
func (s *Session) Err() error {
	if s.local != nil {
		return s.local.Err()
	}
	err, _ := s.err.Load().(error)
	return err
}

// Send delivers one prompt: it steers the running turn, else prompts, else
// holds it.
func (s *Session) Send(ctx context.Context, p agentwire.Prompt) (agentwire.Delivery, error) {
	if s.local != nil {
		return agentwire.Send(ctx, s.local, p)
	}
	in := agentvisor.Input{Text: p.Text, Kind: p.Kind}
	for _, a := range p.Attachments {
		in.Attachments = append(in.Attachments, agentvisor.Attachment{MIME: a.MIME, Path: a.Path, Data: a.Data})
	}
	d, err := s.host.Send(ctx, s.id, in)
	if errors.Is(err, agentvisor.ErrUnsupported) {
		err = fmt.Errorf("%w: %w", agentwire.ErrUnsupported, err)
	}
	return agentwire.Delivery(d), err
}

func (s *Session) Interrupt(ctx context.Context) error {
	if s.local != nil {
		return s.local.Interrupt(ctx)
	}
	return s.host.Interrupt(ctx, s.id)
}

func (s *Session) AnswerPermission(ctx context.Context, id string, d agentwire.Decision) error {
	if s.local != nil {
		return s.local.AnswerPermission(ctx, id, d)
	}
	return s.host.Answer(ctx, s.id, id, agentvisor.Answer{Allow: d.Allow, Message: d.Message, UpdatedInput: d.UpdatedInput})
}

// Close stops the session. The events end at its final state.
func (s *Session) Close(ctx context.Context) error {
	var err error
	s.once.Do(func() {
		if s.local != nil {
			err = s.local.Close(ctx)
			return
		}
		if err = s.host.Stop(ctx, s.id); errors.Is(err, agentvisor.ErrNotFound) || errors.Is(err, agentvisor.ErrState) {
			err = nil
		}
	})
	return err
}

// Detach stops the events and leaves a visor session alive, so that the next
// daemon attaches again. A session in this process closes.
func (s *Session) Detach(ctx context.Context) {
	if s.local != nil {
		_ = s.Close(ctx) // a process session cannot outlive the daemon
	}
	s.cancel()
	<-s.done
}

// pump ends at the final state of the session, or when Detach cancels ctx.
// It reads on while the session is suspended.
func (s *Session) pump(ctx context.Context, st *agentvisor.EventStream) {
	defer close(s.done)
	defer close(s.ch)
	defer st.Close() //nolint:errcheck // Close of a stream always gives nil
	exited := false
	for {
		e, err := st.Next(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.end.Store(agentvisor.StateLost)
				s.lost(ctx, exited)
			}
			return
		}
		if e.Type == agentvisor.EventSessionState && !Followable(e.State) {
			s.end.Store(e.State)
			if e.State == agentvisor.StateLost {
				s.lost(ctx, exited)
			}
			return
		}
		// The visor writes a user event for each Send. The reader has its own.
		if e.Type == agentvisor.EventUser && e.Input {
			continue
		}
		w, ok := e.Wire()
		if !ok {
			continue
		}
		if w.Type == agentwire.EventExit {
			exited = true
			if e.Error != "" {
				s.err.Store(errors.New(e.Error))
			}
		}
		select {
		case s.ch <- Event{Seq: e.Seq, Event: w}:
		case <-ctx.Done():
			return
		}
	}
}

// lost ends the events of a session that the visor lost with an exit, so the
// reader ends the turn.
func (s *Session) lost(ctx context.Context, exited bool) {
	s.err.Store(errors.New("the agent session was lost"))
	if exited {
		return
	}
	select {
	case s.ch <- Event{Event: agentwire.Event{Type: agentwire.EventExit}}:
	case <-ctx.Done():
	}
}
