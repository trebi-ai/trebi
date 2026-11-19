package connectors

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// Gate is the ingress check of one event trigger: room, sender, from,
// contains, extract, match. The pipeline and the rule tester share it.
type Gate struct {
	// Types are the event types that the trigger takes; empty takes all.
	Types     []string
	Rooms     []string
	Allowlist []string
	From      string   // job.FromPeople | job.FromBots | job.FromAnyone; empty is anyone
	Contains  []string // any entry, case-insensitive
	Match     string
	Extract   string
	Fold      bool // compare senders without case (native email)
	// job and trigger fill the job inputs after a match; a nil job skips it.
	job     *job.Job
	trigger job.Trigger
}

// Stage names the step that stopped an event.
const (
	StageType     = "type"
	StageRoom     = "room"
	StageSender   = "sender"
	StageFrom     = "from"
	StageContains = "contains"
	StageExtract  = "extract"
	StageMatch    = "match"
	StageInputs   = "inputs"
)

// GateInput is what the gate reads from an event, apart from the payload.
type GateInput struct {
	Type     string
	Room     string
	Identity string
	Text     string
	Bot      bool
	FollowUp bool // the event continues an open conversation (plan A.9)
	// Vars are the built-in vars of the event; the inputs stage reads them.
	Vars map[string]string
}

// GateResult is the outcome of one gate check.
type GateResult struct {
	Matched   bool
	Stage     string // empty when Matched
	FollowUp  bool
	Vars      map[string]string
	Extracted any // extract output; nil when there is no extract
	Error     string
	// Filled is the inputs of the job after a match; nil with no job.
	Filled *job.Filled
}

// GateFor builds the gate of an event trigger. connector is the type of the
// connection that sent the event.
func GateFor(tr job.Trigger, connector string) Gate {
	return Gate{
		Types: tr.Types, Rooms: tr.Rooms, Allowlist: tr.Allowlist, From: tr.From, Contains: tr.Contains,
		Match: tr.Match, Extract: tr.Extract, Fold: connector == NativeEmail,
	}
}

// WithInputs adds the inputs stage: after a match, the gate fills the
// inputs of j for tr. An automatic trigger with an empty required input or
// a bad value does not match.
func (g Gate) WithInputs(j *job.Job, tr job.Trigger) Gate {
	g.job, g.trigger = j, tr
	return g
}

// Eval checks type → room → sender → from → contains → extract → match →
// inputs. A follow-up skips from, contains, and the result of match;
// extract still gives the vars.
func (g Gate) Eval(ctx context.Context, in GateInput, payload any) GateResult {
	if len(g.Types) > 0 && !slices.Contains(g.Types, in.Type) {
		return GateResult{Stage: StageType}
	}
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
	case g.job != nil:
		g.fill(&out, in.Vars, payload)
	}
	return out
}

// fill runs the inputs stage on a matched result.
func (g Gate) fill(out *GateResult, vars map[string]string, payload any) {
	f := g.job.Fill(&g.trigger, &job.EventVars{Vars: mergeVars(vars, out.Vars), Payload: payload}, nil)
	out.Filled = &f
	if g.trigger.Manual {
		return
	}
	if msg := f.Problem(g.job.InputList(true)); msg != "" {
		out.Matched, out.Stage, out.Error = false, StageInputs, msg
	}
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
	Instance  string // the connection id
	Slug      string // the connection name that triggers use; stored events only
	Type      string
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
	rec.threadless = s.Threadless(rec.instance)
	ev := rec.gateEvent()
	ev.Slug = s.SlugOf(rec.instance)
	return ev, nil
}

// EventTrigger is one route that targets a job.
type EventTrigger struct {
	Row     workspaces.JobRow
	Job     *job.Job // nil when the target does not parse
	Trigger job.Trigger
	Route   routing.Route
	Broken  string // "" or why dispatch starts no run for the route
}

