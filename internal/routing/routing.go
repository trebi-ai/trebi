// Package routing owns the routes files: <workspace>/routes/<connection>.yaml.
// A routes file holds the event triggers of one connection for one workspace.
// The package is a leaf over internal/job. It reads and writes the files and
// checks them; the workspace service finds the files on disk.
package routing

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
)

// Dir is the folder of the routes files in a workspace.
const Dir = "routes"

// Values of the mode key.
const (
	ModeOff = "off"
	ModeOn  = "on"
)

// Values of the gets key of a route.
const (
	GetsEveryMessage = "every_message"
	GetsChosen       = "chosen"
	GetsByHand       = "by_hand"
	GetsOff          = "off"
)

// Values of the unsure and no_fit keys.
const (
	UnsureAsk   = "ask"
	UnsureGuess = "guess"
	NoFitDrop   = "drop"
	NoFitAsk    = "ask"
)

// DefaultSureAt is the sure_at value of a file that sets none.
const DefaultSureAt = 0.7

// The condition keys of a rule, and the target that means no agent.
const (
	RuleSenderIs   = "sender_is"
	RuleSenderEnds = "sender_ends"
	RuleSubjectHas = "subject_has"
	RuleTextHas    = "text_has"
	RuleRoomIs     = "room_is"
	RuleNobody     = "nobody"
)

// Assistant is the target name of the workspace assistant.
const Assistant = "assistant"

// Problem codes.
const (
	CodeInvalidRoute        = "invalid_route"
	CodeRouteTargetMissing  = "route_target_missing"
	CodeRuleTargetMissing   = "rule_target_missing"
	CodeRuleTargetNotChosen = "rule_target_not_chosen"
	CodeNoChoices           = "no_choices"
)

// File is one routes file: <workspace>/routes/<connection>.yaml.
type File struct {
	Connection string // connection slug, from the file name
	Path       string // absolute path
	Exists     bool   // false: no file on disk; the zero File is mode off with no routes
	Hash       string // hash of the bytes, the If-Match value
	Mode       string // ModeOff | ModeOn
	Notes      string
	Routes     []Route
	Rules      []Rule
	Unsure     string  // UnsureAsk | UnsureGuess
	NoFit      string  // NoFitDrop | NoFitAsk
	SureAt     float64 // 0 < SureAt <= 1
	Problems   []Problem
}

// Route is one entry of the routes list.
type Route struct {
	ID        string // rt_<10 hex>; "" until the daemon writes it
	Agent     string // short agent name, or ""
	Job       string // "<agent>/<job>" short form, or ""
	Assistant bool
	Gets      string
	UseFor    string
	// Filter holds the event filter keys only.
	Filter job.TriggerYAML
	Inputs job.InputMap
	Line   int // 1-based line in the file
	// Fault is the reason of an invalid_route problem; "" for a good route.
	Fault string
}

// Rule is one rule. Exactly one condition key is set.
type Rule struct {
	Field string
	Value string
	To    string // a target name (as in Route.TargetName) or RuleNobody
	Line  int
}

// Target is one agent, job, or the assistant that a route can name.
type Target struct {
	Kind        string // "agent" | "job" | "assistant"
	Name        string // index name: "sling-cs", "sling-cs/email-support", "assistant"
	ID          string // job_index id, or the workspace id for the assistant
	Active      bool
	Archived    bool
	Description string
	Kit         []string // connectors of the agent kit of the target
	Job         *job.Job // nil for the assistant or a parse failure
}

// Problem is one fault of a routes file.
type Problem struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Path     string `json:"path"` // "routes[rt_3f9a0c1b2d].rooms", "rules[2].to", "mode", ""
	Route    string `json:"route,omitempty"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
	Fix      *Fix   `json:"fix,omitempty"`
}

// Fix is the assistant action that removes a problem. Label is the text of
// the UI button.
type Fix struct {
	Action string         `json:"action"`
	Label  string         `json:"label,omitempty"`
	Input  map[string]any `json:"input"`
}

// Refs lists the routes and rules that name a set of targets.
type Refs struct {
	Routes []RouteRef `json:"routes"`
	Rules  []RuleRef  `json:"rules"`
}

// RouteRef is one route that names a target. Route is the route id.
type RouteRef struct {
	Connection string `json:"connection"`
	Route      string `json:"route"`
	Target     string `json:"target"`
}

// RuleRef is one rule that names a target. Index is its place in the list.
type RuleRef struct {
	Connection string `json:"connection"`
	Index      int    `json:"index"`
	To         string `json:"to"`
}

// Empty reports whether no route and no rule is listed.
func (r Refs) Empty() bool { return len(r.Routes) == 0 && len(r.Rules) == 0 }

// Exact keeps the routes and rules that name target itself, not its jobs.
func (r Refs) Exact(target string) Refs {
	var out Refs
	for _, x := range r.Routes {
		if x.Target == target {
			out.Routes = append(out.Routes, x)
		}
	}
	for _, x := range r.Rules {
		if x.To == target {
			out.Rules = append(out.Rules, x)
		}
	}
	return out
}

// Describe is the sentence that a delete guard shows for target name.
func (r Refs) Describe(name string) string {
	var routes, rules []string
	for _, x := range r.Routes {
		routes = append(routes, x.Connection)
	}
	for _, x := range r.Rules {
		rules = append(rules, x.Connection)
	}
	var parts []string
	if len(routes) > 0 {
		parts = append(parts, countOf(len(routes), "route")+" ("+strings.Join(uniq(routes), ", ")+")")
	}
	if len(rules) > 0 {
		parts = append(parts, countOf(len(rules), "rule")+" ("+strings.Join(uniq(rules), ", ")+")")
	}
	return "Deleting " + name + " also removes " + strings.Join(parts, " and ") + " from routing."
}

// TargetName is the short target name.
func (r Route) TargetName() string {
	switch {
	case r.Assistant:
		return Assistant
	case r.Job != "":
		return r.Job
	}
	return r.Agent
}

// Broken reports whether the route has an invalid_route problem.
func (r Route) Broken() bool { return r.Fault != "" }

// Trigger is the trigger that the gate and the dispatch use.
func (r Route) Trigger(connection string) job.Trigger {
	tr, _ := job.NormalizeEventTrigger(r.Filter) // Parse rejected a bad filter
	tr.Family, tr.Event, tr.Route = "event", connection, r.ID
	tr.Inputs = r.Inputs
	tr.Manual = r.Gets == GetsByHand
	tr.Disabled = r.Gets == GetsOff
	return tr
}

var idPattern = regexp.MustCompile(`^rt_[0-9a-f]{10}$`)

// NewID mints a route id.
func NewID() string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "rt_" + hex.EncodeToString(b[:])
}

// ValidID reports whether id has the shape of a route id.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// oldPrefix was the start of an agent or job index name. A name in a file
// that still has it is a fault.
const oldPrefix = "agents/"

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func uniq(in []string) []string {
	var out []string
	for _, v := range in {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
