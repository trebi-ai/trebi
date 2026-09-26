package assistant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/store"
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
	// MaxFileBytes caps the native file tools.
	MaxFileBytes int64
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
	if conv.Operator != "" {
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
		Text:           in.Text,
		ChannelType:    in.ChannelType,
		ChannelRef:     in.ChannelRef,
		IdempotencyKey: in.IdempotencyKey,
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

	text := in.Text
	s.mu.Lock()
	if s.pendingRecap != "" {
		text = s.pendingRecap + text
		s.pendingRecap = ""
	}
	// Record the prompt before the send: an early harness echo must pass the
	// drop filter in the pump, and the user event must precede every event of
	// this turn (plan 2026-09-26 A).
	s.lastPrompt = in.Text
	s.lastChannel = in.ChannelType
	s.headless = in.Headless
	s.mu.Unlock()
	s.appendUserEvent(rec.Prompts+1, in)

	perr := s.sess.Prompt(ctx, agentwire.Prompt{Text: text, Kind: "assistant"})
	if perr != nil {
		s.appendSendError(fmt.Sprintf("could not send the prompt: %v", perr))
		_, _ = m.store.Update(ctx, convID, func(r *Record) { r.Status = previous })
		return wire.PromptResult{}, fmt.Errorf("could not send the prompt: %w", perr)
	}

	// Reset the idle timer (plan D.4 step 7).
	s.resetIdleTimer()
	_, _ = m.store.Update(ctx, convID, func(r *Record) { r.Prompts++ })
	rec, _ = m.store.Get(ctx, convID)
	return wire.PromptResult{Message: convMsg(msg), Status: rec.Status}, nil
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
	out := wire.AskResult{Message: res.Message, Status: res.Status}
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
	_, err := m.store.Update(ctx, convID, func(r *Record) { r.Mode = mode })
	return err
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

// Shutdown closes every session. The daemon calls it on the way down.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.live))
	for _, s := range m.live {
		sessions = append(sessions, s)
	}
	m.live = map[string]*session{}
	m.mu.Unlock()
	for _, s := range sessions {
		_, _ = m.store.Update(ctx, s.convID, func(r *Record) { r.Status = StatusClosed })
		s.close(ctx)
	}
	m.cancel()
	return nil
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
	if d, err := m.rt.Detect(context.Background(), agentwire.Harness(harness)); err == nil {
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

// skillsFor renders the selected skills as agent-wire skills (plan A.9).
func (m *Manager) skillsFor(names []string) []agentwire.Skill {
	if len(names) == 0 || m.opts.SkillsDir == "" {
		return nil
	}
	var out []agentwire.Skill
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			continue
		}
		dir := filepath.Join(m.opts.SkillsDir, n)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			out = append(out, agentwire.Skill{Name: n, Dir: dir})
		}
	}
	return out
}

// envsetEnv expands one env set into process env (plan D.5).
func (m *Manager) envsetEnv(name string) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(name) == "" {
		return out
	}
	sets, err := config.LoadEnvSets()
	if err != nil {
		return out
	}
	raw, ok := sets.Get(name)
	if !ok {
		return out
	}
	expanded, _ := secrets.ExpandRefs(raw)
	for k, v := range expanded {
		out[k] = v
	}
	return out
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
		ChannelType: c.ChannelType, ChannelRef: c.ChannelRef, Text: c.Text,
		TurnIndex: c.TurnIndex, IdempotencyKey: c.IdempotencyKey, CreatedAt: c.CreatedAt,
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
