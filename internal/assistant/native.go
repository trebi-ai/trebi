package assistant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/llm"
	"github.com/trebi-ai/trebi/internal/runtime/native"
	"github.com/trebi-ai/trebi/internal/tools"
)

// nativePrefix names the vendor id of a native session: the resume target is
// the persisted message log of the conversation.
const nativePrefix = "conv-"

// nativeRuntime is the agentRuntime driver for harness "native": the
// in-process loop behind agentwire.Session (plan 2026-09-26 D.11).
type NativeDriver struct {
	cfg          config.NativeConfig
	st           *Store
	disp         tools.Dispatcher
	maxFileBytes int64
	log          *slog.Logger

	mu   sync.Mutex
	prov llm.Provider
}

func NewNativeDriver(cfg config.NativeConfig, st *Store, disp tools.Dispatcher, maxFileBytes int64, log *slog.Logger) *NativeDriver {
	if log == nil {
		log = slog.Default()
	}
	return &NativeDriver{cfg: cfg, st: st, disp: disp, maxFileBytes: maxFileBytes, log: log}
}

// Detect reports native availability: a model set and a resolvable key.
func (n *NativeDriver) Detect(_ context.Context, h agentwire.Harness) (agentwire.Detection, error) {
	d := agentwire.Detection{Harness: h}
	if h != HarnessNative {
		return d, fmt.Errorf("native driver only serves harness %q", HarnessNative)
	}
	d.Version = "native"
	if strings.TrimSpace(n.cfg.Model) == "" {
		d.Auth = agentwire.AuthMissing
		d.Detail = "assistant.native.model is not set"
		return d, nil
	}
	if _, _, err := n.cfg.ResolveKey(); err != nil {
		d.Auth = agentwire.AuthMissing
		d.Detail = "no API key for the native model"
		return d, nil
	}
	d.Supported = true
	d.Auth = agentwire.AuthOK
	return d, nil
}

func (n *NativeDriver) provider() (llm.Provider, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.prov != nil {
		return n.prov, nil
	}
	prov, err := llm.New(n.cfg)
	if err != nil {
		return nil, err
	}
	n.prov = prov
	return prov, nil
}

// Start opens one native session. req.SessionID carries "conv-<id>".
func (n *NativeDriver) Start(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	return n.open(ctx, req, false)
}

// Resume reloads the message log of a native session. A missing log starts
// fresh; the manager sends a recap for the lost context.
func (n *NativeDriver) Resume(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	return n.open(ctx, req, true)
}

func (n *NativeDriver) open(ctx context.Context, req agentwire.StartRequest, resume bool) (agentwire.Session, error) {
	if req.SessionID == "" {
		return nil, errors.New("native session needs a session id (conv-<conversation id>)")
	}
	prov, err := n.provider()
	if err != nil {
		return nil, err
	}
	var reg *tools.Registry
	if n.disp != nil {
		reg = tools.NewNativeRegistry(n.disp)
	}
	model := req.Model
	if strings.TrimSpace(model) == "" {
		model = n.cfg.Model
	}
	daily := n.cfg.Limits.DailyTokens
	checkDaily := func(ctx context.Context, add int64) (bool, error) {
		if daily <= 0 || n.st == nil {
			return true, nil
		}
		today, err := n.st.TokensToday(ctx, HarnessNative)
		if err != nil {
			return true, nil // the cap never blocks on its own bookkeeping
		}
		return today+add <= int64(daily), nil
	}
	runOpts := native.RunOpts{
		Provider:      prov,
		RunID:         req.SessionID,
		WorkspaceRoot: req.WorkingDir,
		WorkingDir:    req.WorkingDir,
		System:        req.Instructions,
		Model:         model,
		MaxTokens:     n.cfg.MaxTokensOrDefault(),
		Headers:       n.cfg.Headers,
		MaxTurns:      n.cfg.Limits.MaxTurns,
		ContextTokens: n.cfg.ContextTokens,
		DailyLimit:    daily,
		MaxFileBytes:  n.maxFileBytes,
		Registry:      reg,
		CheckDaily:    checkDaily,
		Log:           n.log,
	}
	tr, err := native.OpenTranscript(req.SessionID)
	if err == nil {
		runOpts.Transcript = tr
	}
	s := &nativeSession{
		loop:     native.NewSession(runOpts),
		vendorID: req.SessionID,
		events:   make(chan agentwire.Event, 256),
		done:     make(chan struct{}),
	}
	s.loop.SetSink(native.Sink{
		AssistantText: func(text string) {
			if strings.TrimSpace(text) == "" {
				return
			}
			s.emit(agentwire.Event{Type: agentwire.EventAssistant, Text: text, At: time.Now().UTC()})
		},
		Usage: func(u llm.Usage) {
			s.emit(agentwire.Event{Type: agentwire.EventUsage, Usage: &agentwire.Usage{
				Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheCreation: u.CacheCreation,
			}, At: time.Now().UTC()})
		},
		ToolEvent: func(phase string, c llm.ToolCall, output string) {
			ev := agentwire.Event{Type: agentwire.EventTool, At: time.Now().UTC(),
				Tool: &agentwire.ToolEvent{ID: c.ID, Name: c.Name, Status: phase}}
			if phase == "completed" {
				ev.Tool.Output = output
			}
			s.emit(ev)
		},
	})
	if resume {
		if msgs, lerr := native.LoadMessages(req.SessionID); lerr == nil {
			s.loop.SeedMessages(msgs)
		}
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	go s.drain()
	return s, nil
}

// nativeSession adapts the native loop to agentwire.Session. The manager
// pump reads Events until Close.
type nativeSession struct {
	loop      *native.Session
	vendorID  string
	events    chan agentwire.Event
	done      chan struct{}
	closeOnce sync.Once

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	err      error
	busy     bool
	turnNo   int
	turnStop context.CancelFunc
}

// Prompt runs one turn. A prompt while a turn runs fails: the manager gates
// prompts on status, and a turn that never ends is an interrupt case.
func (s *nativeSession) Prompt(ctx context.Context, p agentwire.Prompt) error {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return errors.New("native session is still running the previous turn")
	}
	s.busy = true
	s.turnNo++
	turn := s.turnNo
	turnCtx, stop := context.WithCancel(s.ctx)
	s.turnStop = stop
	s.mu.Unlock()

	s.emit(agentwire.Event{Type: agentwire.EventStatus, Status: agentwire.StatusRunning, Turn: turn, At: time.Now().UTC()})

	err := s.loop.Prompt(turnCtx, p.Text)

	s.mu.Lock()
	s.busy = false
	s.turnStop = nil
	s.mu.Unlock()
	stop()

	now := time.Now().UTC()
	switch {
	case errors.Is(err, context.Canceled):
		s.emit(agentwire.Event{Type: agentwire.EventResult,
			Result: &agentwire.Result{Subtype: "interrupted", IsError: true, EndReason: "interrupted"}, Turn: turn, At: now})
	case err != nil:
		s.setErr(err)
		s.emit(agentwire.Event{Type: agentwire.EventError, Error: err.Error(), Code: "native_turn_failed", Turn: turn, At: now})
		s.emit(agentwire.Event{Type: agentwire.EventResult,
			Result: &agentwire.Result{Subtype: "error", IsError: true, EndReason: "error"}, Turn: turn, At: now})
	default:
		s.emit(agentwire.Event{Type: agentwire.EventResult, Result: &agentwire.Result{Subtype: "success"}, Turn: turn, At: now})
	}
	s.emit(agentwire.Event{Type: agentwire.EventStatus, Status: agentwire.StatusIdle, Turn: turn, At: now})
	return nil
}