// EventTriggers lists the routes of the job targets on the connection of a
// stored event that the workspace of the job can see. Paused jobs are in the
// list; routes that get nothing are not.
func (s *Service) EventTriggers(ctx context.Context, ev GateEvent) []EventTrigger {
	if ev.Slug == "" || s.ws == nil {
		return nil
	}
	var out []EventTrigger
	for _, t := range s.triggerIndex(ctx)[ev.Slug] {
		if t.disabled || t.row.Kind != dispatch.KindJob || !s.visibleIn(t.row.WorkspaceID, ev.Slug) {
			continue
		}
		out = append(out, EventTrigger{Row: t.row, Job: t.j, Trigger: t.tr, Route: t.route, Broken: t.broken})
	}
	return out
}

// RouteIssues checks the routes of f against the manifest of the connection:
// the event types, the data fields that the inputs read, the required inputs
// of an automatic route, and the access of the runs that the route starts.
// targets is keyed by short name. A connection that does not resolve gives
// no problems.
func (s *Service) RouteIssues(workspaceID string, f routing.File, targets map[string]routing.Target) []routing.Problem {
	if f.Connection == job.EmailConnector || f.Connection == NativeWebhook {
		return nil
	}
	r, err := s.Resolve(f.Connection, workspaceID)
	if err != nil {
		return nil
	}
	var out []routing.Problem
	for i, rt := range f.Routes {
		t, ok := targets[rt.TargetName()]
		if !ok || t.Job == nil || rt.Broken() {
			continue
		}
		path := rt.Path(i)
		var list job.IssueList
		if rt.Gets != routing.GetsOff {
			list = append(list, r.accessIssues(path, slices.Concat(t.Job.Connectors, t.Kit))...)
		}
		if r.Manifest.Events != nil {
			list = append(list, r.triggerIssues(t.Job, path, rt.Trigger(f.Connection))...)
		}
		for _, is := range list {
			out = append(out, routing.Problem{Code: is.Code, Severity: is.Severity, Path: is.Path, Route: rt.ID, Line: rt.Line, Message: is.Message})
		}
	}
	return out
}

// JobRouteIssues lists the problems of the routes that name the job at the
// index name, and reports whether any route names it. A problem of a
// routes file shows on the job it concerns.
func (s *Service) JobRouteIssues(ctx context.Context, workspaceID, name string) (routed bool, issues job.IssueList) {
	if s.ws == nil {
		return false, nil
	}
	files, err := s.ws.RoutingFiles(ctx, workspaceID)
	if err != nil {
		return false, nil
	}
	targets, err := s.ws.RouteTargets(ctx, workspaceID)
	if err != nil {
		return false, nil
	}
	short := strings.TrimPrefix(name, "agents/")
	for _, f := range files {
		ids := map[string]bool{}
		for _, r := range f.Routes {
			if r.TargetName() == short {
				routed = routed || !r.Broken()
				ids[r.ID] = true
			}
		}
		if len(ids) == 0 {
			continue
		}
		for _, p := range slices.Concat(f.Problems, routing.Check(f, targets), s.RouteIssues(workspaceID, f, targets)) {
			if ids[p.Route] {
				issues = append(issues, job.Issue{
					Path: p.Path, Code: p.Code, Severity: p.Severity,
					Message: "routes/" + f.Connection + ".yaml: " + p.Message,
				})
			}
		}
	}
	return routed, issues
}

// TriggerAccess is what a run that an event trigger starts can do with the
// trigger connection.
type TriggerAccess string

const (
	AccessFull     TriggerAccess = "full"
	AccessReadOnly TriggerAccess = "read_only"
	AccessNone     TriggerAccess = "none"
	// AccessChannel is a connection that answers through its own channel;
	// its access needs no notice.
	AccessChannel TriggerAccess = "channel"
)

// TriggerAccess is the access of a run that r starts. named reports whether
// the job connectors or the agent kit name r.
func (r Resolved) TriggerAccess(named bool) TriggerAccess {
	switch {
	case r.Manifest.Channel != nil:
		return AccessChannel
	case named:
		return AccessFull
	case len(r.ReadOnlyLabels()) > 0:
		return AccessReadOnly
	}
	return AccessNone
}

// ReadOnlyLabels lists the labels of the read-only operations of r.
func (r Resolved) ReadOnlyLabels() []string {
	var out []string
	for _, op := range r.Manifest.Operations() {
		if op.ReadOnly {
			out = append(out, cmp.Or(op.Label, op.Name))
		}
	}
	return out
}

