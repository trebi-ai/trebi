package chains

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// AskOpts is one ask or hand-off. A nil Actor is a person.
type AskOpts struct {
	Workspace      string
	To             string
	Kind           Kind
	Text           string
	Context        string
	Actor          *tools.Actor
	IdempotencyKey string
}

// AskResult is the reply of Ask. Repeat marks an idempotent repeat; Held
// marks a hop that waits for a person.
type AskResult struct {
	Hop            Hop    `json:"hop"`
	Next           string `json:"next,omitempty"`
	Message        string `json:"message,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	Replies        []Hop  `json:"replies,omitempty"`
	Repeat         bool   `json:"-"`
	Held           bool   `json:"-"`
}

// frame is the caller side of an ask.
type frame struct {
	kind    string
	agentID string
	agent   string
	runID   string
	turn    int
	convID  string
	traceID string
	path    []string
	team    *job.TeamYAML
}

// Ask sends an ask or a hand-off to an agent of the same workspace (C.1).
func (s *Service) Ask(ctx context.Context, o AskOpts) (*AskResult, error) {
	o.Kind = cmp.Or(o.Kind, KindAsk)
	o.Text, o.Context = strings.TrimSpace(o.Text), strings.TrimSpace(o.Context)
	if err := o.validate(); err != nil {
		return nil, err
	}
	team, err := s.dir.TeamAgents(ctx, o.Workspace)
	if err != nil {
		return nil, err
	}
	f, err := s.frame(ctx, o, team)
	if err != nil {
		return nil, err
	}
	target := findAgent(team, o.To)
	if target == nil {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("agent %s not found", strings.TrimPrefix(o.To, "agents/"))}
	}
	if target.ID == f.agentID {
		return nil, &Error{Code: CodeValidation, Message: "An agent cannot ask itself."}
	}
	key := cmp.Or(o.IdempotencyKey, f.defaultKey(target.ID, o.Kind, o.Text))
	if key != "" {
		if prev, err := s.hopByKey(ctx, key); err != nil {
			return nil, err
		} else if prev != nil {
			return s.result(ctx, prev, f, target.Name, true), nil
		}
	}
	h := &Hop{
		ID: "hop_" + ulid.Make().String(), TraceID: f.traceID, Workspace: o.Workspace, Kind: o.Kind,
		Depth: len(f.path), pathIDs: append(slices.Clone(f.path), target.ID),
		From: End{Kind: f.kind, AgentID: f.agentID, RunID: f.runID, Turn: f.turn, ConversationID: f.convID},
		To:   End{Kind: EndAgent, AgentID: target.ID},
		Text: o.Text, Context: o.Context, Preview: preview(o.Text), idem: key,
	}
	limits, enabled, err := s.dir.ChainLimits(ctx, o.Workspace)
	if err != nil {
		return nil, err
	}
	if rerr := s.check(ctx, f, target, team, enabled, limits); rerr != nil {
		return nil, s.refuse(ctx, h, rerr)
	}
	c, err := s.ensureChain(ctx, f, h, limits)
	if err != nil {
		return nil, err
	}
	if c.Status == StatusStopped {
		return nil, s.refuse(ctx, h, &Error{Code: CodeStopped, Reason: RefuseStopped,
			Message: "A person stopped this chain. Finish with what you have and reply. Do not ask other agents."})
	}
	if reason, err := s.limitHit(ctx, c, h); err != nil {
		return nil, err
	} else if reason != "" {
		return s.atLimit(ctx, c, f, h, reason)
	}
	return s.queue(ctx, h, f, target.Name)
}

func (o AskOpts) validate() error {
	switch {
	case o.Kind != KindAsk && o.Kind != KindHandoff:
		return &Error{Code: CodeValidation, Message: "kind must be ask or handoff"}
	case o.Text == "":
		return &Error{Code: CodeValidation, Message: "text required"}
	case len(o.Text) > maxText:
		return &Error{Code: CodeValidation, Message: fmt.Sprintf("text is longer than %d bytes. Put the details in a file and name the path.", maxText)}
	case len(o.Context) > maxText:
		return &Error{Code: CodeValidation, Message: fmt.Sprintf("context is longer than %d bytes. Put the details in a file and name the path.", maxText)}
	}
	return nil
}

// queue inserts a queued hop and sends it. A lost race on the idempotency
// key returns the first hop.
func (s *Service) queue(ctx context.Context, h *Hop, f frame, target string) (*AskResult, error) {
	h.Status = HopQueued
	if err := s.insertHop(ctx, h); err != nil {
		if prev, perr := s.hopByKey(ctx, h.idem); perr == nil && prev != nil {
			return s.result(ctx, prev, f, target, true), nil
		}
		return nil, err
	}
	if err := s.send(ctx, h, f, target); err != nil {
		return nil, err
	}
	if f.runID != "" {
		s.runs.AddEvent(ctx, f.runID, "hop_sent", "chains", map[string]string{
			"hop_id": h.ID, "kind": string(h.Kind), "to_agent": target, "to_run_id": h.To.RunID,
		})
	}
	return s.result(ctx, h, f, target, false), nil
}

// frame resolves the caller side (C.1 step 1).
func (s *Service) frame(ctx context.Context, o AskOpts, team []workspaces.TeamAgent) (frame, error) {
	a := o.Actor
	if a == nil || (a.Kind != EndAgent && a.Kind != "job" && a.Kind != EndAssistant) {
		return frame{kind: EndHuman, traceID: ulid.Make().String()}, nil
	}
	switch a.Kind {
	case "job":
		return frame{}, &Error{Code: CodeForbidden, Message: "Only agents can ask agents. Ask the owner agent to do it."}
	case EndAssistant:
		return frame{kind: EndAssistant, convID: a.Conversation, traceID: ulid.Make().String()}, nil
	}
	run, err := s.runs.Get(ctx, a.RunID)
	if err != nil {
		return frame{}, fmt.Errorf("caller run %s: %w", a.RunID, err)
	}
	if run.Workspace != o.Workspace {
		return frame{}, &Error{Code: CodeForbidden, Message: "An agent can ask only the agents of its own workspace."}
	}
	f := frame{kind: EndAgent, agentID: run.JobID, runID: run.ID, turn: run.TurnCount, traceID: run.TraceID}
	if t, err := s.runs.Turn(ctx, run.ID, run.TurnCount); err == nil {
		f.traceID = cmp.Or(t.TraceID, f.traceID)
		f.path = t.Path
	} else if !errors.Is(err, sql.ErrNoRows) {
		return frame{}, err
	}
	if len(f.path) == 0 {
		f.path = []string{f.agentID}
	}
	if me := agentByID(team, f.agentID); me != nil {
		f.agent, f.team = me.Name, me.Team
	} else {
		f.agent = strings.TrimPrefix(run.JobName, "agents/")
	}
	return f, nil
}

// defaultKey is the idempotency key of a run caller that sent none.
func (f frame) defaultKey(toID string, kind Kind, text string) string {
	if f.runID == "" {
		return ""
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s\x00%s\x00%s", f.runID, f.turn, toID, kind, text))
	return hex.EncodeToString(sum[:])
}

// check runs the refusal rules of C.1 steps 2 to 5.
func (s *Service) check(ctx context.Context, f frame, target *workspaces.TeamAgent, team []workspaces.TeamAgent, enabled bool, limits Limits) *Error {
	if !enabled {
		return &Error{Code: CodeUnavailable, Reason: RefuseDisabled,
			Message: "Agents in this workspace cannot ask each other. A person turned it off in the workspace settings."}
	}
	if !target.Available() {
		return &Error{Code: CodeUnavailable, Reason: RefuseDisabled,
			Message: fmt.Sprintf("agents/%s cannot take asks now. Do the work yourself or ask a person.", target.Name)}
	}
	if f.kind == EndAgent && !f.team.Allows(target.Name) {
		return &Error{Code: CodeForbidden, Reason: RefuseTeam,
			Message: fmt.Sprintf("Your team.can_ask list does not include %s.", target.Name)}
	}
	if slices.Contains(f.path, target.ID) {
		waiting := names(team, f.path)
		return &Error{Code: CodeCycle, Reason: RefuseCycle,
			Message: fmt.Sprintf("%s is already in this chain and waits for you. Do not ask %s. Reply instead.", target.Name, strings.Join(waiting, ", "))}
	}
	if f.kind != EndAgent {
		return nil
	}
	var n int
	since := s.now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM chain_hops WHERE from_agent_id=? AND to_agent_id=?
		AND kind IN ('ask','handoff') AND status<>'refused' AND created_at>=?`, f.agentID, target.ID, since).Scan(&n); err != nil {
		s.log.Warn("chain pair rate", "error", err)
		return nil
	}
	if n >= limits.PairPerHour {
		return &Error{Code: CodeRate, Reason: RefuseRate,
			Message: fmt.Sprintf("You asked %s %d times in the last hour. That is the limit. Finish with what you have.", target.Name, n)}
	}
	return nil
}

