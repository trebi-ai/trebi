package routing

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
	"gopkg.in/yaml.v3"
)

// routeYAML is the file form of one route. The filter keys sit inline.
type routeYAML struct {
	ID              string `yaml:"id,omitempty"`
	Agent           string `yaml:"agent,omitempty"`
	Job             string `yaml:"job,omitempty"`
	Assistant       bool   `yaml:"assistant,omitempty"`
	Gets            string `yaml:"gets"`
	UseFor          string `yaml:"use_for,omitempty"`
	job.TriggerYAML `yaml:",inline"`
}

var (
	topKeys   = []string{"mode", "notes", "routes", "rules", "unsure", "no_fit", "sure_at"}                                                                                                          //nolint:gochecknoglobals // static key order
	routeKeys = []string{"id", "agent", "job", "assistant", "gets", "use_for", "match", "extract", "limit", "allowlist", "rooms", "contains", "from", "replies", "chat", "batch", "types", "inputs"} //nolint:gochecknoglobals // static key order
	ruleKeys  = []string{RuleSenderIs, RuleSenderEnds, RuleSubjectHas, RuleTextHas, RuleRoomIs}                                                                                                      //nolint:gochecknoglobals // static key order
)

const (
	msgRepliesNotJob    = `"replies" applies only to a job trigger; an agent keeps its channel conversation`
	msgChatNotJob       = `"chat" applies only to a job trigger; an agent talks in its own chats`
	msgBatchNotOperator = `"batch" applies only to a job or agent trigger`
)

// Load reads <wsPath>/routes/<connection>.yaml. A missing file is mode off
// with no routes.
func Load(wsPath, connection string) (File, error) {
	path := filepath.Join(wsPath, Dir, connection+".yaml")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return newFile(connection, path), nil
	}
	if err != nil {
		return File{}, fmt.Errorf("read routes %s: %w", path, err)
	}
	return Parse(connection, path, data), nil
}

