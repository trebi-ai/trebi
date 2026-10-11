package assistant

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/assistant/wire"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/store"
)

// nativePrefix names the vendor id of a native session: the resume target is
// the message log stored under conv-<conversation id>.
const nativePrefix = "conv-"

// session is one live assistant conversation: its agent process, its pump,
// its permissions, and its delta buffer (plan 2026-09-26 D.6).
type session struct {
	m      *Manager
	convID string
	wsID   string

	sess  agentwire.Session
	snaps *snapshots

	eff      Effective
	harness  string
	mode     string
	vendorID string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	idleTimer *time.Timer

	mu           sync.Mutex
	allow        []string            // session-scoped allowed tools
	permissions  map[string]*permAsk // permission id -> pending ask
	delta        strings.Builder     // open assistant message buffer
	deltaOpen    bool
	deltaTurn    int
	lastPrompt   string // full text of the prompt the session recorded
	lastChannel  string // channel_type of that prompt
	headless     bool
	pendingRecap string
	wireTurn     int  // newest turn number the agent process reported
	held         bool // a held prompt waits for the next turn
}

// permAsk is one pending permission request.
type permAsk struct {
	tool     string
	timer    *time.Timer
	headless bool
}

var (
	errPermissionUnknown = errors.New("unknown or answered permission")
	errDecisionInvalid   = errors.New("decision must be allow_once, allow_session, or deny")
)

// startSession builds the session around a live agent process and starts the
// pump. It stores the row with the minted token hash before the pump runs.
func (m *Manager) startSession(ctx context.Context, conv *conversations.Conversation, eff Effective, harness, mode, vendorID string, headless bool) (*session, error) {
	wsInfo, err := m.ws.WorkspaceInfo(ctx, conv.Workspace)
	if err != nil {
		return nil, err
	}
	token, tokenHash, err := MintSessionToken()
	if err != nil {
		return nil, err
	}

	s := &session{
		m:           m,
		convID:      conv.ID,
		wsID:        conv.Workspace,
		eff:         eff,
		harness:     harness,
		mode:        mode,
		vendorID:    vendorID,
		snaps:       newSnapshots(wsInfo.Path),
		done:        make(chan struct{}),
		permissions: map[string]*permAsk{},
		headless:    headless,
	}
	s.snaps.CurrentTurn = s.currentTurn
	s.ctx, s.cancel = context.WithCancel(m.ctx)

	memory, _ := m.memory.Render(s.ctx, conv.Workspace)
	req := agentwire.StartRequest{
		Harness:      agentwire.Harness(harness),
		WorkingDir:   wsInfo.Path,
		Model:        eff.Model,
		Effort:       eff.Effort,
		Env:          m.envsetEnv(eff.Envset),
		Permissions:  agentwire.PermissionPolicy{Mode: agentwire.PermissionMode(mode)},
		Skills:       m.skillsFor(eff.Skills),
		Instructions: compose(eff.Settings, wsInfo, memory),
		FS:           s.snaps,
	}
	if mode == ModeAutoEdit {
		req.Permissions.AllowTools = []string{"mcp__trebi__*"}
	}
	if harness == HarnessNative {
		// Native runs in-process: no run token, no MCP URL injection. The
		// trebi tool catalog reaches the loop through the driver's
		// ToolSetFuncs (plan 2026-09-26 I.7).
		req.SessionID = nativePrefix + conv.ID
	} else {
		req.Env["TREBI_RUN_TOKEN"] = token
		req.MCPServers = m.trebiMCP(token, harness)
		if vendorID != "" {
			req.SessionID = vendorID
		}
	}

	// Resume when a vendor session is stored. On a resume failure the
	// manager starts fresh and prefixes a recap (plan A.3, D.5).
	var sess agentwire.Session
	recap := false
	if vendorID != "" {
		sess, err = m.rt.Resume(ctx, req)
		if err != nil {
			if harness == HarnessNative {
				// Native resume starts fresh in the driver; treat it as
				// resumed-with-recap only when the message log was missing.
				sess, err = m.rt.Start(ctx, req)
				recap = true
			} else {
				req.SessionID = ""
				sess, err = m.rt.Start(ctx, req)
				recap = true
			}
		}
	} else {
		sess, err = m.rt.Start(ctx, req)
	}
	if err != nil {
		s.cancel()
		return nil, err
	}
	s.sess = sess
	if recap {
		if r := strings.TrimSpace(m.conv.Recap(ctx, conv.ID, 20)); r != "" {
			s.pendingRecap = "## Earlier in this conversation (summary)\n\n" + r + "\n\n## Now\n\n"
		}
		s.appendSystemEvent("Started a new agent session. The earlier context is a summary.")
	}

	if _, err := m.store.Update(ctx, conv.ID, func(r *Record) {
		r.VendorID = sess.ID()
		r.ActorTokenHash = tokenHash
		r.Status = StatusStarting
		r.Model = eff.Model
		r.Effort = eff.Effort
		r.Mode = mode
	}); err != nil {
		_ = sess.Close(ctx)
		s.cancel()
		return nil, err
	}
	m.logStore(store.BusEvent{
		Topic: "assistant.session.started", Entity: "conversation", EntityID: conv.ID,
		Action: "started", Attrs: map[string]string{"harness": harness, "resumed": boolStr(!recap && vendorID != "")},
	})

	m.mu.Lock()
	m.live[conv.ID] = s
	m.mu.Unlock()
	go s.pump()
	return s, nil
}