// refuse keeps the refused hop, so people see what the agent tried.
func (s *Service) refuse(ctx context.Context, h *Hop, e *Error) error {
	h.Status, h.RefuseReason, h.idem = HopRefused, e.Reason, ""
	if err := s.insertHop(ctx, h); err != nil {
		return err
	}
	e.HopID, e.TraceID = h.ID, h.TraceID
	s.log.Info("chain.hop_refused", "refuse_reason", string(e.Reason), "trace_id", h.TraceID,
		"from_agent", h.From.AgentID, "to_agent", h.To.AgentID, "hop_id", h.ID)
	return e
}

// limitHit returns the limit that the new hop passes, or "".
func (s *Service) limitHit(ctx context.Context, c *Chain, h *Hop) (RefuseReason, error) {
	turns, cost, err := s.runs.TraceUsage(ctx, c.TraceID)
	if err != nil {
		return "", err
	}
	var queued int
	if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM chain_hops WHERE trace_id=? AND status=?`,
		c.TraceID, string(HopQueued)).Scan(&queued); err != nil {
		return "", err
	}
	switch {
	case h.Depth > c.Limits.MaxHops:
		return RefuseHops, nil
	case turns+queued+1 > c.Limits.MaxTurns:
		return RefuseTurns, nil
	case c.Limits.MaxUSD > 0 && cost >= c.Limits.MaxUSD:
		return RefuseUSD, nil
	case c.Status == StatusHeld:
		return RefuseTurns, nil
	}
	return "", nil
}

// send writes the message into the pair conversation and delivers it.
func (s *Service) send(ctx context.Context, h *Hop, f frame, target string) error {
	res, err := s.conv.SendHop(ctx, conversations.HopSend{
		Workspace: h.Workspace, OperatorID: h.To.AgentID, Address: pairAddress(f, h.Workspace, target),
		Text:  askMessage(h.Kind, h.ID, f.agent, f.runID, f.turn, h.Text, h.Context),
		HopID: h.ID, TraceID: h.TraceID,
	})
	if err != nil {
		_, _ = s.updateHop(ctx, h.ID, hopUpdate{"status": string(HopFailed)})
		h.Status = HopFailed
		return err
	}
	h.ConversationID, h.MessageID, h.QueueID = res.ConversationID, res.Message.ID, res.QueueID
	if _, err := s.updateHop(ctx, h.ID, hopUpdate{
		"conversation_id": h.ConversationID, "message_id": h.MessageID, "queue_id": h.QueueID,
	}); err != nil {
		return err
	}
	if f.kind != EndAgent && f.convID == "" {
		f.convID = res.ConversationID
		h.From.ConversationID = res.ConversationID
		_, _ = s.updateHop(ctx, h.ID, hopUpdate{"from_conversation_id": res.ConversationID})
		_, _ = s.st.DB().ExecContext(ctx, `UPDATE chains SET root_conversation_id=? WHERE trace_id=? AND root_conversation_id=''`,
			res.ConversationID, h.TraceID)
	}
	if res.RunID != "" {
		if err := s.Bind(ctx, h.ID, res.RunID, res.Turn); err != nil {
			return err
		}
		h.Status, h.To.RunID, h.To.Turn = HopRunning, res.RunID, res.Turn
	}
	return nil
}

// result builds the reply of Ask.
func (s *Service) result(ctx context.Context, h *Hop, f frame, target string, repeat bool) *AskResult {
	if full, err := s.Hop(ctx, h.ID); err == nil {
		h = full
	}
	out := &AskResult{Hop: *h, Repeat: repeat, ConversationID: h.ConversationID, RequestID: h.RequestID, Held: h.Status == HopHeld}
	out.Hop.Text, out.Hop.Context = "", ""
	if f.kind == EndAgent {
		out.Message = askedMessage(h.Kind, target, h.ID)
		if h.Status == HopHeld {
			out.Message = heldMessage(target, h.ID)
		}
		if h.Kind == KindAsk || h.Status == HopHeld {
			out.Next = "end_turn"
		}
	}
	return out
}

// pairAddress is the conversation address of a pair: agent to agent, or a
// person or the assistant to an agent.
func pairAddress(f frame, wsID, target string) conversations.Address {
	if f.kind == EndAgent {
		return conversations.Address{Connector: conversations.ChannelAgent, Room: "agents/" + f.agent + "→agents/" + target}
	}
	return conversations.Address{Connector: conversations.ChannelAssistant, Room: wsID + "#agents/" + target}
}

func (s *Service) hopByKey(ctx context.Context, key string) (*Hop, error) {
	h, err := scanHop(s.st.DB().QueryRowContext(ctx, `SELECT `+hopCols+` FROM chain_hops WHERE idempotency_key=?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return h, err
}