// LoadDir loads each routes file of a workspace, sorted by connection.
func LoadDir(wsPath string) ([]File, error) {
	entries, err := os.ReadDir(filepath.Join(wsPath, Dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read routes folder: %w", err)
	}
	var out []File
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() {
			continue
		}
		f, err := Load(wsPath, name)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// parser collects the problems of one file.
type parser struct {
	f   *File
	ids map[string]bool
}

func (p *parser) add(path, msg string, line int) {
	p.f.Problems = append(p.f.Problems, Problem{
		Code: CodeInvalidRoute, Severity: job.SeverityError, Path: path, Line: line, Message: msg,
	})
}

// Parse reads the bytes of a routes file. It returns no error: a fault is a
// problem of the file. A file that is not valid YAML has no routes, so the
// dispatch runs nothing from it.
func Parse(connection, path string, data []byte) File {
	f := newFile(connection, path)
	f.Exists, f.Hash = true, job.ContentHash(data)
	p := &parser{f: &f, ids: map[string]bool{}}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		p.add("", "the file is not valid YAML: "+err.Error(), 0)
		return f
	}
	if len(doc.Content) == 0 {
		return f
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return f
	}
	if root.Kind != yaml.MappingNode {
		p.add("", "the file must be a map with the keys mode, routes, and rules", root.Line)
		return f
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		switch k.Value {
		case "mode":
			p.enum(&f.Mode, k.Value, v, ModeOn, ModeOff)
		case "notes":
			p.text(&f.Notes, k.Value, v)
		case "unsure":
			p.enum(&f.Unsure, k.Value, v, UnsureAsk, UnsureGuess)
		case "no_fit":
			p.enum(&f.NoFit, k.Value, v, NoFitDrop, NoFitAsk)
		case "sure_at":
			p.sureAt(v)
		case "routes":
			p.routes(v)
		case "rules":
			p.rules(v)
		default:
			p.add(k.Value, fmt.Sprintf("unknown key %q; use %s", k.Value, strings.Join(topKeys, ", ")), k.Line)
		}
	}
	return f
}

func (p *parser) text(dst *string, key string, v *yaml.Node) {
	if v.Kind != yaml.ScalarNode {
		p.add(key, key+" wants text", v.Line)
		return
	}
	*dst = v.Value
}

func (p *parser) enum(dst *string, key string, v *yaml.Node, allowed ...string) {
	if v.Kind != yaml.ScalarNode || !slices.Contains(allowed, v.Value) {
		p.add(key, fmt.Sprintf("%s %q: use %s", key, v.Value, strings.Join(allowed, " or ")), v.Line)
		return
	}
	*dst = v.Value
}

func (p *parser) sureAt(v *yaml.Node) {
	x, err := strconv.ParseFloat(v.Value, 64)
	if v.Kind != yaml.ScalarNode || err != nil || x <= 0 || x > 1 {
		p.add("sure_at", fmt.Sprintf("sure_at %q: use a number above 0 and up to 1", v.Value), v.Line)
		return
	}
	p.f.SureAt = x
}

func (p *parser) rules(v *yaml.Node) {
	if v.Kind != yaml.SequenceNode {
		if v.Kind != yaml.ScalarNode || v.Tag != "!!null" {
			p.add("rules", "rules wants a list", v.Line)
		}
		return
	}
	for i, item := range v.Content {
		path := fmt.Sprintf("rules[%d]", i)
		r, msg := parseRule(item)
		if msg != "" {
			p.add(path, msg, item.Line)
			continue
		}
		p.f.Rules = append(p.f.Rules, r)
	}
}

func (p *parser) routes(v *yaml.Node) {
	if v.Kind != yaml.SequenceNode {
		if v.Kind != yaml.ScalarNode || v.Tag != "!!null" {
			p.add("routes", "routes wants a list", v.Line)
		}
		return
	}
	for i, item := range v.Content {
		r, path, suffix, msg := p.route(i, item)
		if msg != "" {
			r.Fault = msg
			prob := Problem{Code: CodeInvalidRoute, Severity: job.SeverityError, Path: path + suffix, Line: item.Line, Message: msg}
			if ValidID(r.ID) {
				prob.Route = r.ID
			}
			p.f.Problems = append(p.f.Problems, prob)
		}
		p.f.Routes = append(p.f.Routes, r)
	}
}

// route reads route i. On a fault it returns the path suffix and message of
// the first fault; the route stays in the list as a broken route.
func (p *parser) route(i int, n *yaml.Node) (r Route, path, suffix, msg string) {
	path = fmt.Sprintf("routes[%d]", i)
	r = Route{Line: n.Line}
	if n.Kind != yaml.MappingNode {
		return r, path, "", "a route is a map with a target key and a gets key"
	}
	if id := keyValue(n, "id"); ValidID(id) {
		path = "routes[" + id + "]"
	}
	for j := 0; j+1 < len(n.Content); j += 2 {
		if k := n.Content[j].Value; !slices.Contains(routeKeys, k) {
			return r, path, "." + k, fmt.Sprintf("unknown route key %q", k)
		}
	}
	var y routeYAML
	if err := n.Decode(&y); err != nil {
		return r, path, "", "the route has a bad value: " + err.Error()
	}
	r.ID, r.Agent, r.Job, r.Assistant, r.Gets, r.UseFor = y.ID, shortTarget(y.Agent), shortTarget(y.Job), y.Assistant, y.Gets, y.UseFor
	r.Inputs, y.Inputs = y.Inputs, nil
	r.Filter = y.TriggerYAML
	if r.Gets == "" {
		r.Gets = GetsEveryMessage
	}
	if suffix, msg = p.claimID(y.ID); msg != "" {
		return r, path, suffix, msg
	}
	if suffix, msg = r.checkTarget(); msg != "" {
		return r, path, suffix, msg
	}
	if !slices.Contains([]string{GetsEveryMessage, GetsChosen, GetsByHand, GetsOff}, r.Gets) {
		return r, path, ".gets", fmt.Sprintf("gets %q: use %s, %s, %s, or %s", r.Gets, GetsEveryMessage, GetsChosen, GetsByHand, GetsOff)
	}
	tr, err := job.NormalizeEventTrigger(r.Filter)
	if err != nil {
		return r, path, "", err.Error()
	}
	suffix, msg = r.checkReplyKeys(tr)
	return r, path, suffix, msg
}

// claimID records the id of a route and checks it.
func (p *parser) claimID(id string) (suffix, msg string) {
	switch {
	case id == "":
		return "", ""
	case !ValidID(id):
		return ".id", fmt.Sprintf("id %q: use rt_ and 10 hex digits", id)
	case p.ids[id]:
		return ".id", fmt.Sprintf("id %s is used by an earlier route", id)
	}
	p.ids[id] = true
	return "", ""
}

// checkReplyKeys checks the keys that only a job route takes.
func (r Route) checkReplyKeys(tr job.Trigger) (suffix, msg string) {
	if r.Job != "" && !r.Assistant {
		return "", ""
	}
	switch {
	case tr.Replies == job.RepliesContinue:
		return ".replies", msgRepliesNotJob
	case tr.Chat != "":
		return ".chat", msgChatNotJob
	case tr.BatchMS > 0 && r.Assistant:
		return ".batch", msgBatchNotOperator
	}
	return "", ""
}

// checkTarget checks that the route has exactly one well-formed target.
func (r Route) checkTarget() (suffix, msg string) {
	n := 0
	for _, set := range []bool{r.Agent != "", r.Job != "", r.Assistant} {
		if set {
			n++
		}
	}
	switch {
	case n == 0:
		return "", "a route needs one target: agent, job, or assistant: true"
	case n > 1:
		return "", "a route has one target; found more than one of agent, job, and assistant"
	case r.Agent == job.ReservedAgentNobody:
		return ".agent", fmt.Sprintf("%q is not a valid agent name", r.Agent)
	case r.Agent != "" && strings.Contains(r.Agent, "/"):
		return ".agent", fmt.Sprintf("agent %q: use the agent name only; use job for <agent>/<job>", r.Agent)
	case r.Job != "" && strings.Count(r.Job, "/") != 1:
		return ".job", fmt.Sprintf("job %q: use <agent>/<job>", r.Job)
	}
	return "", ""
}

// Check adds the problems that need the targets of the workspace. targets is
// keyed by the short target name. An archived target is no problem.
func Check(f File, targets map[string]Target) []Problem {
	var out []Problem
	add := func(code, path string, r *Route, msg string, fix *Fix) {
		p := Problem{Code: code, Severity: job.SeverityError, Path: path, Message: msg, Fix: fix}
		if r != nil {
			p.Route, p.Line = r.ID, r.Line
		}
		out = append(out, p)
	}
	chosen := map[string]*Route{}
	anyChosen := false
	for i := range f.Routes {
		r := &f.Routes[i]
		if r.Broken() {
			continue
		}
		path := r.Path(i)
		t, ok := targets[r.TargetName()]
		if !ok {
			add(CodeRouteTargetMissing, path, r, fmt.Sprintf("%s does not exist", r.TargetName()), f.removeRoutesFix(r.ID))
			continue
		}
		if r.Gets == GetsChosen {
			anyChosen = true
			if chosen[r.TargetName()] == nil {
				chosen[r.TargetName()] = r
			}
		}
		if t.Job == nil {
			continue
		}
		for _, is := range t.Job.RouteIssues(path, r.Trigger(f.Connection)) {
			p := Problem{Code: is.Code, Severity: is.Severity, Path: is.Path, Route: r.ID, Line: r.Line, Message: is.Message}
			p.Fix = f.issueFix(r, is)
			out = append(out, p)
		}
	}
	out = append(out, f.replyRoomProblems()...)
	if f.Mode == ModeOn && !anyChosen {
		add(CodeNoChoices, "mode", nil, "routing is on, but no route gets chosen messages",
			&Fix{Action: FixAction, Input: map[string]any{"connection": f.Connection, "mode": ModeOff}})
	}
	return append(out, f.ruleProblems(targets, chosen)...)
}

// ruleProblems checks the target of each rule. chosen holds the first route
// that gets chosen messages, for each target.
func (f File) ruleProblems(targets map[string]Target, chosen map[string]*Route) []Problem {
	var out []Problem
	for i, rule := range f.Rules {
		path := fmt.Sprintf("rules[%d].to", i)
		if rule.To == RuleNobody {
			continue
		}
		prob := Problem{Severity: job.SeverityError, Path: path}
		if _, ok := targets[rule.To]; !ok {
			prob.Code, prob.Message, prob.Fix = CodeRuleTargetMissing, fmt.Sprintf("the rule sends to %s, which does not exist", rule.To), f.dropRuleFix(i)
		} else if f.Mode == ModeOn && chosen[rule.To] == nil {
			prob.Code, prob.Message, prob.Fix = CodeRuleTargetNotChosen, fmt.Sprintf("the rule sends to %s, but no route gets chosen messages for it", rule.To), f.chooseFix(rule.To, targets[rule.To])
		} else {
			continue
		}
		out = append(out, prob)
	}
	return out
}

// replyRoomProblems gives a reply_room_taken problem for each room of a
// "chat: reply" job route that an earlier route of the file claims.
func (f File) replyRoomProblems() []Problem {
	var out []Problem
	claimed := map[string]string{}
	for i, r := range f.Routes {
		if r.Broken() || r.Job == "" || r.Filter.Chat != job.ChatReply || r.Gets == GetsOff {
			continue
		}
		for _, room := range r.Filter.Rooms {
			other, ok := claimed[room]
			if !ok {
				claimed[room] = r.TargetName()
				continue
			}
			if other != r.TargetName() {
				out = append(out, Problem{
					Code: job.CodeReplyRoomTaken, Severity: job.SeverityError, Path: r.Path(i) + ".rooms", Route: r.ID, Line: r.Line,
					Message: fmt.Sprintf("the job %s already replies in room %s; one chat has one job that replies", other, room),
				})
			}
		}
	}
	return out
}

// FixAction is the assistant action that edits a routes file.
const FixAction = "connector.routing_set"

// Path is the problem path of the i-th route: its id, else its position.
func (r Route) Path(i int) string {
	if r.ID != "" {
		return "routes[" + r.ID + "]"
	}
	return fmt.Sprintf("routes[%d]", i)
}

func (f File) removeRoutesFix(id string) *Fix {
	if id == "" {
		return nil
	}
	return &Fix{Action: FixAction, Input: map[string]any{"connection": f.Connection, "remove_routes": []string{id}}}
}

func (f File) dropRuleFix(drop int) *Fix {
	rules := make([]map[string]string, 0, len(f.Rules))
	for i, r := range f.Rules {
		if i != drop {
			rules = append(rules, map[string]string{r.Field: r.Value, "to": r.To})
		}
	}
	return &Fix{Action: FixAction, Input: map[string]any{"connection": f.Connection, "rules": rules}}
}

func (f File) chooseFix(target string, t Target) *Fix {
	patch := map[string]any{"gets": GetsChosen}
	for _, r := range f.Routes {
		if r.TargetName() == target && r.ID != "" && !r.Broken() {
			patch["id"] = r.ID
			return &Fix{Action: FixAction, Input: map[string]any{"connection": f.Connection, "routes": []map[string]any{patch}}}
		}
	}
	switch t.Kind {
	case "job":
		patch["job"] = target
	case "agent":
		patch["agent"] = target
	default:
		patch["assistant"] = true
	}
	return &Fix{Action: FixAction, Input: map[string]any{"connection": f.Connection, "routes": []map[string]any{patch}}}
}

// issueFix removes an input that the job does not declare.
func (f File) issueFix(r *Route, is job.Issue) *Fix {
	if is.Code != job.CodeInputUnknown || r.ID == "" {
		return nil
	}
	name := is.Path[strings.LastIndex(is.Path, ".")+1:]
	rest := map[string]string{}
	for _, v := range r.Inputs {
		if v.Name != name {
			rest[v.Name] = v.Value
		}
	}
	var inputs any
	if len(rest) > 0 {
		inputs = rest
	}
	return &Fix{Action: FixAction, Input: map[string]any{"connection": f.Connection, "routes": []map[string]any{{"id": r.ID, "inputs": inputs}}}}
}

func newFile(connection, path string) File {
	return File{
		Connection: connection, Path: path, Mode: ModeOff,
		Unsure: UnsureAsk, NoFit: NoFitDrop, SureAt: DefaultSureAt,
	}
}

func parseRule(n *yaml.Node) (Rule, string) {
	if n.Kind != yaml.MappingNode {
		return Rule{}, "a rule wants a condition and a to key"
	}
	r := Rule{Line: n.Line}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		switch {
		case v.Kind != yaml.ScalarNode:
			return Rule{}, fmt.Sprintf("rule key %q wants text", k)
		case k == "to":
			r.To = strings.TrimSpace(strings.TrimPrefix(v.Value, "agents/"))
		case slices.Contains(ruleKeys, k):
			if r.Field != "" {
				return Rule{}, fmt.Sprintf("a rule has one condition; found %s and %s", r.Field, k)
			}
			r.Field, r.Value = k, strings.TrimSpace(v.Value)
		default:
			return Rule{}, fmt.Sprintf("unknown rule key %q; use %s, and to", k, strings.Join(ruleKeys, ", "))
		}
	}
	switch {
	case r.Field == "":
		return Rule{}, "a rule needs one condition: " + strings.Join(ruleKeys, ", ")
	case r.Value == "":
		return Rule{}, fmt.Sprintf("rule %s needs a value", r.Field)
	case r.To == "":
		return Rule{}, "a rule needs a to key with a target name or " + RuleNobody
	}
	return r, ""
}

func keyValue(n *yaml.Node, key string) string {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1].Value
		}
	}
	return ""
}
