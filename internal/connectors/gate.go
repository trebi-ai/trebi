package connectors

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/job"
)

// Gate is the ingress check of one event trigger: room, sender, extract,
// match. The pipeline and the rule tester share it.
type Gate struct {
	Rooms     []string
	Allowlist []string
	Match     string
	Extract   string
	Fold      bool // compare senders without case (native email)
}

// Stage names the step that stopped an event.
const (
	StageRoom    = "room"
	StageSender  = "sender"
	StageExtract = "extract"
	StageMatch   = "match"
)

// GateResult is the outcome of one gate check.
type GateResult struct {
	Matched   bool
	Stage     string // empty when Matched
	Vars      map[string]string
	Extracted any // extract output; nil when there is no extract
	Error     string
}

// GateFor builds the gate of an event trigger. connector is the type of the
// connection that sent the event.
func GateFor(tr job.Trigger, connector string) Gate {
	return Gate{
		Rooms: tr.Rooms, Allowlist: tr.Allowlist, Match: tr.Match, Extract: tr.Extract,
		Fold: connector == NativeEmail,
	}
}

// Eval checks the room, then the sender, then runs extract and match.
func (g Gate) Eval(ctx context.Context, room, identity string, payload any) GateResult {
	if len(g.Rooms) > 0 && !slices.Contains(g.Rooms, room) {
		return GateResult{Stage: StageRoom}
	}
	if len(g.Allowlist) > 0 && !g.sender(identity) {
		return GateResult{Stage: StageSender}
	}
	res := dispatch.EvalMatch(ctx, g.Match, g.Extract, payload)
	out := GateResult{Matched: res.Matched && res.Error == "", Vars: res.Vars, Extracted: res.Extracted, Error: res.Error}
	switch {
	case res.ExtractErr:
		out.Stage = StageExtract
	case !out.Matched:
		out.Stage = StageMatch
	}
	return out
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
	Payload   any
}

// StoredGateEvent loads one stored event for a gate check. A missing id is
// ErrEventNotFound.
func (s *Service) StoredGateEvent(ctx context.Context, id string) (GateEvent, error) {
	ev := GateEvent{ID: id}
	var raw string
	var room, identity sql.NullString
	err := s.st.DB().QueryRowContext(ctx, `SELECT connector, COALESCE(raw_json,''), room, identity FROM events WHERE id=?`, id).
		Scan(&ev.Connector, &raw, &room, &identity)
	if errors.Is(err, sql.ErrNoRows) {
		return GateEvent{}, fmt.Errorf("%w: %s", ErrEventNotFound, id)
	}
	if err != nil {
		return GateEvent{}, err
	}
	ev.Room, ev.Identity, ev.Payload = room.String, identity.String, decodeStored(raw)
	return ev, nil
}

// DraftGateEvent maps an event value that is not stored. A value with the
// trebi-connector/1 event shape gives the room and the sender; any other
// value has neither.
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
	ev.Room, ev.Identity = rec.room, rec.identity
	return ev
}

// Eval runs the gate against the event. The sender fold follows the
// connector of the event.
func (ev GateEvent) Eval(ctx context.Context, g Gate) GateResult {
	g.Fold = ev.Connector == NativeEmail
	return g.Eval(ctx, ev.Room, ev.Identity, ev.Payload)
}