// Interrupt cancels the running turn. The turn then reports an interrupted
// result.
func (s *nativeSession) Interrupt(_ context.Context) error {
	s.mu.Lock()
	stop := s.turnStop
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	return nil
}

// AnswerPermission is a no-op: the native loop never asks.
func (s *nativeSession) AnswerPermission(_ context.Context, _ string, _ agentwire.Decision) error {
	return nil
}

// InterruptTurn is the explicit protocol interrupt. It is Interrupt.
func (s *nativeSession) InterruptTurn(ctx context.Context) error { return s.Interrupt(ctx) }

// Provider is the harness name.
func (s *nativeSession) Provider() string { return HarnessNative }

// ID is the vendor session id ("conv-<conversation id>").
func (s *nativeSession) ID() string { return s.vendorID }

// PID is 0: the loop is in-process.
func (s *nativeSession) PID() int { return 0 }

// Done closes when the session has ended and Events is drained.
func (s *nativeSession) Done() <-chan struct{} { return s.done }

// Err is the first failure the session recorded, if any.
func (s *nativeSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close cancels any turn and ends the session.
func (s *nativeSession) Close(_ context.Context) error {
	s.cancel()
	<-s.done
	return nil
}

// Events is closed after the terminal events.
func (s *nativeSession) Events() <-chan agentwire.Event { return s.events }

// drain closes done when the session context ends.
func (s *nativeSession) drain() {
	defer s.closeOnce.Do(func() { close(s.done) })
	<-s.ctx.Done()
}

// emit writes one event. A full buffer drops the event rather than blocking
// the turn: the pump reads continuously, so this is a broken-consumer sign.
func (s *nativeSession) emit(e agentwire.Event) {
	defer func() { _ = recover() }()
	s.events <- e
}

func (s *nativeSession) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// harnessRouter sends "native" to the in-process driver and every other
// harness to agent-wire (plan 2026-09-26 D.2).
type HarnessRouter struct {
	wire   *agentwire.Runtime
	native *NativeDriver
}

func (r *HarnessRouter) Detect(ctx context.Context, h agentwire.Harness) (agentwire.Detection, error) {
	if h == HarnessNative {
		return r.native.Detect(ctx, h)
	}
	return r.wire.Detect(ctx, h)
}

func (r *HarnessRouter) Start(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	if req.Harness == HarnessNative {
		return r.native.Start(ctx, req)
	}
	return r.wire.Start(ctx, req)
}

func (r *HarnessRouter) Resume(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	if req.Harness == HarnessNative {
		return r.native.Resume(ctx, req)
	}
	return r.wire.Resume(ctx, req)
}

var (
	_ agentRuntime      = (*HarnessRouter)(nil)
	_ Detector          = (*HarnessRouter)(nil)
	_ agentwire.Session = (*nativeSession)(nil)
)

// NewHarnessRouter builds the runtime router the manager consumes: native
// sessions stay in-process, every other harness goes to agent-wire.
func NewHarnessRouter(wire *agentwire.Runtime, native *NativeDriver) *HarnessRouter {
	return &HarnessRouter{wire: wire, native: native}
}