// pump reads agent events until the session closes and maps each one to a
// stored Event plus a subscriber broadcast (plan D.6).
func (s *session) pump() {
	defer close(s.done)
	defer func() {
		if s.m.registered(s.convID) {
			return // close/delete already wrote the terminal status
		}
	}()

	for evt := range s.sess.Events() {
		if evt.Type == agentwire.EventInit && evt.SessionID != "" {
			vendor := evt.SessionID
			s.mu.Lock()
			s.vendorID = vendor
			s.mu.Unlock()
			_, _ = s.m.store.Update(s.ctx, s.convID, func(r *Record) { r.VendorID = vendor })
		}
		if evt.Turn > 0 {
			turn := evt.Turn
			// A held prompt starts the next turn after a result, with no call
			// to Prompt, so the status flips back here.
			resumed := s.nextWireTurn(turn) && evt.Type != agentwire.EventResult && evt.Type != agentwire.EventError
			_, _ = s.m.store.Update(s.ctx, s.convID, func(r *Record) {
				if turn > r.Turn {
					r.Turn = turn
				}
				if resumed {
					r.Status = StatusRunning
				}
			})
		}

		// agent-wire does not echo prompts; drop one if a future version does.
		if evt.Type == agentwire.EventUser && s.isLastPrompt(evt.Text) {
			continue
		}

		if evt.Type == agentwire.EventTool && evt.Tool != nil && evt.Tool.Status == "started" {
			s.snaps.Capture(s.currentTurn(), evt.Tool.Paths)
		}

		if evt.Type == agentwire.EventPermission && evt.Permission != nil {
			if s.handlePermission(evt) {
				continue // auto-answered from the session allow list
			}
		}

		s.deliver(evt)
		if evt.Type == agentwire.EventResult || evt.Type == agentwire.EventExit {
			s.endTurn()
		}
	}
}

