// Package notify owns the notification route model and its resolver (plan
// 2026-09-28 Part C). A route is an ordered list of steps. Alert delivery
// tries the steps in order and stops at the first sent message.
package notify

import (
	"context"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Step is one route entry. Via is "email" or a connector name. To is an email
// address, the token "contact", the token "admins", or a channel ref.
type Step struct {
	Via string `yaml:"via" json:"via"`
	To  string `yaml:"to,omitempty" json:"to,omitempty"`
}

// Layer names where a route or a contact comes from.
type Layer string

const (
	LayerJob       Layer = "job"
	LayerAgent     Layer = "agent"
	LayerWorkspace Layer = "workspace"
	LayerGlobal    Layer = "global"
	LayerDefault   Layer = "default"
)

// Via and To tokens.
const (
	ViaEmail      = "email"
	TokenContact  = "contact"
	TokenAdmins   = "admins"
	maxSteps      = 8
	ProblemEmail  = "email_off"
	ProblemNoAddr = "no_contact"
	ProblemNoConn = "unknown_connector"
	ProblemNoChan = "no_channel"
)

var viaRe = regexp.MustCompile(`^(email|[a-z0-9][a-z0-9-]{0,39})$`)

// Resolved is a route after inheritance, with each email step expanded to
// addresses.
type Resolved struct {
	Steps         []ResolvedStep `json:"steps"`
	Source        Layer          `json:"source,omitempty"`
	Contact       []string       `json:"contact"`
	ContactSource Layer          `json:"contact_source"`
}

// Usable reports whether any step can send.
func (r Resolved) Usable() bool {
	return slices.ContainsFunc(r.Steps, func(s ResolvedStep) bool { return s.Problem == "" })
}

// Plain returns the steps without their targets, as stored on an outbox row.
func (r Resolved) Plain() []Step {
	out := make([]Step, len(r.Steps))
	for i, s := range r.Steps {
		out[i] = s.Step
	}
	return out
}

// ResolvedStep is one step with its targets. Targets are addresses for an
// email step and the channel ref for a connector step. A step with a Problem
// is skipped at send time.
type ResolvedStep struct {
	Step
	Targets []string `json:"targets"`
	Problem string   `json:"problem,omitempty"`
}

// Scope selects the layers of one resolution. Empty fields skip a layer.
type Scope struct{ WorkspaceID, Agent, Job string }

// Layers reads the route and the contact of each layer. set is true when the
// layer holds a notify key, also when the list is empty. The daemon control
// plane implements it.
type Layers interface {
	JobLayer(ctx context.Context, ws, job string) (route []Step, set bool, contact []string, owner string)
	AgentLayer(ctx context.Context, ws, agent string) (route []Step, set bool, contact []string)
	WorkspaceLayer(ctx context.Context, ws string) (route []Step, set bool, contact []string)
	Global() (route []Step, set bool, admins []string)
}

// Channels tells whether a connector exists and has a channel with a send
// command.
type Channels interface {
	HasChannel(name string) (known, channel bool)
}

// Resolver applies the inheritance rules to a scope.
type Resolver struct {
	layers   Layers
	channels Channels
	emailOn  func() bool
}

// NewResolver builds a resolver. A nil emailOn means email is off.
func NewResolver(l Layers, ch Channels, emailOn func() bool) *Resolver {
	return &Resolver{layers: l, channels: ch, emailOn: emailOn}
}

// DefaultRoute is the route when no layer sets one: email to the contact.
func DefaultRoute() []Step { return []Step{{Via: ViaEmail, To: TokenContact}} }

// Resolve returns the route of sc. The first layer that sets notify wins, in
// the order job, owner agent, workspace, global. The contact follows the same
// order on its own; the global contact is email.admins.
func (r *Resolver) Resolve(ctx context.Context, sc Scope) Resolved {
	return r.resolve(ctx, sc, nil)
}

// ResolveRoute expands a frozen route with the current contact of sc. Alert
// delivery uses it to move to the next step of a stored route.
func (r *Resolver) ResolveRoute(ctx context.Context, sc Scope, route []Step) Resolved {
	if route == nil {
		route = []Step{}
	}
	return r.resolve(ctx, sc, route)
}

// resolve applies the layers. A non-nil frozen route replaces the inherited
// route, and Source is then empty.
func (r *Resolver) resolve(ctx context.Context, sc Scope, frozen []Step) Resolved {
	var (
		route         []Step
		routeSet      bool
		source        Layer
		contact       []string
		contactSource Layer
	)
	take := func(layer Layer, rt []Step, set bool, ct []string) {
		if set && !routeSet {
			route, routeSet, source = rt, true, layer
		}
		if len(ct) > 0 && contact == nil {
			contact, contactSource = ct, layer
		}
	}
	agent := sc.Agent
	if sc.WorkspaceID != "" && sc.Job != "" {
		rt, set, ct, owner := r.layers.JobLayer(ctx, sc.WorkspaceID, sc.Job)
		take(LayerJob, rt, set, ct)
		if agent == "" {
			agent = owner
		}
	}
	if sc.WorkspaceID != "" && agent != "" {
		rt, set, ct := r.layers.AgentLayer(ctx, sc.WorkspaceID, agent)
		take(LayerAgent, rt, set, ct)
	}
	if sc.WorkspaceID != "" {
		rt, set, ct := r.layers.WorkspaceLayer(ctx, sc.WorkspaceID)
		take(LayerWorkspace, rt, set, ct)
	}
	rt, set, admins := r.layers.Global()
	take(LayerGlobal, rt, set, admins)
	switch {
	case frozen != nil:
		route, source = frozen, ""
	case !routeSet:
		route, source = DefaultRoute(), LayerDefault
	}
	contact = expandContact(contact, admins)
	if contactSource == "" {
		contactSource = LayerGlobal
	}
	out := Resolved{Steps: make([]ResolvedStep, 0, len(route)), Source: source, Contact: contact, ContactSource: contactSource}
	for _, s := range route {
		out.Steps = append(out.Steps, r.expand(s, contact, admins))
	}
	return out
}

func (r *Resolver) expand(s Step, contact, admins []string) ResolvedStep {
	rs := ResolvedStep{Step: s, Targets: []string{}}
	if s.Via == ViaEmail {
		switch strings.ToLower(strings.TrimSpace(s.To)) {
		case "", TokenContact:
			rs.Targets = append(rs.Targets, contact...)
		case TokenAdmins:
			rs.Targets = append(rs.Targets, admins...)
		default:
			rs.Targets = append(rs.Targets, strings.ToLower(strings.TrimSpace(s.To)))
		}
		switch {
		case r.emailOn == nil || !r.emailOn():
			rs.Problem = ProblemEmail
		case len(rs.Targets) == 0:
			rs.Problem = ProblemNoAddr
		}
		return rs
	}
	rs.Targets = append(rs.Targets, s.To)
	known, channel := false, false
	if r.channels != nil {
		known, channel = r.channels.HasChannel(s.Via)
	}
	switch {
	case !known:
		rs.Problem = ProblemNoConn
	case !channel:
		rs.Problem = ProblemNoChan
	}
	return rs
}

// ParseSteps decodes and validates a notify value. It takes a YAML node, a
// []Step, or a generic []any from JSON or YAML. Nil stays nil (unset); an
// empty list is kept (no notification). A YAML null is unset.
func ParseSteps(v any) ([]Step, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case yaml.Node:
		return ParseSteps(&t)
	case *yaml.Node:
		if t == nil || t.Kind == 0 {
			return nil, nil
		}
		var raw any
		if err := t.Decode(&raw); err != nil {
			return nil, fmt.Errorf("notify: %w", err)
		}
		return ParseSteps(raw)
	case []Step:
		return t, ValidateSteps(t)
	case []any:
		steps := make([]Step, 0, len(t))
		for i, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("notify: steps are {via, to} maps; the channel list is removed")
			}
			var s Step
			for k, val := range m {
				str, ok := val.(string)
				if !ok {
					return nil, fmt.Errorf("notify[%d].%s: want a string", i, k)
				}
				switch k {
				case "via":
					s.Via = str
				case "to":
					s.To = str
				default:
					return nil, fmt.Errorf("notify[%d]: unknown key %q", i, k)
				}
			}
			steps = append(steps, s)
		}
		return steps, ValidateSteps(steps)
	default:
		return nil, fmt.Errorf("notify: want a list of {via, to} steps")
	}
}