// Named reports whether a list of connection names names r.
func (r Resolved) Named(names []string) bool {
	return slices.ContainsFunc(names, func(n string) bool { return n == r.ID || n == r.Slug })
}

// accessIssues tells the access of the runs that the route at path starts.
func (r Resolved) accessIssues(path string, names []string) job.IssueList {
	switch r.TriggerAccess(r.Named(names)) {
	case AccessReadOnly:
		return job.IssueList{{Path: path, Code: job.CodeTriggerAccessReadOnly, Severity: job.SeverityInfo,
			Message: fmt.Sprintf("Runs that %s starts can read it: %s. They cannot change it.", r.Title, strings.Join(r.ReadOnlyLabels(), ", "))}}
	case AccessNone:
		return job.IssueList{{Path: path, Code: job.CodeTriggerAccessNone, Severity: job.SeverityWarning,
			Message: fmt.Sprintf("%s has no read-only actions, so a run that %s starts gets only the event. To read more or to answer, give full access.", r.Title, r.Title)}}
	}
	return nil
}

// triggerIssues checks the route at path against the manifest of r.
func (r Resolved) triggerIssues(j *job.Job, path string, tr job.Trigger) job.IssueList {
	var out job.IssueList
	add := func(p, code, sev, msg string) {
		out = append(out, job.Issue{Path: p, Code: code, Severity: sev, Message: msg})
	}
	types := tr.Types
	for _, t := range tr.Types {
		if len(r.Manifest.Events.Types) > 0 && !r.Manifest.HasEventType(t) {
			add(path+".types", job.CodeEventTypeUnknown, job.SeverityError, fmt.Sprintf("%s does not send %q events", r.Title, t))
		}
	}
	if len(types) == 0 {
		types = declaredTypes(r.Manifest)
	}
	fields := r.dataFields(types)
	if fields == nil {
		return out
	}
	for _, in := range j.Inputs {
		out = append(out, fields.inputIssues(path+".inputs."+in.Name, in, tr)...)
	}
	return out
}

// eventFields are the top-level data fields of the event types of one
// trigger: the fields of any type, and the fields that each type requires.
type eventFields struct {
	any, all map[string]bool
}

// dataFields reads the data schemas of types. It returns nil when a type
// has no schema, because then a field cannot be checked.
func (r Resolved) dataFields(types []string) *eventFields {
	if r.Dir == "" || len(types) == 0 {
		return nil
	}
	schemas, err := r.Manifest.EventSchemas(r.Dir)
	if err != nil {
		return nil
	}
	f := &eventFields{any: map[string]bool{}}
	for _, t := range types {
		res, ok := schemas[t]
		if !ok {
			return nil
		}
		sch := res.Schema()
		for k := range sch.Properties {
			f.any[k] = true
		}
		req := map[string]bool{}
		for _, k := range sch.Required {
			if f.all == nil || f.all[k] {
				req[k] = true
			}
		}
		f.all = req
	}
	return f
}

// check reports whether the event has the field at path, and whether each
// type requires it.
func (f *eventFields) check(path string) (known, sure bool) {
	top, _, _ := strings.Cut(path, ".")
	return f.any[top], f.all[top]
}

// inputIssues checks how trigger tr fills input in from the event data.
func (f *eventFields) inputIssues(p string, in job.Input, tr job.Trigger) job.IssueList {
	var out job.IssueList
	add := func(code, msg string) {
		out = append(out, job.Issue{Path: p, Code: code, Severity: job.SeverityWarning, Message: msg})
	}
	mv, mapped := tr.Inputs.Get(in.Name)
	if !mapped {
		known, sure := f.check(in.Name)
		switch {
		case tr.Manual || !in.Required || in.Default != nil:
		case !known:
			add(job.CodeRequiredUnfilled, fmt.Sprintf("%s is required, but the event has no %q field; add a value under inputs or a default", in.Title(), in.Name))
		case !sure:
			add(job.CodeMayBeEmpty, fmt.Sprintf("%s: the event can leave %q empty", in.Title(), in.Name))
		}
		return out
	}
	for _, name := range job.TemplateNames(mv.Value) {
		field, ok := strings.CutPrefix(name, "data.")
		if !ok {
			continue
		}
		known, sure := f.check(field)
		switch {
		case !known:
			add(job.CodeFieldUnknown, fmt.Sprintf("%s: the event has no %q field", in.Title(), field))
		case !sure && in.Required && !tr.Manual:
			add(job.CodeMayBeEmpty, fmt.Sprintf("%s: the event can leave %q empty", in.Title(), field))
		}
	}
	return out
}

