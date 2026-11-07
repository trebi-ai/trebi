package assistant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/files"
	harnesspkg "github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/visor"
)

// agentRuntime is the part of *agentwire.Runtime the manager uses, extended
// with the native driver through the harness router.
type agentRuntime interface {
	Detect(ctx context.Context, h agentwire.Harness) (agentwire.Detection, error)
	Start(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error)
	Resume(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error)
}

// conversationLog is the part of *conversations.Service the manager writes.
type conversationLog interface {
	Get(ctx context.Context, id string) (*conversations.Conversation, error)
	AppendUser(ctx context.Context, convID string, in conversations.UserTurn) (conversations.Message, bool, error)
	AppendAssistant(ctx context.Context, convID string, turn int, text string) (conversations.Message, error)
	AppendSystem(ctx context.Context, convID, text string) error
	Recap(ctx context.Context, id string, n int) string
	LastAssistantRow(ctx context.Context, convID string, turn int) (conversations.Message, error)
	LastUserMessage(ctx context.Context, convID string) (*conversations.Message, error)
	MessagesAfter(ctx context.Context, convID, afterID string, limit int) ([]conversations.Message, error)
	Delete(ctx context.Context, convID string) error
}

// memoryReader renders <workspace>/MEMORY.md for the instructions.
type memoryReader interface {
	Render(ctx context.Context, workspaceID string) (string, error)
}

// inboxOpener opens the headless permission timeout item.
type inboxOpener interface {
	OpenPermissionTimeout(ctx context.Context, workspaceID, conversationID, tool string) error
}

// Wire errors the API maps to HTTP statuses.
var (
	errValidation  = errors.New("validation")
	errForbidden   = errors.New("forbidden")
	errConflict    = errors.New("conflict")
	errUnavailable = errors.New("unavailable")
	errNoSnapshot  = errors.New("no snapshot")
	errNoLive      = errors.New("no live assistant session")
)

// Options carries the process-wide wiring the manager cannot reach itself.
type Options struct {
	// MCPURL is the daemon's own /mcp endpoint (URL form of the Trebi server).
	MCPURL string
	// TrebiExe is this binary, for the stdio bridge fallback.
	TrebiExe string
	// SkillsDir is home.SkillsDir().
	SkillsDir string
	// Skills adds the workspace skills to a session. Nil keeps the selected
	// global skills only.
	Skills skillResolver
	// MaxFileBytes caps the native file tools.
	MaxFileBytes int64
	// Host gives the agent-visor that runs the vendor harnesses, so a turn
	// survives a daemon restart. Nil runs them in this process.
	Host func(context.Context) (visor.Host, error)
	// Now is the clock; tests replace it.
	Now func() time.Time
	// Log receives lifecycle lines.
	Log *slog.Logger
}

// TurnEnd is the payload of one finished turn (plan D.9).
type TurnEnd struct {
	ConversationID string
	Turn           int
	Reply          conversations.Message // the last assistant row of the turn
	Channel        string                // channel_type of the user row that started the turn
}

// Manager owns the live assistant sessions of the daemon.
type Manager struct {
	rt       agentRuntime
	store    *Store
	conv     conversationLog
	settings *Resolver
	ws       OverrideStore
	memory   memoryReader
	inbox    inboxOpener
	opts     Options
	log      *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	live  map[string]*session // conversation id -> live session
	subs  map[string][]chan Event
	hooks []func(context.Context, TurnEnd)
}

// NewManager builds a session manager. rt is normally a *harnessRouter.
func NewManager(rt agentRuntime, st *Store, conv conversationLog, res *Resolver, ws OverrideStore, mem memoryReader, inbox inboxOpener, opts Options) *Manager {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		rt: rt, store: st, conv: conv, settings: res, ws: ws, memory: mem,
		inbox: inbox, opts: opts, log: log,
		ctx: ctx, cancel: cancel,
		live: map[string]*session{},
		subs: map[string][]chan Event{},
	}
}

// Prompt stores the user row and sends one turn. It starts or resumes the
// process when needed (plan D.4).
func (m *Manager) Prompt(ctx context.Context, convID string, in wire.PromptInput) (wire.PromptResult, error) {
	res, err := m.prompt(ctx, convID, in)
	if err != nil {
		return wire.PromptResult{}, err
	}
	return res, nil
}