// ValidateSteps checks the parse-time rules of a route.
func ValidateSteps(steps []Step) error {
	if len(steps) > maxSteps {
		return fmt.Errorf("notify: at most %d steps", maxSteps)
	}
	for i, s := range steps {
		if s.Via == "" {
			return fmt.Errorf("notify[%d].via is required", i)
		}
		if !viaRe.MatchString(s.Via) {
			return fmt.Errorf("notify[%d].via %q: want email or a connector name", i, s.Via)
		}
		to := strings.TrimSpace(s.To)
		if s.Via != ViaEmail {
			if to == "" {
				return fmt.Errorf("notify[%d].to is required for connector %q", i, s.Via)
			}
			continue
		}
		if to == "" || strings.EqualFold(to, TokenContact) || strings.EqualFold(to, TokenAdmins) {
			continue
		}
		if !isAddress(to) {
			return fmt.Errorf("notify[%d].to %q: want an address, contact, or admins", i, s.To)
		}
	}
	return nil
}

// ParseContact decodes a contact value: one string or a list. Each item is an
// address or the token admins.
func ParseContact(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case yaml.Node:
		return ParseContact(&t)
	case *yaml.Node:
		if t == nil || t.Kind == 0 {
			return nil, nil
		}
		var raw any
		if err := t.Decode(&raw); err != nil {
			return nil, fmt.Errorf("contact: %w", err)
		}
		return ParseContact(raw)
	case string:
		return ParseContact([]string{t})
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("contact: want an address or a list of addresses")
			}
			out = append(out, s)
		}
		return ParseContact(out)
	case []string:
		out := make([]string, 0, len(t))
		for i, s := range t {
			s = strings.TrimSpace(s)
			if strings.EqualFold(s, TokenAdmins) {
				out = append(out, TokenAdmins)
				continue
			}
			if !isAddress(s) {
				return nil, fmt.Errorf("contact[%d] %q: want an address or admins", i, s)
			}
			out = append(out, strings.ToLower(s))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("contact: want an address or a list of addresses")
	}
}

func isAddress(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && strings.EqualFold(a.Address, s)
}

// expandContact replaces the admins token with the admin addresses, folds
// case, and drops repeats.
func expandContact(contact, admins []string) []string {
	out := []string{}
	add := func(a string) {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	if contact == nil {
		contact = []string{TokenAdmins}
	}
	for _, c := range contact {
		if strings.EqualFold(strings.TrimSpace(c), TokenAdmins) {
			for _, a := range admins {
				add(a)
			}
			continue
		}
		add(c)
	}
	return out
}