// ErrNotAccepted is a stored event that no route of a job takes.
var ErrNotAccepted = errors.New("event not accepted")

// ErrNoRoute is a job with no route on the connection of the event, or an
// unknown route id.
var ErrNoRoute = errors.New("no route")

// HandRoute picks the route of a target that takes ev for a run that a
// person starts: the route routeID when it is set, else the first by-hand
// route of the target on the connection of ev, else the first other one. It
// returns ErrNotAccepted when the gate of the route stops ev, and
// ErrNoRoute when the target has no such route.
func (s *Service) HandRoute(ctx context.Context, ev GateEvent, workspaceID, target, routeID string, force bool) (job.Trigger, GateResult, error) {
	target = strings.TrimPrefix(target, "agents/")
	var manual, other []cachedTrig
	for _, t := range s.triggerIndex(ctx)[ev.Slug] {
		if t.row.WorkspaceID != workspaceID || t.disabled || t.broken != "" || t.j == nil {
			continue
		}
		switch {
		case routeID != "" && t.tr.Route != routeID:
		case strings.TrimPrefix(t.row.Name, "agents/") != target:
			if routeID != "" {
				return job.Trigger{}, GateResult{}, fmt.Errorf("%w: route %s is for %s, not %s", ErrNotAccepted, routeID, t.row.Name, target)
			}
		case t.tr.Manual:
			manual = append(manual, t)
		default:
			other = append(other, t)
		}
	}
	last := GateResult{}
	for _, t := range append(manual, other...) {
		res := ev.RunGate(ctx, t.j, t.tr, force)
		if res.Matched {
			return t.tr, res, nil
		}
		last = res
	}
	if len(manual)+len(other) == 0 {
		if routeID != "" {
			return job.Trigger{}, last, fmt.Errorf("%w: %s", ErrNoRoute, routeID)
		}
		return job.Trigger{}, last, fmt.Errorf("%w: %s has no route on this connection", ErrNoRoute, target)
	}
	if routeID != "" {
		return job.Trigger{}, last, fmt.Errorf("%w: route %s stops it at %s", ErrNotAccepted, routeID, cmp.Or(last.Error, last.Stage))
	}
	return job.Trigger{}, last, fmt.Errorf("%w: %s has no route that takes this event", ErrNotAccepted, target)
}

// RunGate checks the gate of trigger tr of j for a run that a person
// starts from ev: no follow-up, and the inputs stage fills as for a manual
// trigger. force skips the match stage. It writes nothing.
func (ev GateEvent) RunGate(ctx context.Context, j *job.Job, tr job.Trigger, force bool) GateResult {
	g := GateFor(tr, ev.Connector)
	if force {
		g.Match = ""
	}
	tr.Manual = true
	return ev.Eval(ctx, g.WithInputs(j, tr), false)
}

// DraftGateEvent maps an event value that is not stored. A value with the
// trebi-connector/1 event shape gives the room, the sender, and the text;
// any other value has none. threadless gives the lane rule of a channel
// without threads.
func DraftGateEvent(connector string, threadless bool, payload any) GateEvent {
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
	rec.threadless = threadless
	return rec.gateEvent()
}

// gateEvent is the gate side of a record.
func (r eventRecord) gateEvent() GateEvent {
	return GateEvent{
		ID: r.id, Connector: r.connector, Instance: r.instance, Type: r.eventType, Room: r.room, Identity: r.identity,
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
	return g.Eval(ctx, GateInput{Type: ev.Type, Room: ev.Room, Identity: ev.Identity, Text: ev.Text, Bot: ev.Bot,
		FollowUp: followUp, Vars: ev.record.vars()}, ev.Payload)
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