func (m *Manager) prompt(ctx context.Context, convID string, in wire.PromptInput) (wire.PromptResult, error) {
	conv, err := m.conv.Get(ctx, convID)
	if err != nil {
		return wire.PromptResult{}, err
	}
	if conv.OperatorID != "" {
		return wire.PromptResult{}, fmt.Errorf("%w: agent conversations do not use the assistant", errValidation)
	}
	eff, err := m.settings.Effective(ctx, conv.Workspace)
	if err != nil {
		return wire.PromptResult{}, err
	}
	if eff.Disabled {
		return wire.PromptResult{}, fmt.Errorf("%w: the assistant is disabled in this workspace", errForbidden)
	}

	rec, err := m.store.Get(ctx, convID)
	if err != nil && !errors.Is(err, ErrNoSession) {
		return wire.PromptResult{}, err
	}

	// Store the user row first. On a duplicate idempotency key, send
	// nothing (plan D.4 steps 2-3).
	ut := conversations.UserTurn{
		Text: in.Text,
		Address: conversations.Address{
			Connector: in.Connector, Instance: in.Instance, Room: in.Room, Thread: in.Thread,
		},
		IdempotencyKey: in.IdempotencyKey,
		Attachments:    in.Attachments,
	}
	msg, dup, aerr := m.conv.AppendUser(ctx, convID, ut)
	if aerr != nil {
		return wire.PromptResult{}, aerr
	}
	if dup {
		return wire.PromptResult{Message: convMsg(msg), Duplicate: true, Status: rec.Status}, nil
	}

	m.mu.Lock()
	s := m.live[convID]
	m.mu.Unlock()
	// A prompt while a turn runs joins that turn or waits for its end. Its
	// user event follows the send, so the delivery is known.
	if s != nil && (rec.Status == StatusRunning || rec.Status == StatusWaiting) {
		return m.sendDuringTurn(ctx, s, msg, in)
	}
	if s == nil {
		s, err = m.openSession(ctx, conv, eff, in, rec)
		if err != nil {
			return wire.PromptResult{}, err
		}
	}

	// The status flips before the send: a fast permission event must not be
	// overwritten by the tail of this request (plan D.4 step 4).
	previous := rec.Status
	if previous == "" || previous == StatusClosed || previous == StatusError {
		previous = StatusIdle
	}
	_, _ = m.store.Update(ctx, convID, func(r *Record) { r.Status = StatusRunning })

	text := s.takePrompt(in)
	if rec.Status == StatusPaused {
		// A restart stopped the last turn; the agent must know.
		text = runs.RestartPrompt + "\n\n" + text
	}
	s.appendUserEvent(rec.Prompts+1, in, agentwire.DeliveryPrompted)

	delivery, perr := s.send(ctx, text, in.Attachments)
	if perr != nil {
		s.appendSendError(fmt.Sprintf("could not send the prompt: %v", perr))
		_, _ = m.store.Update(ctx, convID, func(r *Record) { r.Status = previous })
		return wire.PromptResult{}, fmt.Errorf("could not send the prompt: %w", perr)
	}

	// Reset the idle timer (plan D.4 step 7).
	s.resetIdleTimer()
	if delivery == agentwire.DeliveryPrompted {
		s.armStart()
	}
	_, _ = m.store.Update(ctx, convID, func(r *Record) { r.Prompts++ })
	rec, _ = m.store.Get(ctx, convID)
	return wire.PromptResult{Message: convMsg(msg), Status: rec.Status, Delivery: string(delivery)}, nil
}

// openSession starts a session for the conversation, evicting an idle one
// when max_live is reached (plan D.5).
func (m *Manager) openSession(ctx context.Context, conv *conversations.Conversation, eff Effective, in wire.PromptInput, rec Record) (*session, error) {
	harness := eff.Resolved
	mode := eff.Mode
	fixed := rec.ConversationID != ""
	if fixed {
		// After the first turn the harness is fixed for the conversation
		// (plan D.5 step 1). An empty pin inherits the resolved harness.
		if rec.Harness != "" {
			harness = rec.Harness
		}
		if rec.Mode != "" {
			mode = rec.Mode
		}
	}
	if !fixed {
		if in.Harness != "" {
			if !wire.KnownHarness(in.Harness) {
				return nil, fmt.Errorf("%w: unknown harness %q", errValidation, in.Harness)
			}
			harness = in.Harness
		}
		if in.Mode != "" {
			mode = in.Mode
		}
	}
	if harness == "" {
		return nil, fmt.Errorf("%w: no supported harness and no native model/key", errUnavailable)
	}
	if err := checkApprovalMode(harness, mode); err != nil {
		return nil, err
	}

	if err := m.evictForCap(ctx); err != nil {
		return nil, err
	}

	if rec.ConversationID == "" {
		err := m.store.Create(ctx, Record{
			ConversationID: conv.ID,
			WorkspaceID:    conv.Workspace,
			Harness:        harness,
			Model:          eff.Model,
			Effort:         eff.Effort,
			Mode:           mode,
			VendorID:       rec.VendorID,
			Status:         StatusStarting,
		})
		if err != nil {
			return nil, err
		}
		rec, _ = m.store.Get(ctx, conv.ID)
	}
	// An empty pin inherits the resolved harness: write the resolution back,
	// so the session row and the drawer picker show what actually runs
	// (plan 2026-09-26 F.2; the API create writes mode-only pins).
	if fixed && rec.Harness != harness {
		if _, err := m.store.Update(ctx, conv.ID, func(r *Record) { r.Harness = harness }); err != nil {
			return nil, err
		}
		rec.Harness = harness
	}
	return m.startSession(ctx, conv, eff, harness, mode, rec.VendorID, in.Headless)
}

