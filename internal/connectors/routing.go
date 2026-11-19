package connectors

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/decisions"
	"github.com/trebi-ai/trebi/internal/decisions/policies"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/store"
)

// How a conversation got its route. The router methods are in policies.
const (
	MethodYou          = "you"
	MethodConversation = "conversation"
	MethodOnly         = "only"
	MethodNothing      = "nothing"
	MethodRule         = "rule"
	MethodIgnored      = "ignored"
)

const (
	routerTick        = 30 * time.Second
	routerMaxAttempts = 3
	choiceKeep        = 180 * 24 * time.Hour
)

// RouteDecider makes and tests routing decisions. *decisions.Service
// satisfies it.
type RouteDecider interface {
	Decide(ctx context.Context, workspace, kind string, subj decisions.Subject, state decisions.State) (decisions.Decision, error)
	Test(ctx context.Context, kind string, state decisions.State) (decisions.Decision, error)
}

// RouteWaitChoice is one route that a person can pick.
type RouteWaitChoice struct {
	RouteID, Target, TargetKind string
	Share                       float64
}

// RouteWait is a conversation that waits for a person to choose a route.
// Failed is true when the router did not finish.
type RouteWait struct {
	Workspace, Connection, ConnectionTitle, EventID, DecisionID, Summary, TraceID string
	Choices                                                                       []RouteWaitChoice
	Failed                                                                        bool
}

// RouteWaitHook raises the Inbox item of a RouteWait. It returns the id.
type RouteWaitHook func(ctx context.Context, w RouteWait) (inboxID string, err error)

// ChoiceStatus is the state of a conversation choice.
type ChoiceStatus string

// The states of a conversation choice.
const (
	ChoiceChosen   ChoiceStatus = "chosen"
	ChoiceNobody   ChoiceStatus = "nobody"
	ChoiceWaiting  ChoiceStatus = "waiting"
	ChoiceDeciding ChoiceStatus = "deciding"
)

// ConversationChoice is the route that one conversation of a connection
// has. Later messages of the conversation follow it.
type ConversationChoice struct {
	Workspace, Connection, Key string
	Status                     ChoiceStatus
	RouteID, Target, Method    string
	RuleIndex                  int
	DecisionID, PickRouteID    string
	FirstEventID, InboxID      string
	Attempts                   int
	Revision                   int64
	CreatedAt, UpdatedAt       time.Time
}

// routeVia tells how a route got a message.
type routeVia struct {
	Method     string
	RuleIndex  int
	DecisionID string
	Share      float64
	FollowUp   bool // the message continues a chosen conversation
	Force      bool // a person sends it: the match filter does not apply
	Action     string
}

var errChoiceRace = errors.New("the conversation choice changed")

// WithRouteDecider sets the decider of the router. With none, the router
// step asks a person.
func (s *Service) WithRouteDecider(d RouteDecider) *Service {
	s.routeDecider = d
	return s
}

// OnRouteWait sets the hook that asks a person to choose a route.
func (s *Service) OnRouteWait(h RouteWaitHook) *Service {
	s.onRouteWait = h
	return s
}

// choosable reports whether the route takes part in the choice of routing.
func (c cachedTrig) choosable() bool {
	return c.mode == routing.ModeOn && c.route.Gets == routing.GetsChosen && c.broken == "" && c.route.ID != ""
}

// routeKey is the conversation of the event for routing: <room>/<root>, or
// the room when the root is the room.
func (r eventRecord) routeKey() string {
	switch {
	case r.root != "" && r.root != r.room && r.room != "":
		return r.room + "/" + r.root
	case r.root != "" && r.room == "":
		return r.root
	case r.room != "":
		return r.room
	}
	return cmp.Or(r.thread, r.id)
}