// deliver maps one agent event to the wire type, buffers deltas, stores the
// stored events, and broadcasts to subscribers.
func (s *session) deliver(evt agentwire.Event) {
	if evt.Type == agentwire.EventAssistant && evt.Delta {
		s.mu.Lock()
		s.delta.WriteString(evt.Text)
		if !s.deltaOpen {
			s.deltaOpen = true
			s.deltaTurn = s.currentTurn()
		}
		s.mu.Unlock()
		live := eventFrom(evt)
		live.Seq = 0
		live.Turn = s.currentTurn()
		s.m.broadcast(s.convID, live)
		return
	}
	if evt.Type == agentwire.EventAssistant {
		// A non-delta assistant text closes the buffer. A final non-delta
		// echo of the same text is dropped (plan D.6).
		var flushed string
		if s.deltaOpen {
			s.mu.Lock()
			flushed = s.delta.String()
			s.delta.Reset()
			s.deltaOpen = false
			s.mu.Unlock()
			s.persistAssistant(flushed, s.currentTurn())
		}
		if evt.Text == flushed {
			return
		}
		if strings.TrimSpace(evt.Text) != "" {
			s.persistAssistant(evt.Text, s.currentTurn())
		}
		return
	}

	e := eventFrom(evt)
	e.Turn = s.currentTurn()
	switch e.Type {
	case string(agentwire.EventUsage):
		if e.Usage != nil {
			u := *e.Usage
			_, _ = s.m.store.Update(s.ctx, s.convID, func(r *Record) {
				r.TokensIn += u.Input
				r.TokensOut += u.Output
				r.TokensCached += u.CacheRead + u.CacheCreation
				r.CostUSD += u.CostUSD
			})
		}
	case string(agentwire.EventResult):
		s.setStatus(StatusIdle)
	case string(agentwire.EventStatus):
		if evt.Status == agentwire.StatusIdle {
			s.setStatus(StatusIdle)
		}
	case string(agentwire.EventExit):
		if err := s.sess.Err(); err != nil {
			_, _ = s.m.store.Update(s.ctx, s.convID, func(r *Record) {
				r.Status = StatusError
				r.Error = err.Error()
			})
		} else {
			s.setStatus(StatusClosed)
		}
	case string(agentwire.EventError):
		if e.Error != nil && e.Error.Code == agentwire.CodeHeldPromptDropped {
			e.Error.Message = "The assistant stopped before it read your queued message. Send it again."
		}
		if e.Error != nil && e.Error.Message != "" {
			_, _ = s.m.store.Update(s.ctx, s.convID, func(r *Record) { r.Error = e.Error.Message })
		}
	case string(agentwire.EventUser):
		e.Channel = s.lastChannel
	}

	if seq, err := s.m.store.AppendEvent(s.ctx, s.convID, e); err == nil {
		e.Seq = seq
	}
	s.m.broadcast(s.convID, e)
	s.observe(e)

	if e.Type == string(agentwire.EventExit) {
		reason := "error"
		if s.sess.Err() == nil {
			reason = "closed"
		}
		s.m.processEnded(s.convID, reason)
	}
}

// persistAssistant stores one closed assistant text block, appends the
// conversation row, and keeps the list fields current.
func (s *session) persistAssistant(text string, turn int) {
	if strings.TrimSpace(text) == "" {
		return
	}
	e := Event{Type: "assistant", Turn: turn, At: time.Now().UTC(), Text: text}
	if seq, err := s.m.store.AppendEvent(s.ctx, s.convID, e); err == nil {
		e.Seq = seq
	}
	s.m.broadcast(s.convID, e)
	s.observe(e)
	if _, err := s.m.conv.AppendAssistant(s.ctx, s.convID, turn, text); err == nil {
		_, _ = s.m.store.Update(s.ctx, s.convID, func(r *Record) { r.LastMessage = truncateRunes(text, 140) })
	}
}

// endTurn closes the delta buffer and fires the turn-end hooks (plan D.9).
func (s *session) endTurn() {
	s.mu.Lock()
	if s.deltaOpen {
		text := s.delta.String()
		turn := s.deltaTurn
		s.delta.Reset()
		s.deltaOpen = false
		s.mu.Unlock()
		s.persistAssistant(text, turn)
	} else {
		s.mu.Unlock()
	}

	s.resetIdleTimer()
	channel := s.lastChannel
	turn := s.currentTurn()
	reply, _ := s.m.conv.LastAssistantRow(s.ctx, s.convID, turn)
	s.m.fireTurnEnd(TurnEnd{
		ConversationID: s.convID,
		Turn:           turn,
		Reply:          reply,
		Channel:        channel,
	})
}