// evictForCap closes the least recently used idle session when max_live live
// processes are open. When none is idle it refuses (plan D.5 step 2).
func (m *Manager) evictForCap(ctx context.Context) error {
	cap := m.settings.Timing().MaxLive
	m.mu.Lock()
	liveCount := len(m.live)
	var oldestID string
	var oldestAt time.Time
	for id, s := range m.live {
		rec, err := m.store.Get(ctx, s.convID)
		if err == nil && rec.Status == StatusIdle {
			if oldestID == "" || rec.UpdatedAt.Before(oldestAt) {
				oldestID, oldestAt = id, rec.UpdatedAt
			}
		}
	}
	m.mu.Unlock()
	if liveCount < cap {
		return nil
	}
	if oldestID == "" {
		return fmt.Errorf("%w: too many live assistant sessions", errConflict)
	}
	_ = m.closeWith(ctx, oldestID, "evicted")
	return nil
}

// Ask is Prompt plus a wait for the turn result. Email, triggers, and agent
// escalation use it (plan D.3).
func (m *Manager) Ask(ctx context.Context, convID string, in wire.PromptInput, wait time.Duration) (wire.AskResult, error) {
	res, err := m.Prompt(ctx, convID, in)
	if err != nil {
		return wire.AskResult{}, err
	}
	out := wire.AskResult{Message: res.Message, Status: res.Status, Delivery: res.Delivery}
	if wait <= 0 {
		return out, nil
	}
	after := res.Message.ID
	deadline := m.now().Add(wait)
	for len(out.Replies) == 0 {
		replies, err := m.conv.MessagesAfter(ctx, convID, after, 50)
		if err == nil {
			for _, r := range replies {
				if r.Role == conversations.RoleAssistant {
					out.Replies = append(out.Replies, convMsg(r))
				}
			}
		}
		if len(out.Replies) > 0 || !m.now().Before(deadline) {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, nil
		case <-time.After(500 * time.Millisecond):
		}
	}
	return out, nil
}

// Interrupt stops the current turn (plan F.2). A session that is not running
// is a conflict.
func (m *Manager) Interrupt(ctx context.Context, convID string) error {
	s, err := m.liveSession(convID, StatusRunning)
	if err != nil {
		return err
	}
	if err := s.sess.Interrupt(ctx); err != nil {
		return fmt.Errorf("could not interrupt: %w", err)
	}
	return nil
}

// Answer routes a permission decision to the agent.
func (m *Manager) Answer(ctx context.Context, convID, permissionID string, d Decision) error {
	m.mu.Lock()
	s := m.live[convID]
	m.mu.Unlock()
	if s == nil {
		return errNoLive
	}
	return s.answer(ctx, permissionID, d.Choice, d.Message)
}

// SetMode applies the mode to the next process start (plan N: agent-wire
// v0.2.1 has no mode setter).
func (m *Manager) SetMode(ctx context.Context, convID, mode string) error {
	switch mode {
	case ModeAsk, ModeAutoEdit, ModeAuto:
	default:
		return fmt.Errorf("%w: invalid mode %q", errValidation, mode)
	}
	if rec, err := m.store.Get(ctx, convID); err == nil {
		if err := checkApprovalMode(rec.Harness, mode); err != nil {
			return err
		}
	}
	_, err := m.store.Update(ctx, convID, func(r *Record) { r.Mode = mode })
	return err
}

// checkApprovalMode refuses a mode that asks before a tool call on a harness
// that cannot ask.
func checkApprovalMode(harness, mode string) error {
	if harness == "" || mode == ModeAuto || config.HarnessAsksApproval(wire.BaseHarness(harness)) {
		return nil
	}
	label := harnesspkg.Label(harness)
	return fmt.Errorf("%w: %s cannot ask before it uses a tool. Choose Auto for this conversation", errValidation, label)
}

