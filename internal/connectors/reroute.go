package connectors

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/trebi-ai/trebi/internal/decisions"
	"github.com/trebi-ai/trebi/internal/decisions/policies"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/routing"
)

// Errors of Reroute.
var (
	ErrRoutingOff      = errors.New("routing is off for this connection")
	ErrRouteNotFound   = errors.New("route not found")
	ErrAmbiguousTarget = errors.New("more than one route has this target")
)

// NotAcceptedError is a route whose filters stop the event.
type NotAcceptedError struct{ Stage, Reason string }

func (e *NotAcceptedError) Error() string {
	return "the route does not take this event: " + cmp.Or(e.Reason, e.Stage)
}

// RerouteOpts is one correction of the route of a conversation. To is a
// route id, a target name, or "nobody". Ignore with To "nobody" runs nothing
// and gives the decision no feedback.
type RerouteOpts struct {
	EventID, Workspace, To string
	Force, Ignore          bool
}

// RerouteResult is what a correction did. Feedback is "agree", "disagree",
// or "" when the correction gives the decision no feedback.
type RerouteResult struct {
	EventID     string                `json:"event_id"`
	Workspace   string                `json:"workspace"`
	Connection  string                `json:"connection"`
	Key         string                `json:"conversation_key"`
	RouteID     string                `json:"route_id,omitempty"`
	Target      string                `json:"target,omitempty"`
	TargetKind  string                `json:"target_kind,omitempty"`
	QueueID     string                `json:"queue_id,omitempty"`
	Outcome     dispatch.EventOutcome `json:"outcome,omitempty"`
	DecisionID  string                `json:"decision_id,omitempty"`
	PickRouteID string                `json:"pick_route_id,omitempty"`
	InboxID     string                `json:"inbox_id,omitempty"`
	Wrong       bool                  `json:"wrong"`
	Feedback    string                `json:"feedback,omitempty"`
}

// Reroute moves the whole conversation of an event to another route, or to
// nobody. Later replies follow the new choice. It cancels the queued item
// of the old route for this event; a run that started stays.
func (s *Service) Reroute(ctx context.Context, o RerouteOpts) (RerouteResult, error) {
	res := RerouteResult{EventID: o.EventID, Workspace: o.Workspace}
	ev, err := s.loadRecord(ctx, o.EventID)
	if err != nil {
		return res, err
	}
	if !s.prepareEvent(&ev) {
		return res, ErrRoutingOff
	}
	res.Connection, res.Key = ev.slug, ev.routeKey()
	all := s.workspaceRoutes(ctx, o.Workspace, ev.slug)
	if !slices.ContainsFunc(all, func(t cachedTrig) bool { return t.mode == routing.ModeOn }) {
		return res, ErrRoutingOff
	}
	chosen := slices.DeleteFunc(slices.Clone(all), func(t cachedTrig) bool { return !t.choosable() })
	var target *cachedTrig
	if o.To != routing.RuleNobody {
		t, err := resolveTarget(chosen, o.To)
		if err != nil {
			return res, err
		}
		if err := s.rerouteGate(ctx, ev, t, o.Force); err != nil {
			return res, err
		}
		target = &t
		res.RouteID, res.Target, res.TargetKind = t.tr.Route, t.route.TargetName(), kindOf(t)
	}

	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	cur, found, err := s.loadChoice(ctx, s.st.DB(), o.Workspace, ev.slug, res.Key)
	if err != nil {
		return res, err
	}
	res.DecisionID, res.PickRouteID, res.InboxID = cur.DecisionID, cur.PickRouteID, cur.InboxID
	via := routeVia{Method: MethodYou, DecisionID: cur.DecisionID, Force: o.Force, Action: "rerouted"}
	if o.Ignore {
		via.Method = MethodIgnored
	}
	switch {
	case found && (cur.Status == ChoiceDeciding || cur.Status == ChoiceWaiting):
		err = s.rerouteWaiting(ctx, cur, chosen, target, via)
	default:
		if !found {
			cur = ConversationChoice{Workspace: o.Workspace, Connection: ev.slug, Key: res.Key, FirstEventID: ev.id}
		}
		err = s.rerouteSettled(ctx, ev, cur, chosen, target, via)
	}
	if err != nil {
		return res, err
	}
	if target != nil {
		res.Outcome, res.QueueID = s.outcomeOf(ctx, ev.id, o.Workspace, target.tr.Route)
	}
	res.Wrong, res.Feedback = rerouteFeedback(res, found, o.Ignore)
	return res, nil
}

// workspaceRoutes lists the live routes of one connection in a workspace.
func (s *Service) workspaceRoutes(ctx context.Context, ws, conn string) []cachedTrig {
	var out []cachedTrig
	for _, t := range s.triggersFor(ctx, conn) {
		if t.row.WorkspaceID == ws {
			out = append(out, t)
		}
	}
	return out
}

func kindOf(t cachedTrig) string {
	if t.route.Job != "" {
		return "job"
	}
	return "agent"
}