// handlePermission answers from the session allow list at once; otherwise it
// marks waiting and starts the headless timer. It reports whether the event
// was consumed.
func (s *session) handlePermission(evt agentwire.Event) bool {
	p := evt.Permission
	if p == nil || p.ID == "" {
		return false
	}
	if s.allows(p.Tool) {
		err := s.sess.AnswerPermission(s.ctx, p.ID, agentwire.Decision{Allow: true})
		if err == nil {
			s.mu.Lock()
			delete(s.permissions, p.ID)
			s.mu.Unlock()
			s.storeAnswer(Answer{PermissionID: p.ID, Decision: "allow_session", By: "allow_list"})
			return true
		}
		// fall through: a failed auto-answer becomes a visible request
	}
	headless := s.headless
	s.mu.Lock()
	ask := &permAsk{tool: p.Tool, headless: headless}
	if headless {
		ask.timer = time.AfterFunc(s.m.permissionTimeout(), func() {
			s.timeoutPermission(p.ID)
		})
	}
	s.permissions[p.ID] = ask
	s.mu.Unlock()
	s.setStatus(StatusWaiting)
	return false
}

// timeoutPermission denies one pending permission and opens an inbox item
// (plan A.14).
func (s *session) timeoutPermission(permissionID string) {
	s.mu.Lock()
	ask, ok := s.permissions[permissionID]
	tool := ""
	if ok {
		tool = ask.tool
		delete(s.permissions, permissionID)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = s.sess.AnswerPermission(s.ctx, permissionID, agentwire.Decision{Allow: false, Message: "no operator replied in time"})
	s.storeAnswer(Answer{PermissionID: permissionID, Decision: "deny", By: "timeout"})
	s.setStatus(StatusIdle)
	s.m.logStore(store.BusEvent{
		Topic: "assistant.permission.timeout", Entity: "conversation", EntityID: s.convID,
		Action: "timeout", Attrs: map[string]string{"tool": tool},
	})
	if s.m.inbox != nil {
		_ = s.m.inbox.OpenPermissionTimeout(s.ctx, s.wsID, s.convID, tool)
	}
}

// answer routes a decision to the agent and stores the answer event.
func (s *session) answer(ctx context.Context, permissionID, decision, message string) error {
	s.mu.Lock()
	ask, ok := s.permissions[permissionID]
	s.mu.Unlock()
	if !ok {
		return errPermissionUnknown
	}
	d := agentwire.Decision{Message: message}
	allowSession := false
	switch decision {
	case "allow_once":
		d.Allow = true
	case "allow_session":
		d.Allow = true
		allowSession = true
	case "deny":
	default:
		return errDecisionInvalid
	}
	if err := s.sess.AnswerPermission(ctx, permissionID, d); err != nil {
		return err
	}
	if allowSession {
		s.allowTool(ask.tool)
	}
	if ask.timer != nil {
		ask.timer.Stop()
	}
	s.mu.Lock()
	delete(s.permissions, permissionID)
	s.mu.Unlock()
	s.setStatus(StatusRunning)
	s.storeAnswer(Answer{PermissionID: permissionID, Decision: decision, By: "user"})
	return nil
}

func (s *session) storeAnswer(a Answer) {
	e := Event{Type: "answer", Turn: s.currentTurn(), At: time.Now().UTC(), Answer: &a}
	if seq, err := s.m.store.AppendEvent(s.ctx, s.convID, e); err == nil {
		e.Seq = seq
	}
	s.m.broadcast(s.convID, e)
}

// pendingPermissions lists the open permission ids.
func (s *session) pendingPermissions() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for id, ask := range s.permissions {
		out[id] = ask.tool
	}
	return out
}

// allowTool remembers a tool for the rest of the session.
func (s *session) allowTool(tool string) {
	if tool == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.allow {
		if existing == tool {
			return
		}
	}
	s.allow = append(s.allow, tool)
}

// allows reports whether the session answers this tool itself.
func (s *session) allows(tool string) bool {
	if tool == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pattern := range s.allow {
		if matchToolName(pattern, tool) {
			return true
		}
	}
	return false
}