// Close ends the process of one conversation. The conversation stays.
func (m *Manager) Close(ctx context.Context, convID string) error {
	return m.closeWith(ctx, convID, "user")
}

func (m *Manager) closeWith(ctx context.Context, convID, reason string) error {
	m.mu.Lock()
	s := m.live[convID]
	delete(m.live, convID)
	m.mu.Unlock()
	if s == nil {
		// Not live: still mark the row closed so the drawer is honest.
		_, _ = m.store.Update(ctx, convID, func(r *Record) { r.Status = StatusClosed })
		return nil
	}
	s.close(ctx)
	_, _ = m.store.Update(ctx, convID, func(r *Record) {
		r.Status = StatusClosed
		r.ActorTokenHash = ""
	})
	m.logClosed(convID, reason)
	return nil
}

// Delete closes the process and deletes the conversation, its messages, its
// events, and its session row (plan F.2).
func (m *Manager) Delete(ctx context.Context, convID string) error {
	_ = m.closeWith(ctx, convID, "user")
	if err := m.store.DeleteSession(ctx, convID); err != nil && !errors.Is(err, ErrNoSession) {
		return err
	}
	if err := m.store.DeleteEvents(ctx, convID); err != nil {
		return err
	}
	return m.conv.Delete(ctx, convID)
}

// Revert restores one file to its content before a turn (plan D.6).
func (m *Manager) Revert(ctx context.Context, convID string, turn int, path string) (files.Info, error) {
	m.mu.Lock()
	s := m.live[convID]
	m.mu.Unlock()
	if s == nil {
		return files.Info{}, errNoSnapshot
	}
	info, err := s.snaps.Revert(ctx, turn, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return files.Info{}, errNoSnapshot
		}
		return files.Info{}, err
	}
	return info, nil
}

// Events replays stored events after afterSeq (plan F.2).
func (m *Manager) Events(ctx context.Context, convID string, afterSeq int64, limit int) ([]Event, error) {
	return m.store.Events(ctx, convID, afterSeq, limit)
}

// Subscribe registers a live event channel for one conversation. The cancel
// func unregisters the channel.
func (m *Manager) Subscribe(convID string) (<-chan Event, func()) {
	ch := make(chan Event, 256)
	m.mu.Lock()
	m.subs[convID] = append(m.subs[convID], ch)
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		list := m.subs[convID]
		for i, c := range list {
			if c == ch {
				m.subs[convID] = append(list[:i], list[i+1:]...)
				return
			}
		}
	}
}

// broadcast sends one event to the conversation's subscribers. Deltas carry
// Seq = 0 and are subscriber-only.
func (m *Manager) broadcast(convID string, e Event) {
	m.mu.Lock()
	subs := append([]chan Event(nil), m.subs[convID]...)
	m.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- e:
		default: // a slow follower drops the frame; the replay heals it
		}
	}
}

// Session returns one session record.
func (m *Manager) Session(ctx context.Context, convID string) (Record, error) {
	return m.store.Get(ctx, convID)
}

// Sessions lists the session records of one workspace.
func (m *Manager) Sessions(ctx context.Context, workspaceID string) ([]Record, error) {
	return m.store.List(ctx, workspaceID)
}

// TurnActive reports whether a turn is in flight on the conversation: the
// recorded status is starting, running, waiting on a permission, or paused
// until the resume after boot. A closed or idle record, or no record, is not
// active.
func (m *Manager) TurnActive(ctx context.Context, convID string) bool {
	rec, err := m.store.Get(ctx, convID)
	if err != nil {
		return false
	}
	switch rec.Status {
	case StatusStarting, StatusRunning, StatusWaiting, StatusPaused:
		return true
	default:
		return false
	}
}

// Live reports whether a process is attached to the conversation.
func (m *Manager) Live(convID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live[convID] != nil
}

// OnTurnEnd registers one turn-end hook (plan D.9).
func (m *Manager) OnTurnEnd(fn func(context.Context, TurnEnd)) {
	m.mu.Lock()
	m.hooks = append(m.hooks, fn)
	m.mu.Unlock()
}

func (m *Manager) fireTurnEnd(te TurnEnd) {
	m.mu.Lock()
	hooks := make([]func(context.Context, TurnEnd), len(m.hooks))
	copy(hooks, m.hooks)
	m.mu.Unlock()
	for _, fn := range hooks {
		fn(m.ctx, te)
	}
}

