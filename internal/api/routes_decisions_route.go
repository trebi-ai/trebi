package api

import (
	"cmp"
	"context"
	"regexp"
	"strconv"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/decisions"
	"github.com/trebi-ai/trebi/internal/decisions/policies"
)

// decisionView is a decision with the route block of an event.route decision.
type decisionView struct {
	decisions.Decision
	Route *routeBlock `json:"route,omitempty"`
}

// routeBlock is what a person needs to judge a routing decision.
type routeBlock struct {
	Connection  string                 `json:"connection"`
	Key         string                 `json:"conversation_key,omitempty"`
	Event       *connectors.EventBrief `json:"event,omitempty"`
	Choices     []routeBlockChoice     `json:"choices"`
	NobodyShare float64                `json:"nobody_share"`
	Pick        routeBlockPick         `json:"pick"`
	SureAt      float64                `json:"sure_at"`
	Unsure      string                 `json:"unsure,omitempty"`
	NoFit       string                 `json:"no_fit,omitempty"`
	Outcome     decisions.Outcome      `json:"outcome"`
	Status      string                 `json:"status"`
	Waiting     bool                   `json:"waiting"`
	InboxID     string                 `json:"inbox_id,omitempty"`
	ChosenByYou *routeBlockYou         `json:"chosen_by_you,omitempty"`
}

type routeBlockChoice struct {
	Key        string  `json:"key"`
	RouteID    string  `json:"route_id"`
	Target     string  `json:"target"`
	TargetKind string  `json:"target_kind"`
	Share      float64 `json:"share"`
}

// routeBlockPick is the pick of the router. Method is router, guess, ask, or
// nobody. BestRouteID is the best route other than nobody: the "Best guess".
type routeBlockPick struct {
	RouteID     string  `json:"route_id,omitempty"`
	BestRouteID string  `json:"best_route_id,omitempty"`
	Method      string  `json:"method"`
	Share       float64 `json:"share"`
}

type routeBlockYou struct {
	RouteID string `json:"route_id"`
	Wrong   bool   `json:"wrong"`
}

// optionTarget reads "<target> (<kind>): <use for>" from an option.
var optionTarget = regexp.MustCompile(`^(.*?) \((agent|job)\): `)

// view adds the route block to a decision of the kind event.route.
func (s *Server) decisionView(ctx context.Context, d decisions.Decision) decisionView {
	v := decisionView{Decision: d}
	if d.Kind == policies.RouteKind {
		v.Route = s.routeBlockOf(ctx, d)
	}
	return v
}

func (s *Server) decisionViews(ctx context.Context, list []decisions.Decision) []decisionView {
	out := make([]decisionView, len(list))
	for i, d := range list {
		out[i] = s.decisionView(ctx, d)
	}
	return out
}

func (s *Server) routeBlockOf(ctx context.Context, d decisions.Decision) *routeBlock {
	b := routeBlockFacts(d)
	if d.SubjectID == "" || s.connectors == nil {
		return b
	}
	if ev, err := s.connectors.EventBrief(ctx, d.SubjectID); err == nil {
		b.Event = &ev
	}
	if c, ok, err := s.connectors.ChoiceOfDecision(ctx, d.ID); err == nil && ok {
		b.Key, b.InboxID = c.Key, c.InboxID
		b.Waiting = c.Status == connectors.ChoiceWaiting || c.Status == connectors.ChoiceDeciding
		if c.Method == connectors.MethodYou {
			got := cmp.Or(c.RouteID, policies.RouteNobodyKey)
			b.ChosenByYou = &routeBlockYou{RouteID: got, Wrong: c.PickRouteID != "" && got != c.PickRouteID}
		}
	}
	return b
}

// routeBlockFacts fills the part of the block that the decision alone holds.
func routeBlockFacts(d decisions.Decision) *routeBlock {
	b := &routeBlock{Connection: d.Scope, Outcome: d.Outcome, Status: d.Status, Choices: []routeBlockChoice{}}
	i := indexOfQuestion(d, "route")
	if i < 0 {
		return b
	}
	state := decisions.State{Facts: d.Facts, Choices: d.Questions[i].Options}
	a := decisions.Answer{}
	if j := indexOfAnswer(d, "route"); j >= 0 {
		a = d.Answers[j]
	}
	for _, o := range d.Questions[i].Options {
		if o.Key == policies.RouteNobodyKey {
			continue
		}
		c := routeBlockChoice{Key: o.Key, RouteID: o.Ref, Share: a.Distribution[o.Key]}
		if m := optionTarget.FindStringSubmatch(o.Description); m != nil {
			c.Target, c.TargetKind = m[1], m[2]
		}
		b.Choices = append(b.Choices, c)
	}
	b.NobodyShare = a.Distribution[policies.RouteNobodyKey]
	b.SureAt, _ = strconv.ParseFloat(d.Facts[policies.RouteFactSureAt], 64)
	b.Unsure, b.NoFit = d.Facts[policies.RouteFactUnsure], d.Facts[policies.RouteFactNoFit]
	if d.Status == decisions.StatusOK {
		b.Pick = pickOf(policies.PickRoute(a, state))
	}
	return b
}

func pickOf(p policies.RoutePick) routeBlockPick {
	out := routeBlockPick{RouteID: p.RouteID, BestRouteID: p.Best, Share: p.Share}
	switch p.Outcome {
	case policies.RouteAsk:
		out.Method = "ask"
	case policies.RouteNobody:
		out.Method = "nobody"
	default:
		out.Method = p.Method
	}
	return out
}

func indexOfQuestion(d decisions.Decision, id string) int {
	for i, q := range d.Questions {
		if q.ID == id {
			return i
		}
	}
	return -1
}

func indexOfAnswer(d decisions.Decision, id string) int {
	for i, a := range d.Answers {
		if a.ID == id {
			return i
		}
	}
	return -1
}

// routeShares maps each route id of an event.route decision, and "nobody", to its share.
func routeShares(d decisions.Decision) map[string]float64 {
	out := map[string]float64{}
	i, j := indexOfQuestion(d, "route"), indexOfAnswer(d, "route")
	if i < 0 || j < 0 {
		return out
	}
	for _, o := range d.Questions[i].Options {
		if o.Ref != "" {
			out[o.Ref] = d.Answers[j].Distribution[o.Key]
		}
	}
	out[policies.RouteNobodyKey] = d.Answers[j].Distribution[policies.RouteNobodyKey]
	return out
}