// matchToolName matches a tool name against a session allow entry, where the
// entry may end in "*".
func matchToolName(pattern, tool string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(tool, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == tool
}

// setStatus writes the status row and emits conversation.updated through the
// store.
func (s *session) setStatus(status string) {
	_, _ = s.m.store.Update(s.ctx, s.convID, func(r *Record) { r.Status = status })
}

func (s *session) currentTurn() int {
	rec, err := s.m.store.Get(context.Background(), s.convID)
	if err != nil {
		return 0
	}
	return rec.Turn
}

// observe keeps the list fields of the session row current.
func (s *session) observe(e Event) {
	if e.Type != "assistant" || e.Delta || e.Text == "" {
		return
	}
	text := e.Text
	_, _ = s.m.store.Update(s.ctx, s.convID, func(r *Record) { r.LastMessage = truncateRunes(text, 140) })
}

// nextWireTurn records turn and reports whether it starts the turn of a held
// prompt.
func (s *session) nextWireTurn(turn int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if turn <= s.wireTurn {
		return false
	}
	s.wireTurn = turn
	held := s.held
	s.held = false
	return held
}

// markHeld marks a held prompt and reports whether one already waited.
func (s *session) markHeld() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.held
	s.held = true
	return was
}

// takePrompt records in as the last prompt and returns the text to send,
// with a pending recap in front. Call it before the send: an early harness
// echo must pass the drop filter in the pump (plan 2026-09-26 A).
func (s *session) takePrompt(in wire.PromptInput) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	text := in.Text
	if s.pendingRecap != "" {
		text = s.pendingRecap + text
		s.pendingRecap = ""
	}
	s.lastPrompt = in.Text
	s.lastChannel = in.Connector
	s.headless = in.Headless
	return text
}

func (s *session) isLastPrompt(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPrompt != "" && text == s.lastPrompt
}

func (s *session) appendSystemEvent(text string) {
	e := Event{Type: "system", Turn: s.currentTurn(), At: time.Now().UTC(), Text: text}
	if seq, err := s.m.store.AppendEvent(s.ctx, s.convID, e); err == nil {
		e.Seq = seq
	}
	s.m.broadcast(s.convID, e)
	_ = s.m.conv.AppendSystem(s.ctx, s.convID, text)
}

// appendUserEvent records the prompt as a `user` event. A new turn records
// it before the harness sees it (plan 2026-09-26 A). The drawer builds its transcript from events,
// so the prompt reaches it only through this event.
func (s *session) appendUserEvent(turn int, in wire.PromptInput, d agentwire.Delivery) {
	e := Event{
		Type:     "user",
		Turn:     turn,
		At:       time.Now().UTC(),
		Text:     displayOf(in),
		Prompt:   in.Text,
		Context:  in.Context,
		Channel:  in.Connector,
		Delivery: string(d),
	}
	if seq, err := s.m.store.AppendEvent(s.ctx, s.convID, e); err == nil {
		e.Seq = seq
	}
	s.m.broadcast(s.convID, e)
}

// appendSendError records a daemon-side send failure as an `error` event. The
// `user` event stays, so the prompt still shows.
func (s *session) appendSendError(text string) {
	e := Event{
		Type:  "error",
		Turn:  s.currentTurn(),
		At:    time.Now().UTC(),
		Error: &Error{Class: "send", Message: text},
	}
	if seq, err := s.m.store.AppendEvent(s.ctx, s.convID, e); err == nil {
		e.Seq = seq
	}
	s.m.broadcast(s.convID, e)
}

// resetIdleTimer arms the idle close timer (plan A.3).
func (s *session) resetIdleTimer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	d := s.m.idleClose()
	s.idleTimer = time.AfterFunc(d, func() {
		_ = s.m.closeWith(context.Background(), s.convID, "idle")
	})
}

// stopIdleTimer disarms the idle close timer.
func (s *session) stopIdleTimer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

// close ends the agent process and cancels the session context.
func (s *session) close(ctx context.Context) {
	s.stopIdleTimer()
	s.mu.Lock()
	asks := s.permissions
	s.permissions = map[string]*permAsk{}
	s.mu.Unlock()
	for _, ask := range asks {
		if ask.timer != nil {
			ask.timer.Stop()
		}
	}
	if s.sess != nil {
		_ = s.sess.Close(ctx)
	}
	s.cancel()
	<-s.done
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