// Shutdown ends every process. A session in a turn pauses: the resume after
// boot continues the turn. Other sessions close.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.live))
	for _, s := range m.live {
		sessions = append(sessions, s)
	}
	m.live = map[string]*session{}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.stopSession(ctx, s)
		}()
	}
	wg.Wait()
	m.cancel()
	return nil
}

// ResumePaused continues each turn that a daemon stop paused. The daemon
// calls it once after boot. It stores no user row: the reply covers the
// prompt that started the turn, on the same channel.
func (m *Manager) ResumePaused(ctx context.Context) {
	recs, err := m.store.Paused(ctx)
	if err != nil {
		m.log.Warn("assistant resume paused", "err", err)
		return
	}
	for _, rec := range recs {
		if ctx.Err() != nil {
			return
		}
		if err := m.resumePaused(ctx, rec); err != nil {
			m.log.Warn("assistant resume paused", "conversation_id", rec.ConversationID, "err", err)
			_, _ = m.store.Update(ctx, rec.ConversationID, func(r *Record) {
				if r.Status == StatusPaused {
					r.Status = StatusError
					r.Error = "The assistant could not continue after the restart. Send your message again."
				}
			})
		}
	}
}

// PendingPermissions lists the open permission ids of one conversation.
func (m *Manager) PendingPermissions(convID string) map[string]string {
	m.mu.Lock()
	s := m.live[convID]
	m.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.pendingPermissions()
}

// --- internal helpers ---

// liveSession returns the live session and checks one status.
func (m *Manager) liveSession(convID string, want string) (*session, error) {
	m.mu.Lock()
	s := m.live[convID]
	m.mu.Unlock()
	if s == nil {
		return nil, errNoLive
	}
	if want != "" {
		rec, err := m.store.Get(context.Background(), convID)
		if err != nil {
			return nil, err
		}
		if rec.Status != want {
			return nil, fmt.Errorf("%w: session is %s, not %s", errConflict, rec.Status, want)
		}
	}
	return s, nil
}

// processEnded drops a process that ended by itself (exit event).
func (m *Manager) processEnded(convID, reason string) {
	m.mu.Lock()
	_, was := m.live[convID]
	delete(m.live, convID)
	m.mu.Unlock()
	if !was {
		return
	}
	_, _ = m.store.Update(context.Background(), convID, func(r *Record) { r.ActorTokenHash = "" })
	m.logClosed(convID, reason)
}

func (m *Manager) logClosed(convID, reason string) {
	_, _ = m.st().Append(context.Background(), store.BusEvent{
		Topic: "assistant.session.closed", Entity: "conversation", EntityID: convID,
		Action: "closed", Attrs: map[string]string{"reason": reason},
	})
}

func (m *Manager) registered(convID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live[convID] != nil
}

func (m *Manager) logStore(ev store.BusEvent) {
	_, _ = m.st().Append(context.Background(), ev)
}

// st returns the raw store for the few direct statements.
func (m *Manager) st() *store.Store { return m.store.st }

func (m *Manager) idleClose() time.Duration {
	if m.settings != nil {
		return m.settings.Timing().IdleClose
	}
	return DefaultIdleClose
}

func (m *Manager) permissionTimeout() time.Duration {
	if m.settings != nil {
		return m.settings.Timing().PermissionTimeout
	}
	return DefaultPermissionTimeout
}

func (m *Manager) startTimeout() time.Duration {
	if m.settings != nil {
		return m.settings.Timing().StartTimeout
	}
	return DefaultStartTimeout
}

func (m *Manager) now() time.Time {
	if m.opts.Now != nil {
		return m.opts.Now()
	}
	return time.Now()
}

// SetMCPURL updates the /mcp endpoint new sessions receive. The server
// calls it once its listener is bound, so a late-bound port still injects
// a reachable URL.
func (m *Manager) SetMCPURL(u string) {
	m.mu.Lock()
	m.opts.MCPURL = u
	m.mu.Unlock()
}

func (m *Manager) mcpURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opts.MCPURL
}

// trebiMCP renders the Trebi MCP server for one session (plan D.8). A
// harness whose injection cannot take a URL server gets the stdio bridge.
func (m *Manager) trebiMCP(token, harness string) []agentwire.MCPServer {
	capability := agentwire.MCPNone
	if d, err := m.rt.Detect(context.Background(), agentwire.Harness(wire.BaseHarness(harness))); err == nil {
		capability = d.Features.MCPInject
	}
	if capability == agentwire.MCPFlags || capability == agentwire.MCPSession {
		return []agentwire.MCPServer{{
			Name:    "trebi",
			URL:     m.mcpURL(),
			Headers: map[string]string{"X-Trebi-Run-Token": token},
		}}
	}
	return []agentwire.MCPServer{{
		Name:    "trebi",
		Command: m.opts.TrebiExe,
		Args:    []string{"mcp", "serve"},
		Env:     map[string]string{"TREBI_RUN_TOKEN": token},
	}}
}