// ensureChain creates the chain row at the first hop of a trace.
func (s *Service) ensureChain(ctx context.Context, f frame, h *Hop, limits Limits) (*Chain, error) {
	now := s.stamp()
	rootAgent, rootRun := "", ""
	if f.kind == EndAgent {
		rootAgent, rootRun = f.path[0], f.runID
	}
	err := s.tx(ctx, func(tx *sql.Tx) ([]store.BusEvent, error) {
		res, err := tx.ExecContext(ctx, `INSERT INTO chains (trace_id, workspace_id, root_kind, root_run_id,
			root_conversation_id, root_agent_id, status, max_hops, max_turns, max_usd, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(trace_id) DO NOTHING`,
			h.TraceID, h.Workspace, f.kind, rootRun, f.convID, rootAgent, StatusOpen,
			limits.MaxHops, limits.MaxTurns, limits.MaxUSD, now, now)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, nil
		}
		return []store.BusEvent{chainEvent("chain.created", "created", h.TraceID, h.Workspace, StatusOpen, rootAgent, 1)}, nil
	})
	if err != nil {
		return nil, err
	}
	return s.chain(ctx, h.TraceID)
}

func chainEvent(topic, action, traceID, ws, status, rootAgent string, rev int64) store.BusEvent {
	return store.BusEvent{
		Topic: topic, TraceID: traceID, Entity: "chain", EntityID: traceID, Action: action, Revision: rev,
		Attrs: map[string]string{"workspace": ws, "trace_id": traceID, "status": status, "root_agent": rootAgent},
	}
}

func findAgent(team []workspaces.TeamAgent, name string) *workspaces.TeamAgent {
	name = strings.TrimPrefix(strings.TrimSpace(name), "agents/")
	for i := range team {
		if team[i].Name == name {
			return &team[i]
		}
	}
	return nil
}

func agentByID(team []workspaces.TeamAgent, id string) *workspaces.TeamAgent {
	for i := range team {
		if team[i].ID == id {
			return &team[i]
		}
	}
	return nil
}

// names maps agent IDs to short names. An unknown ID stays as it is.
func names(team []workspaces.TeamAgent, ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id
		if a := agentByID(team, id); a != nil {
			out[i] = a.Name
		}
	}
	return out
}