// resolveTarget finds a route by id, then by target name. A name that more
// than one route has is ambiguous.
func resolveTarget(chosen []cachedTrig, to string) (cachedTrig, error) {
	if t, ok := find(chosen, to); ok {
		return t, nil
	}
	var hits []cachedTrig
	for _, t := range chosen {
		if t.route.TargetName() == to {
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 0:
		return cachedTrig{}, fmt.Errorf("%w: %s", ErrRouteNotFound, to)
	case 1:
		return hits[0], nil
	}
	return cachedTrig{}, fmt.Errorf("%w: %s; use the route id", ErrAmbiguousTarget, to)
}

// rerouteGate checks the filters of the route for the event. Force skips
// only the match filter, as a run by hand does.
func (s *Service) rerouteGate(ctx context.Context, ev eventRecord, t cachedTrig, force bool) error {
	g := GateFor(t.tr, ev.connector).WithInputs(t.j, t.tr)
	if force {
		g.Match = ""
	}
	if r := g.Eval(ctx, ev.gateInput(), ev.payload); !r.Matched {
		return &NotAcceptedError{Stage: r.Stage, Reason: r.Error}
	}
	return nil
}

// rerouteWaiting settles a conversation that waits for the router or a
// person. Every waiting event of the conversation moves.
func (s *Service) rerouteWaiting(ctx context.Context, cur ConversationChoice, chosen []cachedTrig, target *cachedTrig, via routeVia) error {
	first, err := s.loadRecord(ctx, cur.FirstEventID)
	if err != nil {
		return err
	}
	if !s.prepareEvent(&first) {
		return ErrRoutingOff
	}
	return s.settle(ctx, cur, first, s.candidates(ctx, first, chosen, false), target, via)
}

// rerouteSettled moves a conversation that has a choice, or none.
func (s *Service) rerouteSettled(ctx context.Context, ev eventRecord, c ConversationChoice, chosen []cachedTrig, target *cachedTrig, via routeVia) error {
	c.Method, c.RuleIndex = via.Method, 0
	keep := ""
	if target != nil {
		c.Status, c.RouteID, c.Target = ChoiceChosen, target.tr.Route, target.route.TargetName()
		keep = target.tr.Route
	} else {
		c.Status, c.RouteID, c.Target = ChoiceNobody, "", ""
	}
	if err := s.saveChoiceBus(ctx, c, ev, via.Action, nil); err != nil {
		return err
	}
	started := s.cancelQueued(ctx, ev.id, c.Workspace, keep)
	if target != nil {
		if oc, _ := s.outcomeOf(ctx, ev.id, c.Workspace, keep); oc != dispatch.OutcomeQueued {
			s.dispatchTrigger(ctx, ev, *target, via)
		}
	}
	for _, t := range chosen {
		if t.tr.Route == keep || started[t.tr.Route] {
			continue
		}
		if oc, _ := s.outcomeOf(ctx, ev.id, c.Workspace, t.tr.Route); oc != "" || s.passes(ctx, ev, t) {
			s.insertOutcome(ctx, ev, t.row, t.tr.Route, dispatch.OutcomeNotChosen, via, "", "another route got this message")
		}
	}
	return nil
}

// passes reports whether the filters of the route take the event.
func (s *Service) passes(ctx context.Context, ev eventRecord, t cachedTrig) bool {
	return GateFor(t.tr, ev.connector).WithInputs(t.j, t.tr).Eval(ctx, ev.gateInput(), ev.payload).Matched
}

// cancelQueued skips the waiting queue items of the event for the routes
// other than keep. It returns the routes that already have a run.
func (s *Service) cancelQueued(ctx context.Context, eventID, ws, keep string) map[string]bool {
	started := map[string]bool{}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id, route_id, status, COALESCE(run_id,'') FROM event_queue
		WHERE event_id=? AND workspace_id=? AND route_id <> ? AND route_id <> ''`, eventID, ws, keep)
	if err != nil {
		s.log.Warn("reroute queue read", "event", eventID, "error", err)
		return started
	}
	type item struct{ id, route, status, run string }
	var list []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.route, &it.status, &it.run); err != nil {
			s.log.Warn("reroute queue read", "event", eventID, "error", err)
			break
		}
		list = append(list, it)
	}
	_ = rows.Close() //nolint:errcheck // read-only query
	for _, it := range list {
		switch {
		case it.status == string(dispatch.QueuePending) || it.status == string(dispatch.QueueBlocked):
			if err := s.queue.Skip(ctx, it.id, "another route got this message"); err != nil {
				s.log.Warn("reroute cancel", "queue", it.id, "error", err)
			}
		case it.run != "" || it.status == string(dispatch.QueueClaimed):
			started[it.route] = true
		}
	}
	return started
}

// outcomeOf reads the outcome row of an event and a route.
func (s *Service) outcomeOf(ctx context.Context, eventID, ws, routeID string) (dispatch.EventOutcome, string) {
	var oc, q sql.NullString
	err := s.st.DB().QueryRowContext(ctx, `SELECT outcome, queue_id FROM event_outcomes WHERE event_id=? AND workspace_id=? AND route_id=?`,
		eventID, ws, routeID).Scan(&oc, &q)
	if err != nil {
		return "", ""
	}
	return dispatch.EventOutcome(oc.String), q.String
}

// rerouteFeedback tells if the correction differs from the pick of the
// decision, and the feedback value for it. A correction with no decision
// pick, or an ignore, gives no feedback.
func rerouteFeedback(r RerouteResult, found, ignore bool) (wrong bool, feedback string) {
	if !found || ignore || r.DecisionID == "" || r.PickRouteID == "" {
		return false, ""
	}
	got := r.RouteID
	if got == "" {
		got = policies.RouteNobodyKey
	}
	if got != r.PickRouteID {
		return true, decisions.FeedbackDisagree
	}
	return false, decisions.FeedbackAgree
}