// skillResolver returns the effective skills of one scope.
type skillResolver interface {
	Resolve(ctx context.Context, rs skills.RunScope) ([]skills.Resolved, error)
}

// displayOf returns the draft without the context blocks.
func displayOf(in wire.PromptInput) string {
	if strings.TrimSpace(in.Display) != "" {
		return in.Display
	}
	return in.Text
}

func convMsg(c conversations.Message) wire.Message {
	return wire.Message{
		ID: c.ID, ConversationID: c.ConversationID, Role: c.Role,
		Connector: c.Connector, Instance: c.Instance, Room: c.Room, Thread: c.Thread, Text: c.Text,
		TurnIndex: c.TurnIndex, IdempotencyKey: c.IdempotencyKey, CreatedAt: c.CreatedAt,
		Attachments: c.Attachments,
	}
}

// Decision answers one permission request.
type Decision struct {
	Choice  string `json:"decision"` // allow_once|allow_session|deny
	Message string `json:"message,omitempty"`
}

// SessionInfo is the drawer's view of one session (plan F.2).
type SessionInfo struct {
	Harness     string    `json:"harness"`
	Model       string    `json:"model,omitempty"`
	Mode        string    `json:"mode"`
	Status      string    `json:"status"` // starting|running|waiting|idle|closed|error
	Error       string    `json:"error,omitempty"`
	Turn        int       `json:"turn"`
	Prompts     int       `json:"prompts"`
	LastMessage string    `json:"last_message,omitempty"`
	Usage       Usage     `json:"usage"`
	Live        bool      `json:"live"` // a process is attached now
	UpdatedAt   time.Time `json:"updated_at"`
}

// Info renders the SessionInfo of one record.
func (r Record) Info(live bool) SessionInfo {
	return SessionInfo{
		Harness: r.Harness, Model: r.Model, Mode: r.Mode, Status: r.Status,
		Error: r.Error, Turn: r.Turn, Prompts: r.Prompts, LastMessage: r.LastMessage,
		Usage: Usage{Input: r.TokensIn, Output: r.TokensOut, CacheRead: r.TokensCached, CostUSD: r.CostUSD},
		Live:  live, UpdatedAt: r.UpdatedAt,
	}
}

// PromptInput, PromptResult, and AskResult are the wire types re-exported
// for callers of the manager.
type (
	PromptInput  = wire.PromptInput
	PromptResult = wire.PromptResult
	AskResult    = wire.AskResult
)

// KnownHarness reports whether h names native or an agent-wire harness.
var KnownHarness = wire.KnownHarness