// routeMessage is the part of the event that a rule reads.
func (r eventRecord) routeMessage() routing.Message {
	subject := cmp.Or(r.subject, firstLine(r.text))
	return routing.Message{From: cmp.Or(r.identity, r.sender), Subject: subject, Text: r.text, Room: r.room}
}

// fromLabel is the sender as the router and the Inbox show it.
func (r eventRecord) fromLabel() string {
	if r.sender != "" && r.identity != "" && r.sender != r.identity {
		return r.sender + " <" + r.identity + ">"
	}
	return cmp.Or(r.sender, r.identity)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const choiceCols = `workspace_id, connection, conversation_key, status, COALESCE(route_id,''), COALESCE(target,''), method,
	COALESCE(rule_index,0), COALESCE(decision_id,''), COALESCE(pick_route_id,''), first_event_id, COALESCE(inbox_id,''),
	attempts, revision, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanChoice(r rowScanner) (ConversationChoice, error) {
	var c ConversationChoice
	var status, created, updated string
	err := r.Scan(&c.Workspace, &c.Connection, &c.Key, &status, &c.RouteID, &c.Target, &c.Method, &c.RuleIndex,
		&c.DecisionID, &c.PickRouteID, &c.FirstEventID, &c.InboxID, &c.Attempts, &c.Revision, &created, &updated)
	if err != nil {
		return c, err
	}
	c.Status = ChoiceStatus(status)
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, created) // a bad time reads as zero
	c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return c, nil
}

// loadChoice reads the choice of one conversation.
func (s *Service) loadChoice(ctx context.Context, q rowQuerier, ws, conn, key string) (ConversationChoice, bool, error) {
	c, err := scanChoice(q.QueryRowContext(ctx, `SELECT `+choiceCols+` FROM route_choices
		WHERE workspace_id=? AND connection=? AND conversation_key=?`, ws, conn, key))
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationChoice{}, false, nil
	}
	return c, err == nil, err
}

// saveChoice inserts the choice, or updates it when its revision is the
// stored one. A lost race returns errChoiceRace.
func (s *Service) saveChoice(ctx context.Context, tx *sql.Tx, c ConversationChoice) error {
	now := s.now().UTC().Format(time.RFC3339Nano)
	var res sql.Result
	var err error
	if c.Revision == 0 {
		res, err = tx.ExecContext(ctx, `INSERT INTO route_choices (workspace_id, connection, conversation_key, status, route_id, target, method,
			rule_index, decision_id, pick_route_id, first_event_id, inbox_id, attempts, revision, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?) ON CONFLICT DO NOTHING`,
			c.Workspace, c.Connection, c.Key, string(c.Status), store.NullStr(c.RouteID), store.NullStr(c.Target), c.Method,
			nullInt(c.RuleIndex), store.NullStr(c.DecisionID), store.NullStr(c.PickRouteID), c.FirstEventID,
			store.NullStr(c.InboxID), c.Attempts, now, now)
	} else {
		res, err = tx.ExecContext(ctx, `UPDATE route_choices SET status=?, route_id=?, target=?, method=?, rule_index=?,
			decision_id=?, pick_route_id=?, first_event_id=?, inbox_id=?, attempts=?, revision=revision+1, updated_at=?
			WHERE workspace_id=? AND connection=? AND conversation_key=? AND revision=?`,
			string(c.Status), store.NullStr(c.RouteID), store.NullStr(c.Target), c.Method, nullInt(c.RuleIndex),
			store.NullStr(c.DecisionID), store.NullStr(c.PickRouteID), c.FirstEventID, store.NullStr(c.InboxID),
			c.Attempts, now, c.Workspace, c.Connection, c.Key, c.Revision)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errChoiceRace
	}
	return nil
}

// saveChoiceBus saves the choice and appends an event.routed event in one
// transaction. run adds more writes to the transaction.
func (s *Service) saveChoiceBus(ctx context.Context, c ConversationChoice, ev eventRecord, action string, run func(tx *sql.Tx) error) error {
	s.st.LockBus()
	err := s.st.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.saveChoice(ctx, tx, c); err != nil {
			return err
		}
		if run != nil {
			if err := run(tx); err != nil {
				return err
			}
		}
		_, err := store.AppendInTx(tx, routedEvent(ev.id, ev.trace, action, c))
		return err
	})
	s.st.UnlockBus()
	if err == nil {
		s.st.NotifyBus()
	}
	return err
}

func routedEvent(eventID, trace, action string, c ConversationChoice) store.BusEvent {
	return store.BusEvent{
		Topic: "event.routed", TraceID: trace, Entity: "event", EntityID: eventID, Action: action,
		Attrs: map[string]string{
			"workspace": c.Workspace, "connection": c.Connection, "conversation_key": c.Key, "route_id": c.RouteID,
			"target": c.Target, "method": c.Method, "status": string(c.Status), "decision_id": c.DecisionID,
		},
	}
}

// candidates keeps the chosen routes whose filters pass for ev. When
// record is true, a route that fails writes its outcome.
func (s *Service) candidates(ctx context.Context, ev eventRecord, ts []cachedTrig, record bool) []cachedTrig {
	var out []cachedTrig
	for _, t := range ts {
		res := GateFor(t.tr, ev.connector).WithInputs(t.j, t.tr).Eval(ctx, ev.gateInput(), ev.payload)
		if res.Matched {
			out = append(out, t)
		} else if record {
			s.insertOutcome(ctx, ev, t.row, t.tr.Route, failedOutcome(res), routeVia{}, "", res.Error)
		}
	}
	return out
}

// outcomeAll writes the same outcome for each route in ts.
func (s *Service) outcomeAll(ctx context.Context, ev eventRecord, ts []cachedTrig, outcome dispatch.EventOutcome, via routeVia, msg string) {
	for _, t := range ts {
		s.insertOutcome(ctx, ev, t.row, t.tr.Route, outcome, via, "", msg)
	}
}

// outcomeOthers writes not_chosen for each route in ts except the one with
// id keep.
func (s *Service) outcomeOthers(ctx context.Context, ev eventRecord, ts []cachedTrig, keep string, via routeVia) {
	for _, t := range ts {
		if t.tr.Route != keep {
			s.insertOutcome(ctx, ev, t.row, t.tr.Route, dispatch.OutcomeNotChosen, via, "", "another route got this message")
		}
	}
}

// find returns the route with the id.
func find(ts []cachedTrig, routeID string) (cachedTrig, bool) {
	i := slices.IndexFunc(ts, func(t cachedTrig) bool { return t.tr.Route == routeID })
	if i < 0 {
		return cachedTrig{}, false
	}
	return ts[i], true
}

// routeEvent sends one event to one of the chosen routes of a workspace:
// the stored choice of the conversation, the only candidate, a rule, or the
// router. It writes outcomes and logs; it returns no error.
func (s *Service) routeEvent(ctx context.Context, ev eventRecord, chosen []cachedTrig) {
	if len(chosen) == 0 || s.st.IsDraining() {
		return
	}
	ws, conn, key := chosen[0].row.WorkspaceID, ev.slug, ev.routeKey()
	file := chosen[0].file
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	choice, found, err := s.loadChoice(ctx, s.st.DB(), ws, conn, key)
	if err != nil {
		s.log.Warn("route choice read", "event", ev.id, "error", err)
		return
	}
	if found && choice.Status == ChoiceChosen {
		if t, ok := find(chosen, choice.RouteID); ok {
			s.followChoice(ctx, ev, choice, t, chosen)
			return
		}
	}
	if found && (choice.Status == ChoiceDeciding || choice.Status == ChoiceWaiting) {
		// A reply follows the choice, so every chosen route waits.
		s.outcomeAll(ctx, ev, chosen, dispatch.OutcomeRouteWait, routeVia{DecisionID: choice.DecisionID}, "waiting for a choice")
		return
	}
	cands := s.candidates(ctx, ev, chosen, true)
	base := ConversationChoice{Workspace: ws, Connection: conn, Key: key, Revision: choice.Revision, FirstEventID: ev.id}
	switch {
	case len(cands) == 0:
		return
	case len(cands) == 1:
		s.takeRoute(ctx, ev, base, cands[0], cands, routeVia{Method: MethodOnly})
		return
	}
	if s.applyRules(ctx, ev, file, base, cands) {
		return
	}
	s.startRouter(ctx, ev, base, cands)
}

// applyRules takes the route of the first rule that fits. It reports whether
// a rule decided.
func (s *Service) applyRules(ctx context.Context, ev eventRecord, file *routing.File, base ConversationChoice, cands []cachedTrig) bool {
	msg := ev.routeMessage()
	for i, r := range file.Rules {
		if !r.Fits(msg) {
			continue
		}
		via := routeVia{Method: MethodRule, RuleIndex: i + 1}
		if r.To == routing.RuleNobody {
			s.takeNobody(ctx, ev, base, cands, via)
			return true
		}
		if t, ok := candidateFor(cands, r.To); ok {
			s.takeRoute(ctx, ev, base, t, cands, via)
			return true
		}
	}
	return false
}

// candidateFor finds the first candidate whose target has the name.
func candidateFor(cands []cachedTrig, target string) (cachedTrig, bool) {
	i := slices.IndexFunc(cands, func(t cachedTrig) bool { return t.route.TargetName() == target })
	if i < 0 {
		return cachedTrig{}, false
	}
	return cands[i], true
}

// followChoice sends a message of a chosen conversation to its route. The
// other chosen routes that pass get not_chosen.
func (s *Service) followChoice(ctx context.Context, ev eventRecord, c ConversationChoice, t cachedTrig, chosen []cachedTrig) {
	via := routeVia{Method: MethodConversation, DecisionID: c.DecisionID, FollowUp: ev.id != c.FirstEventID}
	if !via.FollowUp {
		via.Method, via.RuleIndex = c.Method, c.RuleIndex
	}
	s.dispatchTrigger(ctx, ev, t, via)
	var others []cachedTrig
	for _, o := range chosen {
		if o.tr.Route != t.tr.Route {
			others = append(others, o)
		}
	}
	s.outcomeOthers(ctx, ev, s.candidates(ctx, ev, others, true), t.tr.Route, via)
}

// takeRoute saves the choice and sends the event to t.
func (s *Service) takeRoute(ctx context.Context, ev eventRecord, c ConversationChoice, t cachedTrig, cands []cachedTrig, via routeVia) {
	c.Status, c.RouteID, c.Target, c.Method, c.RuleIndex = ChoiceChosen, t.tr.Route, t.route.TargetName(), via.Method, via.RuleIndex
	if err := s.saveChoiceBus(ctx, c, ev, "routed", nil); err != nil {
		s.log.Warn("route choice save", "event", ev.id, "error", err)
	}
	s.dispatchTrigger(ctx, ev, t, via)
	s.outcomeOthers(ctx, ev, cands, t.tr.Route, via)
}

// takeNobody saves a choice of nobody and drops the event.
func (s *Service) takeNobody(ctx context.Context, ev eventRecord, c ConversationChoice, cands []cachedTrig, via routeVia) {
	c.Status, c.Method, c.RuleIndex = ChoiceNobody, via.Method, via.RuleIndex
	if err := s.saveChoiceBus(ctx, c, ev, "routed", nil); err != nil {
		s.log.Warn("route choice save", "event", ev.id, "error", err)
	}
	s.outcomeAll(ctx, ev, cands, dispatch.OutcomeNotChosen, via, "nobody gets this message")
}

// startRouter marks the conversation as deciding and wakes the worker.
func (s *Service) startRouter(ctx context.Context, ev eventRecord, c ConversationChoice, cands []cachedTrig) {
	c.Status, c.Method = ChoiceDeciding, ""
	now := ev.receivedAt
	err := s.saveChoiceBus(ctx, c, ev, "waiting", func(tx *sql.Tx) error {
		for _, t := range cands {
			if err := upsertOutcomeIn(ctx, tx, ev.id, t.row.WorkspaceID, t.row.ID, t.tr.Route, dispatch.OutcomeRouteWait, routeVia{}, "", "deciding", now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.log.Warn("route start", "event", ev.id, "error", err)
		return
	}
	s.wakeRouter()
}

func (s *Service) wakeRouter() {
	select {
	case s.routerWake <- struct{}{}:
	default:
	}
}

// runRouter decides the conversations that wait for the router, on each
// tick and wake. It ends with ctx.
func (s *Service) runRouter(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.routerEvery)
	defer t.Stop()
	for {
		s.decidePending(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.routerWake:
		}
	}
}

// decidePending decides each deciding conversation, oldest first.
func (s *Service) decidePending(ctx context.Context) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+choiceCols+` FROM route_choices WHERE status=? ORDER BY updated_at`, string(ChoiceDeciding))
	if err != nil {
		s.log.Warn("router scan", "error", err)
		return
	}
	var list []ConversationChoice
	for rows.Next() {
		c, err := scanChoice(rows)
		if err != nil {
			s.log.Warn("router scan", "error", err)
			break
		}
		list = append(list, c)
	}
	_ = rows.Close() //nolint:errcheck // read-only query
	for _, c := range list {
		if ctx.Err() != nil {
			return
		}
		if err := s.decideRoute(ctx, c); err != nil {
			s.log.Warn("route decision", "event", c.FirstEventID, "error", err)
		}
	}
}

// chosenRoutes lists the live chosen routes of one connection in a workspace.
func (s *Service) chosenRoutes(ctx context.Context, ws, conn string) []cachedTrig {
	var out []cachedTrig
	for _, t := range s.triggersFor(ctx, conn) {
		if t.row.WorkspaceID == ws && t.choosable() {
			out = append(out, t)
		}
	}
	return out
}

// routeChoices names the candidates r1..rN in file order.
func routeChoices(cands []cachedTrig) []policies.RouteChoice {
	out := make([]policies.RouteChoice, len(cands))
	for i, t := range cands {
		kind := "agent"
		if t.route.Job != "" {
			kind = "job"
		}
		out[i] = policies.RouteChoice{Key: fmt.Sprintf("r%d", i+1), RouteID: t.tr.Route, Target: t.route.TargetName(), TargetKind: kind, UseFor: t.route.UseFor}
	}
	return out
}

// routeInput is what the router sees of ev and a routes file.
func (s *Service) routeInput(ev eventRecord, f routing.File, cands []cachedTrig) policies.RouteInput {
	title := ev.slug
	if c, err := s.Lookup(ev.slug); err == nil {
		title = c.DisplayTitle(title)
	}
	return policies.RouteInput{
		Connection: ev.slug, ConnectionTitle: title, Notes: f.Notes, Choices: routeChoices(cands),
		SureAt: f.SureAt, Unsure: f.Unsure, NoFit: f.NoFit,
		From: ev.fromLabel(), Subject: ev.routeMessage().Subject, Room: cmp.Or(ev.roomName, ev.room), Text: ev.text,
	}
}

// decideRoute asks the router about one deciding conversation and applies
// the pick. It returns only store errors.
func (s *Service) decideRoute(ctx context.Context, c ConversationChoice) error {
	ev, err := s.loadRecord(ctx, c.FirstEventID)
	if err != nil {
		s.routeMu.Lock()
		defer s.routeMu.Unlock()
		return s.dropChoice(ctx, c, "the first event is gone")
	}
	if !s.prepareEvent(&ev) {
		return nil
	}
	chosen := s.chosenRoutes(ctx, c.Workspace, c.Connection)
	cands := s.candidates(ctx, ev, chosen, false)
	if len(cands) < 2 {
		return s.settleSmall(ctx, c, ev, cands)
	}
	in := s.routeInput(ev, *cands[0].file, cands)
	state := policies.RouteState(in)
	d, answer, failed, err := s.askRouter(ctx, c, state)
	if err != nil {
		return s.routeAttempt(ctx, c, ev, in, err)
	}
	pick := policies.PickRoute(answer, state)
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	cur, ok, err := s.loadChoice(ctx, s.st.DB(), c.Workspace, c.Connection, c.Key)
	if err != nil || !ok || cur.Revision != c.Revision || cur.Status != ChoiceDeciding {
		return err // a person or a newer event changed the conversation
	}
	cur.DecisionID, cur.PickRouteID, cur.Attempts = d.ID, pickedRoute(pick), 0
	switch {
	case failed || pick.Outcome == policies.RouteAsk:
		return s.askPerson(ctx, cur, ev, in, d, answer, failed)
	case pick.Outcome == policies.RouteNobody:
		return s.settle(ctx, cur, ev, cands, nil, routeVia{Method: policies.MethodRouter, DecisionID: d.ID, Share: pick.Share})
	}
	t, ok := find(cands, pick.RouteID)
	if !ok {
		return s.askPerson(ctx, cur, ev, in, d, answer, true)
	}
	return s.settle(ctx, cur, ev, cands, &t, routeVia{Method: pick.Method, DecisionID: d.ID, Share: pick.Share})
}

// askRouter runs the route decision. failed is true when no usable answer
// exists; err is set only when the call itself failed.
func (s *Service) askRouter(ctx context.Context, c ConversationChoice, state decisions.State) (d decisions.Decision, a decisions.Answer, failed bool, err error) {
	if s.routeDecider == nil {
		return d, a, true, nil
	}
	d, err = s.routeDecider.Decide(ctx, c.Workspace, policies.RouteKind,
		decisions.Subject{Kind: policies.RouteSubject, ID: c.FirstEventID, Scope: c.Connection}, state)
	if err != nil {
		return d, a, false, err
	}
	if i := slices.IndexFunc(d.Answers, func(x decisions.Answer) bool { return x.ID == "route" }); i >= 0 {
		a = d.Answers[i]
	}
	return d, a, d.Status != decisions.StatusOK, nil
}

// pickedRoute is the route that the router named: a route id, or nobody.
func pickedRoute(p policies.RoutePick) string {
	if p.Key == policies.RouteNobodyKey {
		return policies.RouteNobodyKey
	}
	return p.RouteID
}

// routeAttempt counts a decide call that failed with no decision. After
// the last attempt, a person chooses.
func (s *Service) routeAttempt(ctx context.Context, c ConversationChoice, ev eventRecord, in policies.RouteInput, cause error) error {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	cur, ok, err := s.loadChoice(ctx, s.st.DB(), c.Workspace, c.Connection, c.Key)
	if err != nil || !ok || cur.Revision != c.Revision || cur.Status != ChoiceDeciding {
		return err
	}
	cur.Attempts++
	s.log.Warn("route decision failed", "event", ev.id, "attempt", cur.Attempts, "error", cause)
	if cur.Attempts >= routerMaxAttempts {
		return s.askPerson(ctx, cur, ev, in, decisions.Decision{}, decisions.Answer{}, true)
	}
	return s.st.WithTx(ctx, func(tx *sql.Tx) error { return s.saveChoice(ctx, tx, cur) })
}

// askPerson sets the conversation to waiting and raises the Inbox item.
func (s *Service) askPerson(ctx context.Context, c ConversationChoice, ev eventRecord, in policies.RouteInput, d decisions.Decision, a decisions.Answer, failed bool) error {
	w := RouteWait{
		Workspace: c.Workspace, Connection: c.Connection, ConnectionTitle: in.ConnectionTitle, EventID: ev.id, DecisionID: d.ID,
		Summary: strings.TrimSpace(ev.fromLabel() + ": " + in.Subject), TraceID: ev.trace, Failed: failed,
	}
	for _, ch := range in.Choices {
		w.Choices = append(w.Choices, RouteWaitChoice{RouteID: ch.RouteID, Target: ch.Target, TargetKind: ch.TargetKind, Share: a.Distribution[ch.Key]})
	}
	slices.SortStableFunc(w.Choices, func(x, y RouteWaitChoice) int { return cmp.Compare(y.Share, x.Share) })
	c.Status = ChoiceWaiting
	if s.onRouteWait != nil {
		id, err := s.onRouteWait(ctx, w)
		if err != nil {
			s.log.Warn("route wait hook", "event", ev.id, "error", err)
		}
		c.InboxID = id
	}
	err := s.saveChoiceBus(ctx, c, ev, "waiting", func(tx *sql.Tx) error {
		return s.waitOutcomes(ctx, tx, c, "waiting for a person")
	})
	if errors.Is(err, errChoiceRace) {
		return nil
	}
	return err
}

// waitOutcomes sets every route_wait row of the conversation to the
// decision of c.
func (s *Service) waitOutcomes(ctx context.Context, tx *sql.Tx, c ConversationChoice, msg string) error {
	ids, err := s.waitingEvents(ctx, tx, c)
	if err != nil {
		return err
	}
	for _, eid := range ids {
		for _, o := range eid.rows {
			if err := upsertOutcomeIn(ctx, tx, eid.id, c.Workspace, o.jobID, o.routeID, dispatch.OutcomeRouteWait,
				routeVia{DecisionID: c.DecisionID}, "", msg, o.at); err != nil {
				return err
			}
		}
	}
	return nil
}

type waitRow struct{ jobID, routeID, at string }

type waitEvent struct {
	id   string
	rows []waitRow
}

// querier reads rows inside a transaction or from the database.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// waitingEvents lists the events of the conversation that have route_wait
// rows, oldest first.
func (s *Service) waitingEvents(ctx context.Context, q querier, c ConversationChoice) ([]waitEvent, error) {
	rows, err := q.QueryContext(ctx, `SELECT o.event_id, o.job_id, o.route_id, o.created_at, COALESCE(e.room,''), COALESCE(e.root,''), COALESCE(e.thread,'')
		FROM event_outcomes o JOIN events e ON e.id = o.event_id
		WHERE o.workspace_id=? AND o.outcome=? AND e.instance = (SELECT instance FROM events WHERE id=?)
		ORDER BY e.received_at, e.id`, c.Workspace, string(dispatch.OutcomeRouteWait), c.FirstEventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []waitEvent
	for rows.Next() {
		var eid, jobID, routeID, at, room, root, thread string
		if err := rows.Scan(&eid, &jobID, &routeID, &at, &room, &root, &thread); err != nil {
			return nil, err
		}
		rec := eventRecord{id: eid, room: room, root: root, thread: thread}
		if rec.routeKey() != c.Key {
			continue
		}
		if n := len(out); n == 0 || out[n-1].id != eid {
			out = append(out, waitEvent{id: eid})
		}
		out[len(out)-1].rows = append(out[len(out)-1].rows, waitRow{jobID, routeID, at})
	}
	return out, rows.Err()
}

// settle ends the wait of a conversation. With t set, t gets every waiting
// event in order; with t nil, nobody does. It saves the final choice first.
func (s *Service) settle(ctx context.Context, c ConversationChoice, first eventRecord, cands []cachedTrig, t *cachedTrig, via routeVia) error {
	c.Method, c.RuleIndex = via.Method, via.RuleIndex
	if t != nil {
		c.Status, c.RouteID, c.Target = ChoiceChosen, t.tr.Route, t.route.TargetName()
	} else {
		c.Status, c.RouteID, c.Target = ChoiceNobody, "", ""
	}
	if err := s.saveChoiceBus(ctx, c, first, cmp.Or(via.Action, "routed"), nil); err != nil {
		if errors.Is(err, errChoiceRace) {
			return nil
		}
		return err
	}
	events, err := s.waitingEvents(ctx, s.st.DB(), c)
	if err != nil {
		return err
	}
	for _, we := range events {
		ev, err := s.loadRecord(ctx, we.id)
		if err != nil {
			s.log.Warn("route settle", "event", we.id, "error", err)
			continue
		}
		if !s.prepareEvent(&ev) {
			continue
		}
		v := via
		v.FollowUp = we.id != c.FirstEventID
		now := time.Now().UTC().Format(time.RFC3339Nano)
		ev.receivedAt = now
		for _, o := range we.rows {
			if t != nil && o.routeID == t.tr.Route {
				continue
			}
			s.upsertOutcome(ctx, we.id, c.Workspace, o.jobID, o.routeID, dispatch.OutcomeNotChosen, v, "", "another route got this message", now)
		}
		if t != nil {
			s.dispatchTrigger(ctx, ev, *t, v)
		}
	}
	return nil
}

// settleSmall ends the wait when the routes changed: one candidate gets the
// conversation, and none drops the choice.
func (s *Service) settleSmall(ctx context.Context, c ConversationChoice, ev eventRecord, cands []cachedTrig) error {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	cur, ok, err := s.loadChoice(ctx, s.st.DB(), c.Workspace, c.Connection, c.Key)
	if err != nil || !ok || cur.Revision != c.Revision || cur.Status != ChoiceDeciding {
		return err
	}
	if len(cands) == 1 {
		return s.settle(ctx, cur, ev, cands, &cands[0], routeVia{Method: MethodOnly})
	}
	return s.dropChoice(ctx, cur, "no route is left to choose")
}

// dropChoice deletes the choice and ends the waiting outcomes as no_match.
// The caller holds routeMu.
func (s *Service) dropChoice(ctx context.Context, c ConversationChoice, msg string) error {
	events, err := s.waitingEvents(ctx, s.st.DB(), c)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, we := range events {
		for _, o := range we.rows {
			s.upsertOutcome(ctx, we.id, c.Workspace, o.jobID, o.routeID, dispatch.OutcomeNoMatch, routeVia{}, "", msg, now)
		}
	}
	_, err = s.st.DB().ExecContext(ctx, `DELETE FROM route_choices WHERE workspace_id=? AND connection=? AND conversation_key=? AND revision=?`,
		c.Workspace, c.Connection, c.Key, c.Revision)
	return err
}

// WaitingDecisionIDs lists the decisions of the conversations that wait for
// a person. An empty ws or connection matches all.
func (s *Service) WaitingDecisionIDs(ctx context.Context, ws, connection string) ([]string, error) {
	q := `SELECT decision_id FROM route_choices WHERE status=? AND decision_id IS NOT NULL AND decision_id <> ''`
	args := []any{string(ChoiceWaiting)}
	for col, v := range map[string]string{"workspace_id": ws, "connection": connection} {
		if v != "" {
			q += " AND " + col + "=?"
			args = append(args, v)
		}
	}
	rows, err := s.st.DB().QueryContext(ctx, q+" ORDER BY updated_at", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PruneChoices deletes the choices not changed for 180 days. It keeps the
// conversations that wait.
func (s *Service) PruneChoices(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.st.DB().ExecContext(ctx, `DELETE FROM route_choices WHERE updated_at < ? AND status NOT IN (?, ?)`,
		now.Add(-choiceKeep).UTC().Format(time.RFC3339Nano), string(ChoiceWaiting), string(ChoiceDeciding))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
