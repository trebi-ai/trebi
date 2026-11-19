package connectors

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/trebi-ai/trebi/internal/decisions"
	"github.com/trebi-ai/trebi/internal/decisions/policies"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// ErrBadInput is a route test call that cannot run.
var ErrBadInput = errors.New("bad route test input")

// MaxRouteTestEvents is the most events that one route test takes.
const MaxRouteTestEvents = 20

// The steps of a route test, in the order of routeEvent.
const (
	StepFilters      = "filters"
	StepEveryMessage = "every_message"
	StepConversation = "conversation"
	StepOnly         = "only"
	StepRules        = "rules"
	StepRouter       = "router"
)

// RouteTestInput is one test of the routing of a connection. Give stored
// events, or the text of a message. Routing is a draft patch of the routes
// file; nil tests the stored file.
type RouteTestInput struct {
	Workspace, Connection     string
	EventIDs                  []string
	Text, From, Subject, Room string
	Routing                   *routing.Patch
}

// RouteTestStep is what one step of the routing decided.
type RouteTestStep struct {
	Step   string `json:"step"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
}

// RouteShare is the share that the router gives to one route.
type RouteShare struct {
	RouteID    string  `json:"route_id"`
	Target     string  `json:"target"`
	TargetKind string  `json:"target_kind"`
	Share      float64 `json:"share"`
}

// RouteTestResult is the routing of one message. Outcome is chosen, nobody,
// ask, route_wait, nothing, off, or not_found.
type RouteTestResult struct {
	EventID     string              `json:"event_id,omitempty"`
	Steps       []RouteTestStep     `json:"steps"`
	Outcome     string              `json:"outcome"`
	RouteID     string              `json:"route_id,omitempty"`
	Target      string              `json:"target,omitempty"`
	TargetKind  string              `json:"target_kind,omitempty"`
	Method      string              `json:"method,omitempty"`
	RuleIndex   int                 `json:"rule_index,omitempty"`
	Shares      []RouteShare        `json:"shares,omitempty"`
	NobodyShare float64             `json:"nobody_share"`
	Reason      string              `json:"reason,omitempty"`
	Decider     string              `json:"decider,omitempty"`
	CostUSD     float64             `json:"cost_usd"`
	Tokens      pricing.TokenTotals `json:"tokens"`
	Before      *EventRouting       `json:"before,omitempty"`
}

// RouteTestReport is the answer of RouteTest. Problems are the faults of the
// draft file.
type RouteTestReport struct {
	Results  []RouteTestResult `json:"results"`
	Problems []routing.Problem `json:"problems"`
}

// EventRouting is the stored routing of the conversation of an event.
// Share is the share of the router for the chosen route; the caller fills it
// from the decision.
type EventRouting struct {
	Workspace  string       `json:"workspace"`
	Connection string       `json:"connection"`
	Key        string       `json:"conversation_key"`
	Status     ChoiceStatus `json:"status"`
	RouteID    string       `json:"route_id,omitempty"`
	Target     string       `json:"target,omitempty"`
	TargetKind string       `json:"target_kind,omitempty"`
	Method     string       `json:"method,omitempty"`
	RuleIndex  int          `json:"rule_index"`
	DecisionID string       `json:"decision_id,omitempty"`
	Share      float64      `json:"share"`
	Wrong      bool         `json:"wrong"`
	InboxID    string       `json:"inbox_id,omitempty"`
}

// routeTester holds the draft file and its routes for one RouteTest call.
type routeTester struct {
	s    *Service
	file routing.File
	live []cachedTrig // routes that dispatch looks at
	slug string
}

// RouteTest runs the routing steps for stored events or for one text, on the
// stored routes file or on a draft of it. It starts no run and stores
// nothing. The router step calls the decider, which costs money.
func (s *Service) RouteTest(ctx context.Context, in RouteTestInput) (RouteTestReport, error) {
	rep := RouteTestReport{Results: []RouteTestResult{}, Problems: []routing.Problem{}}
	switch {
	case len(in.EventIDs) == 0 && strings.TrimSpace(in.Text) == "":
		return rep, fmt.Errorf("%w: give event ids or a text", ErrBadInput)
	case len(in.EventIDs) > MaxRouteTestEvents:
		return rep, fmt.Errorf("%w: a test takes at most %d events", ErrBadInput, MaxRouteTestEvents)
	case s.ws == nil:
		return rep, fmt.Errorf("%w: no workspace service", ErrBadInput)
	}
	id := s.IDOf(in.Connection)
	if id == "" {
		return rep, fmt.Errorf("connector %q not found: %w", in.Connection, ErrNotFound)
	}
	t := &routeTester{s: s, slug: cmp.Or(s.SlugOf(id), in.Connection)}
	var err error
	if t.file, err = s.draftFile(ctx, in.Workspace, t.slug, in.Routing); err != nil {
		return rep, err
	}
	if err := t.load(ctx, in.Workspace, &rep); err != nil {
		return rep, err
	}
	for _, evID := range in.EventIDs {
		rec, err := s.loadRecord(ctx, evID)
		if errors.Is(err, ErrEventNotFound) {
			rep.Results = append(rep.Results, RouteTestResult{EventID: evID, Steps: []RouteTestStep{}, Outcome: "not_found"})
			continue
		}
		if err != nil {
			return rep, err
		}
		s.prepareEvent(&rec)
		rep.Results = append(rep.Results, t.run(ctx, in.Workspace, rec))
	}
	if strings.TrimSpace(in.Text) != "" {
		rec := eventRecord{
			connector: id, instance: id, slug: t.slug, eventType: "message", text: in.Text, sender: in.From, identity: in.From,
			subject: in.Subject, room: in.Room,
			payload: map[string]any{"text": in.Text, "from": in.From, "subject": in.Subject, "room": in.Room},
		}
		rep.Results = append(rep.Results, t.run(ctx, in.Workspace, rec))
	}
	return rep, nil
}

// draftFile is the stored routes file with the draft patch applied. A draft
// that does not parse is ErrBadInput with the problem in the error text.
func (s *Service) draftFile(ctx context.Context, ws, slug string, p *routing.Patch) (routing.File, error) {
	if p == nil {
		return s.ws.Routing(ctx, ws, slug)
	}
	f, err := s.ws.PreviewRouting(ctx, ws, slug, *p)
	var ve *routing.ValidationError
	if errors.As(err, &ve) {
		return f, fmt.Errorf("%w: %w", ErrBadInput, ve)
	}
	return f, err
}

// load joins the routes of the draft to their targets and lists the faults.
func (t *routeTester) load(ctx context.Context, ws string, rep *RouteTestReport) error {
	rows, err := t.s.ws.ListJobs(ctx, "")
	if err != nil {
		return err
	}
	targets := routeTargetRows(rows)
	parsed := map[string]*job.Job{}
	rr := workspaces.RouteRow{WorkspaceID: ws, File: t.file}
	for _, r := range t.file.Routes {
		if c := t.s.cacheRoute(rr, r, targets, parsed); c != nil && c.live() {
			t.live = append(t.live, *c)
		}
	}
	checks, err := t.s.ws.RouteTargets(ctx, ws)
	if err != nil {
		return err
	}
	rep.Problems = append(rep.Problems, t.file.Problems...)
	rep.Problems = append(rep.Problems, routing.Check(t.file, checks)...)
	return nil
}

// run routes one message on the draft. It never writes.
func (t *routeTester) run(ctx context.Context, ws string, ev eventRecord) RouteTestResult {
	res := RouteTestResult{EventID: ev.id, Steps: []RouteTestStep{}}
	if ev.id != "" {
		if rs, err := t.s.EventRouting(ctx, ev.id); err == nil {
			for _, r := range rs {
				if r.Workspace == ws {
					res.Before = &r
				}
			}
		}
	}
	if t.file.Mode != routing.ModeOn {
		return t.runOff(ctx, ev, res)
	}
	var chosen []cachedTrig
	for _, c := range t.live {
		if c.choosable() {
			chosen = append(chosen, c)
		}
	}
	t.everyMessage(ctx, ev, &res)
	if len(chosen) == 0 {
		res.step(StepFilters, "none", "No route gets a chosen message.")
		res.Outcome, res.Method = "nothing", MethodNothing
		return res
	}
	cands := t.s.candidates(ctx, ev, chosen, false)
	res.step(StepFilters, passWord(len(cands)), names(cands, "pass the filters of this message"))
	if t.conversation(ctx, ws, ev, chosen, &res) {
		return res
	}
	switch len(cands) {
	case 0:
		res.step(StepOnly, "none", "No route takes this message.")
		res.Outcome, res.Method = "nothing", MethodNothing
		return res
	case 1:
		res.step(StepOnly, "only", cands[0].route.TargetName()+" is the only route that takes this message.")
		res.pick(cands[0], MethodOnly)
		return res
	}
	res.step(StepOnly, "many", fmt.Sprintf("%d routes take this message.", len(cands)))
	if t.rules(ev, cands, &res) {
		return res
	}
	t.router(ctx, ev, cands, &res)
	return res
}

// runOff reports the routes that run when routing is off.
func (t *routeTester) runOff(ctx context.Context, ev eventRecord, res RouteTestResult) RouteTestResult {
	res.Outcome = "off"
	res.Reason = "Routing is off. Each route that gets every message and passes its filters runs."
	t.everyMessage(ctx, ev, &res)
	return res
}

// everyMessage lists the routes that get every message and pass.
func (t *routeTester) everyMessage(ctx context.Context, ev eventRecord, res *RouteTestResult) {
	var pass []cachedTrig
	for _, c := range t.live {
		if c.route.Gets == routing.GetsEveryMessage && c.broken == "" &&
			GateFor(c.tr, ev.connector).WithInputs(c.j, c.tr).Eval(ctx, ev.gateInput(), ev.payload).Matched {
			pass = append(pass, c)
		}
	}
	res.step(StepEveryMessage, passWord(len(pass)), names(pass, "run at once"))
}

// conversation reads the stored choice of the conversation. It reports
// whether the choice ends the test.
func (t *routeTester) conversation(ctx context.Context, ws string, ev eventRecord, chosen []cachedTrig, res *RouteTestResult) bool {
	if ev.id == "" {
		res.step(StepConversation, "none", "A test message has no conversation.")
		return false
	}
	c, found, err := t.s.loadChoice(ctx, t.s.st.DB(), ws, t.slug, ev.routeKey())
	switch {
	case err != nil || !found:
		res.step(StepConversation, "none", "This conversation has no choice yet.")
	case c.Status == ChoiceChosen:
		if tr, ok := find(chosen, c.RouteID); ok {
			res.step(StepConversation, "follows", "The conversation goes to "+c.Target+".")
			res.pick(tr, MethodConversation)
			return true
		}
		res.step(StepConversation, "gone", "The route of this conversation is not in the file. Trebi chooses again.")
	case c.Status == ChoiceWaiting || c.Status == ChoiceDeciding:
		res.step(StepConversation, "waiting", "The conversation waits for a choice.")
		res.Outcome = "route_wait"
		return true
	default:
		res.step(StepConversation, "nobody", "Nobody got this conversation. Trebi chooses again.")
	}
	return false
}

// rules finds the first rule that fits. It reports whether a rule decided.
func (t *routeTester) rules(ev eventRecord, cands []cachedTrig, res *RouteTestResult) bool {
	msg := ev.routeMessage()
	for i, r := range t.file.Rules {
		if !r.Fits(msg) {
			continue
		}
		if r.To == routing.RuleNobody {
			res.step(StepRules, "nobody", fmt.Sprintf("Rule %d sends this message to nobody.", i+1))
			res.Outcome, res.Method, res.RuleIndex = "nobody", MethodRule, i+1
			return true
		}
		if c, ok := candidateFor(cands, r.To); ok {
			res.step(StepRules, "rule", fmt.Sprintf("Rule %d sends this message to %s.", i+1, r.To))
			res.pick(c, MethodRule)
			res.RuleIndex = i + 1
			return true
		}
	}
	res.step(StepRules, "none", "No rule fits this message.")
	return false
}

// router asks the decider with a test call. The call stores no decision.
func (t *routeTester) router(ctx context.Context, ev eventRecord, cands []cachedTrig, res *RouteTestResult) {
	in := t.s.routeInput(ev, t.file, cands)
	state := policies.RouteState(in)
	res.Outcome = string(policies.RouteAsk)
	if t.s.routeDecider == nil {
		res.step(StepRouter, "ask", "This build has no router. A person chooses.")
		return
	}
	d, err := t.s.routeDecider.Test(ctx, policies.RouteKind, state)
	res.Decider, res.CostUSD, res.Tokens = d.Decider, d.CostUSD, d.Tokens
	if err != nil || d.Status != decisions.StatusOK {
		res.step(StepRouter, "failed", "The router did not finish. A person chooses.")
		res.Reason = cmp.Or(d.Error, "The router did not finish.")
		return
	}
	var a decisions.Answer
	if i := slices.IndexFunc(d.Answers, func(x decisions.Answer) bool { return x.ID == "route" }); i >= 0 {
		a = d.Answers[i]
	}
	pick := policies.PickRoute(a, state)
	res.NobodyShare = a.Distribution[policies.RouteNobodyKey]
	for _, ch := range in.Choices {
		res.Shares = append(res.Shares, RouteShare{RouteID: ch.RouteID, Target: ch.Target, TargetKind: ch.TargetKind, Share: a.Distribution[ch.Key]})
	}
	res.Reason = pick.Reason(state)
	switch pick.Outcome {
	case policies.RouteNobody:
		res.step(StepRouter, "nobody", res.Reason)
		res.Outcome, res.Method = "nobody", pick.Method
	case policies.RouteAsk:
		res.step(StepRouter, "ask", res.Reason)
	default:
		c, ok := find(cands, pick.RouteID)
		if !ok {
			res.step(StepRouter, "ask", "The router named a route that is not a choice. A person chooses.")
			return
		}
		res.step(StepRouter, pick.Method, res.Reason)
		res.pick(c, pick.Method)
	}
}

func (r *RouteTestResult) step(step, result, detail string) {
	r.Steps = append(r.Steps, RouteTestStep{Step: step, Result: result, Detail: detail})
}

// pick sets the chosen route.
func (r *RouteTestResult) pick(c cachedTrig, method string) {
	r.Outcome, r.Method = "chosen", method
	r.RouteID, r.Target, r.TargetKind = c.tr.Route, c.route.TargetName(), kindOf(c)
}

func passWord(n int) string {
	if n == 0 {
		return "none"
	}
	return "pass"
}

// names lists the targets of ts in one sentence that ends with what.
func names(ts []cachedTrig, what string) string {
	if len(ts) == 0 {
		return "No route " + what + "."
	}
	list := make([]string, len(ts))
	for i, c := range ts {
		list[i] = c.route.TargetName()
	}
	return strings.Join(list, ", ") + " " + what + "."
}

// EventRouting reads the stored routing of the conversation of an event, one
// entry for each workspace that has a choice.
func (s *Service) EventRouting(ctx context.Context, eventID string) ([]EventRouting, error) {
	ev, err := s.loadRecord(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if !s.prepareEvent(&ev) {
		return nil, nil
	}
	rows, err := s.st.DB().QueryContext(ctx, `SELECT `+choiceCols+` FROM route_choices
		WHERE connection=? AND conversation_key=? ORDER BY workspace_id`, ev.slug, ev.routeKey())
	if err != nil {
		return nil, err
	}
	var choices []ConversationChoice
	for rows.Next() {
		c, err := scanChoice(rows)
		if err != nil {
			_ = rows.Close() //nolint:errcheck // the scan error wins
			return nil, err
		}
		choices = append(choices, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close() //nolint:errcheck // the read error wins
		return nil, err
	}
	_ = rows.Close() //nolint:errcheck // the rows are read
	out := make([]EventRouting, 0, len(choices))
	for _, c := range choices {
		out = append(out, s.eventRouting(ctx, c))
	}
	return out, nil
}

func (s *Service) eventRouting(ctx context.Context, c ConversationChoice) EventRouting {
	r := EventRouting{
		Workspace: c.Workspace, Connection: c.Connection, Key: c.Key, Status: c.Status, RouteID: c.RouteID, Target: c.Target,
		Method: c.Method, RuleIndex: c.RuleIndex, DecisionID: c.DecisionID, InboxID: c.InboxID,
	}
	for _, t := range s.workspaceRoutes(ctx, c.Workspace, c.Connection) {
		if t.tr.Route == c.RouteID {
			r.TargetKind = kindOf(t)
		}
	}
	if c.Method == MethodYou && c.PickRouteID != "" {
		r.Wrong = cmp.Or(c.RouteID, policies.RouteNobodyKey) != c.PickRouteID
	}
	return r
}

// ChoiceOfDecision reads the conversation choice that holds a decision.
func (s *Service) ChoiceOfDecision(ctx context.Context, decisionID string) (ConversationChoice, bool, error) {
	c, err := scanChoice(s.st.DB().QueryRowContext(ctx, `SELECT `+choiceCols+` FROM route_choices WHERE decision_id=? LIMIT 1`, decisionID))
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationChoice{}, false, nil
	}
	return c, err == nil, err
}

// EventBrief is the part of an event that a person needs to judge a route.
type EventBrief struct {
	ID         string `json:"id"`
	Sender     string `json:"sender"`
	Subject    string `json:"subject"`
	Text       string `json:"text"`
	ReceivedAt string `json:"received_at"`
}

// briefTextCap is the most characters of the message text in an EventBrief.
const briefTextCap = 500

// EventBrief reads the sender, subject, and first text of a stored event.
func (s *Service) EventBrief(ctx context.Context, eventID string) (EventBrief, error) {
	ev, err := s.loadRecord(ctx, eventID)
	if err != nil {
		return EventBrief{}, err
	}
	text := []rune(ev.text)
	if len(text) > briefTextCap {
		text = text[:briefTextCap]
	}
	return EventBrief{ID: ev.id, Sender: ev.fromLabel(), Subject: ev.routeMessage().Subject, Text: string(text), ReceivedAt: ev.receivedAt}, nil
}