// Preview composes the instructions a new session in one workspace would
// start with: role, workspace, the effective context, and the memory
// render (plan F.1).
func (m *Manager) Preview(ctx context.Context, workspaceID string) (string, error) {
	eff, err := m.settings.Effective(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	ws, err := m.ws.WorkspaceInfo(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	mem, _ := m.memory.Render(ctx, workspaceID)
	return compose(eff.Settings, ws, mem), nil
}

// skillsFor renders the selected global skills and the skills of the
// workspace as agent-wire skills (plan A.9). A workspace skill wins over a
// selected global skill of the same name.
func (m *Manager) skillsFor(ctx context.Context, workspaceID string, names []string) []agentwire.Skill {
	var out []agentwire.Skill
	seen := map[string]bool{}
	if m.opts.Skills != nil && workspaceID != "" {
		list, err := m.opts.Skills.Resolve(ctx, skills.RunScope{WorkspaceID: workspaceID})
		if err != nil {
			m.log.Warn("resolve workspace skills", "workspace", workspaceID, "err", err)
		}
		for _, r := range list {
			if r.Used && r.Scope == skills.ScopeWorkspace {
				out = append(out, agentwire.Skill{Name: r.Name, Dir: r.Path})
				seen[r.Name] = true
			}
		}
	}
	if m.opts.SkillsDir == "" {
		return out
	}
	for _, n := range names {
		if strings.TrimSpace(n) == "" || seen[n] {
			continue
		}
		dir := filepath.Join(m.opts.SkillsDir, n)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			out = append(out, agentwire.Skill{Name: n, Dir: dir})
		}
	}
	return out
}

// sendDuringTurn delivers a prompt while a turn runs. A steered prompt joins
// the current turn. A held prompt starts the next turn when this one ends.
// Neither changes the status.
func (m *Manager) sendDuringTurn(ctx context.Context, s *session, msg conversations.Message, in wire.PromptInput) (wire.PromptResult, error) {
	text := s.takePrompt(in)
	delivery, err := s.send(ctx, text, in.Attachments)
	if err != nil {
		s.appendSendError(fmt.Sprintf("could not send the prompt: %v", err))
		return wire.PromptResult{}, fmt.Errorf("could not send the prompt: %w", err)
	}
	rec, _ := m.store.Get(ctx, s.convID)
	turn := rec.Turn
	switch delivery {
	case agentwire.DeliverySteered:
	case agentwire.DeliveryHeld:
		// Held prompts join into one followup turn.
		turn = rec.Turn + 1
		if !s.markHeld() {
			_, _ = m.store.Update(ctx, s.convID, func(r *Record) { r.Prompts++ })
		}
	default:
		turn = rec.Prompts + 1
		_, _ = m.store.Update(ctx, s.convID, func(r *Record) { r.Prompts++ })
	}
	s.appendUserEvent(turn, in, delivery)
	s.resetIdleTimer()
	rec, _ = m.store.Get(ctx, s.convID)
	return wire.PromptResult{Message: convMsg(msg), Status: rec.Status, Delivery: string(delivery)}, nil
}

func (m *Manager) stopSession(ctx context.Context, s *session) {
	rec, err := m.store.Get(ctx, s.convID)
	s.mu.Lock()
	vendor := s.vendorID
	s.mu.Unlock()
	visorID := s.sess.ID()
	if err != nil || (vendor == "" && visorID == "") || !inTurn(rec.Status) {
		// Close first: the reader records the exit of the session.
		s.close(ctx)
		_, _ = m.store.Update(ctx, s.convID, func(r *Record) { r.Status = StatusClosed })
		return
	}
	if visorID != "" {
		// The agent continues the turn in the visor, so it keeps its token.
		s.detach(ctx)
		if err := m.store.SetVisorSeq(ctx, s.convID, s.seq); err != nil {
			m.log.Warn("save the visor seq of a conversation", "conversation_id", s.convID, "err", err)
		}
		_, _ = m.store.Update(ctx, s.convID, func(r *Record) { r.Status = StatusPaused })
	} else {
		s.pause(ctx)
		_, _ = m.store.Update(ctx, s.convID, func(r *Record) {
			r.Status = StatusPaused
			r.ActorTokenHash = ""
		})
	}
	e := Event{Type: "system", Turn: rec.Turn, At: m.now().UTC(), Text: pausedNote}
	if seq, err := m.store.AppendEvent(ctx, s.convID, e); err == nil {
		e.Seq = seq
	}
	m.broadcast(s.convID, e)
	m.logClosed(s.convID, "restart")
}

// unreadPrompt returns the last prompt when the agent showed no work on it
// before the stop, so the resume sends it again.
func (m *Manager) unreadPrompt(ctx context.Context, convID string) string {
	last, ok, err := m.store.LastPromptEvent(ctx, convID)
	if err != nil || !ok {
		return ""
	}
	after, err := m.store.Events(ctx, convID, last.Seq, 500)
	if err != nil {
		return ""
	}
	for _, e := range after {
		switch e.Type {
		case "assistant", "tool", "permission":
			return ""
		}
	}
	if last.Prompt != "" {
		return last.Prompt
	}
	return last.Text
}

// attach follows the live visor session id of a paused conversation again,
// after the last event that the store has. The turn continues with no new
// prompt, and its reply arrives as usual.
func (m *Manager) attach(ctx context.Context, rec Record, id string, seq uint64) error {
	host, err := m.opts.Host(ctx)
	if err != nil {
		return err
	}
	vs, err := host.Session(ctx, id)
	if err != nil {
		return err
	}
	if !visor.Followable(vs.State) {
		return fmt.Errorf("visor session %s is %s", id, vs.State)
	}
	conv, err := m.conv.Get(ctx, rec.ConversationID)
	if err != nil {
		return err
	}
	eff, err := m.settings.Effective(ctx, conv.Workspace)
	if err != nil {
		return err
	}
	wsInfo, err := m.ws.WorkspaceInfo(ctx, conv.Workspace)
	if err != nil {
		return err
	}
	user, err := m.conv.LastUserMessage(ctx, rec.ConversationID)
	if err != nil {
		return err
	}
	headless := false
	s := m.newSession(conv, wsInfo.Path, eff, rec.Harness, rec.Mode, rec.VendorID, false)
	if user != nil {
		headless = user.Connector != conversations.ChannelUI && user.Connector != conversations.ChannelCLI
		s.lastChannel, s.headless = user.Connector, headless
	}
	s.wireTurn, s.seq = rec.Turn, seq
	sess, err := visor.Follow(ctx, host, vs, seq)
	if err != nil {
		s.cancel()
		return err
	}
	s.sess = sess
	status := StatusRunning
	for _, p := range vs.Pending {
		s.handlePermission(agentwire.Event{Type: agentwire.EventPermission, Permission: &agentwire.Permission{ID: p.ID, Tool: p.Tool}})
		status = StatusWaiting
	}
	_, _ = m.store.Update(ctx, rec.ConversationID, func(r *Record) { r.Status = status })
	m.mu.Lock()
	m.live[conv.ID] = s
	m.mu.Unlock()
	go s.pump()
	m.logStore(store.BusEvent{
		Topic: "assistant.session.started", Entity: "conversation", EntityID: conv.ID,
		Action: "started", Attrs: map[string]string{"harness": rec.Harness, "resumed": "true"},
	})
	m.log.Info("assistant attached after restart", "conversation_id", rec.ConversationID, "visor_session", id, "after", seq)
	return nil
}

const pausedNote = "Trebi restarted in the middle of this turn. The assistant continues it when Trebi starts again."

// Time limits of one paused turn at shutdown. They stay under the launchd
// ExitTimeOut (20 s).
const (
	pauseInterruptWait = 3 * time.Second
	pauseCloseWait     = 5 * time.Second
)

func inTurn(status string) bool {
	return status == StatusStarting || status == StatusRunning || status == StatusWaiting
}

func (m *Manager) resumePaused(ctx context.Context, rec Record) error {
	if m.Live(rec.ConversationID) {
		return nil
	}
	if id, seq, err := m.store.Visor(ctx, rec.ConversationID); err == nil && id != "" && m.opts.Host != nil {
		err := m.attach(ctx, rec, id, seq)
		if err == nil {
			return nil
		}
		m.log.Info("assistant session not attached", "conversation_id", rec.ConversationID, "visor_session", id, "reason", err)
		_ = m.store.SetVisor(ctx, rec.ConversationID, "", 0) // the resume below starts a new session
	}
	conv, err := m.conv.Get(ctx, rec.ConversationID)
	if err != nil {
		return err
	}
	eff, err := m.settings.Effective(ctx, conv.Workspace)
	if err != nil {
		return err
	}
	if eff.Disabled {
		_, _ = m.store.Update(ctx, rec.ConversationID, func(r *Record) { r.Status = StatusClosed })
		return nil
	}
	user, err := m.conv.LastUserMessage(ctx, rec.ConversationID)
	if err != nil {
		return err
	}
	in := wire.PromptInput{Text: runs.RestartPrompt}
	if user != nil {
		in.Connector, in.Instance, in.Room, in.Thread = user.Connector, user.Instance, user.Room, user.Thread
		// The original flag is not stored; only the drawer and the
		// command line have a person who answers permissions.
		in.Headless = user.Connector != conversations.ChannelUI && user.Connector != conversations.ChannelCLI
	}
	if p := m.unreadPrompt(ctx, rec.ConversationID); p != "" {
		in.Text = p
	}

	// A prompt in the meantime can resume the session first.
	cur, err := m.store.Get(ctx, rec.ConversationID)
	if err != nil || cur.Status != StatusPaused || m.Live(rec.ConversationID) {
		return err
	}
	s, err := m.openSession(ctx, conv, eff, in, cur)
	if err != nil {
		return err
	}
	_, _ = m.store.Update(ctx, rec.ConversationID, func(r *Record) { r.Status = StatusRunning })
	text := s.takePrompt(in)
	delivery, err := s.send(ctx, text, nil)
	if err != nil {
		s.appendSendError(fmt.Sprintf("could not continue after the restart: %v", err))
		_ = m.closeWith(ctx, rec.ConversationID, "error")
		return err
	}
	s.resetIdleTimer()
	if delivery == agentwire.DeliveryPrompted {
		s.armStart()
	}
	m.log.Info("assistant resumed after restart", "conversation_id", rec.ConversationID)
	return nil
}

// harnessEnv is the env of a harness (the base env, then the variant env),
// with the secret values read. It is never nil.
func (m *Manager) harnessEnv(name string) map[string]string {
	out := map[string]string{}
	cfg, err := config.Load()
	if err != nil {
		return out
	}
	maps.Copy(out, secrets.ResolveRefs(cfg.HarnessEnv(name)))
	return out
}
