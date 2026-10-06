package connectors

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// Gate is the ingress check of one event trigger: room, sender, from,
// contains, extract, match. The pipeline and the rule tester share it.
type Gate struct {
	Rooms     []string
	Allowlist []string
	From      string   // job.FromPeople | job.FromBots | job.FromAnyone; empty is anyone
	Contains  []string // any entry, case-insensitive
	Match     string
	Extract   string
	Fold      bool // compare senders without case (native email)
}

// Stage names the step that stopped an event.
const (
	StageRoom     = "room"
	StageSender   = "sender"
	StageFrom     = "from"
	StageContains = "contains"
	StageExtract  = "extract"
	StageMatch    = "match"
)

// GateInput is what the gate reads from an event, apart from the payload.
type GateInput struct {
	Room     string
	Identity string
	Text     string
	Bot      bool
	FollowUp bool // the event continues an open conversation (plan A.9)
}

// GateResult is the outcome of one gate check.
type GateResult struct {
	Matched   bool
	Stage     string // empty when Matched
	FollowUp  bool
	Vars      map[string]string
	Extracted any // extract output; nil when there is no extract
	Error     string
}

// GateFor builds the gate of an event trigger. connector is the type of the
// connection that sent the event.
func GateFor(tr job.Trigger, connector string) Gate {
	return Gate{
		Rooms: tr.Rooms, Allowlist: tr.Allowlist, From: tr.From, Contains: tr.Contains,
		Match: tr.Match, Extract: tr.Extract, Fold: connector == NativeEmail,
	}
}

// Eval checks room → sender → from → contains → extract → match. A
// follow-up skips from, contains, and the result of match; extract still
// gives the vars.
func (g Gate) Eval(ctx context.Context, in GateInput, payload any) GateResult {
	if len(g.Rooms) > 0 && !slices.Contains(g.Rooms, in.Room) {
		return GateResult{Stage: StageRoom}
	}
	if len(g.Allowlist) > 0 && !g.sender(in.Identity) {
		return GateResult{Stage: StageSender}
	}
	if !in.FollowUp {
		if !g.from(in.Bot) {
			return GateResult{Stage: StageFrom}
		}
		if !g.contains(in.Text) {
			return GateResult{Stage: StageContains}
		}
	}
	match := g.Match
	if in.FollowUp {
		match = ""
	}
	res := dispatch.EvalMatch(ctx, match, g.Extract, payload)
	out := GateResult{Matched: res.Matched && res.Error == "", FollowUp: in.FollowUp, Vars: res.Vars, Extracted: res.Extracted, Error: res.Error}
	switch {
	case res.ExtractErr:
		out.Stage = StageExtract
	case !out.Matched:
		out.Stage = StageMatch
	}
	return out
}

// from compares the sender kind with the from key.
func (g Gate) from(bot bool) bool {
	switch g.From {
	case job.FromPeople:
		return !bot
	case job.FromBots:
		return bot
	}
	return true
}

// contains reports whether text contains any entry, without case. No
// entries takes every text.
func (g Gate) contains(text string) bool {
	if len(g.Contains) == 0 {
		return true
	}
	text = strings.ToLower(text)
	for _, c := range g.Contains {
		if strings.Contains(text, strings.ToLower(c)) {
			return true
		}
	}
	return false
}

// sender compares the identity with the allowlist. A connector with no
// identity is closed. An email address compares without case; every other
// identity is exact.
func (g Gate) sender(identity string) bool {
	if identity == "" {
		return false
	}
	for _, a := range g.Allowlist {
		if a == "" {
			continue
		}
		if a == identity || (g.Fold && strings.EqualFold(strings.TrimSpace(a), identity)) {
			return true
		}
	}
	return false
}

// GateEvent is the event side of a gate check.
type GateEvent struct {
	ID        string
	Connector string // type of the connection; the native name for email
	Room      string
	Identity  string
	Text      string
	Bot       bool
	Payload   any
	record    eventRecord // the built-in vars and the conversation key
}

// StoredGateEvent loads one stored event for a gate check. A missing id is
// ErrEventNotFound.
func (s *Service) StoredGateEvent(ctx context.Context, id string) (GateEvent, error) {
	rec, err := s.loadRecord(ctx, id)
	if err != nil {
		return GateEvent{}, err
	}
	return rec.gateEvent(), nil
}

// DraftGateEvent maps an event value that is not stored. A value with the
// trebi-connector/1 event shape gives the room, the sender, and the text;
// any other value has none.
func DraftGateEvent(connector string, payload any) GateEvent {
	ev := GateEvent{Connector: connector, Payload: payload}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ev
	}
	var pe protocol.Event
	if json.Unmarshal(raw, &pe) != nil || pe.Type == "" {
		return ev
	}
	rec := protocolRecord(connector, "", pe, false)
	rec.payload = payload
	return rec.gateEvent()
}

// gateEvent is the gate side of a record.
func (r eventRecord) gateEvent() GateEvent {
	return GateEvent{
		ID: r.id, Connector: r.connector, Room: r.room, Identity: r.identity,
		Text: r.text, Bot: r.bot, Payload: r.payload, record: r,
	}
}

// Eval runs the gate against the event. The sender fold follows the
// connector of the event. A bot message in an open conversation does not
// run, as in the pipeline (plan A.9).
func (ev GateEvent) Eval(ctx context.Context, g Gate, followUp bool) GateResult {
	if followUp && ev.Bot {
		return GateResult{Stage: StageFrom}
	}
	g.Fold = ev.Connector == NativeEmail
	return g.Eval(ctx, GateInput{Room: ev.Room, Identity: ev.Identity, Text: ev.Text, Bot: ev.Bot, FollowUp: followUp}, ev.Payload)
}

// FollowUp reports whether ev continues an open conversation of j, a job
// with replies: continue.
func (s *Service) FollowUp(ctx context.Context, ev GateEvent, row workspaces.JobRow, j *job.Job) (bool, error) {
	return s.conversationOpen(ctx, ev.record, row, j)
}

// Vars merges the built-in vars of the event under the extract vars.
func (ev GateEvent) Vars(extract map[string]string) map[string]string {
	vars := ev.record.vars()
	if vars == nil && extract == nil {
		return nil
	}
	return mergeVars(vars, extract)
}

// ConversationKey is the lane key that a trigger gives the event. An
// extract var conversation_key wins.
func (ev GateEvent) ConversationKey(continues bool, extract map[string]string) string {
	return cmp.Or(extract["conversation_key"], ev.record.conversationKey(continues))
}
