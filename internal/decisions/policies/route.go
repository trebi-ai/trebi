package policies

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/trebi-ai/trebi/internal/decisions"
	"github.com/trebi-ai/trebi/internal/routing"
)

// Route decision kind, subject, and outcomes.
const (
	RouteKind    = "event.route"
	RouteSubject = "event"
	// RouteNobodyKey is the option key of "nobody".
	RouteNobodyKey = "nobody"

	RouteChosen decisions.Outcome = "chosen"
	RouteNobody decisions.Outcome = "nobody"
	RouteAsk    decisions.Outcome = "ask"

	// MethodRouter is a pick that met sure_at. MethodGuess is a pick below it.
	MethodRouter = "router"
	MethodGuess  = "guess"
)

// Facts of an event.route state.
const (
	RouteFactConnection = "connection"
	RouteFactNotes      = "notes"
	RouteFactSureAt     = "sure_at"
	RouteFactUnsure     = "unsure"
	RouteFactNoFit      = "no_fit"
	RouteFactChoices    = "choices" // JSON: option key to route id
)

const (
	routeQuestionID = "route"
	routeMinChoices = 2
	routeMaxChoices = 30
	routeTextCap    = 4000
)

// RouteChoice is one route that the router may pick. Key is r1..rN in file
// order. TargetKind is agent or job.
type RouteChoice struct {
	Key, RouteID, Target, TargetKind, UseFor string
}

// RouteInput is what the router sees of one message and one connection.
type RouteInput struct {
	Connection, ConnectionTitle, Notes string
	Choices                            []RouteChoice
	SureAt                             float64
	Unsure, NoFit                      string
	From, Subject, Room, Text          string
}

// Route is the policy that picks one route for a message. The safe outcome
// is ask: when the check does not finish, a person chooses.
func Route() decisions.Policy {
	return decisions.Policy{
		Kind: RouteKind, Version: 1, Label: "Event routing", Subject: RouteSubject, Safe: RouteAsk,
		Build: func(s decisions.State) ([]decisions.Question, error) {
			if n := len(s.Choices); n < routeMinChoices || n > routeMaxChoices {
				return nil, fmt.Errorf("the router needs %d to %d choices; found %d", routeMinChoices, routeMaxChoices, n)
			}
			opts := append(append([]decisions.Option{}, s.Choices...),
				decisions.Option{Key: RouteNobodyKey, Description: "Nobody. No choice fits this message."})
			return []decisions.Question{{
				ID: routeQuestionID, Type: decisions.TypeChoice, Shares: true, Options: opts,
				Text: "Which choice should get this message?",
			}}, nil
		},
		Map: func(a decisions.Answers, s decisions.State) (decisions.Outcome, []string) {
			p := PickRoute(a[routeQuestionID], s)
			return p.Outcome, []string{p.Reason(s)}
		},
	}
}

// RouteState builds the state of one routing call. The message is outside
// data; the notes and the use_for texts are trusted.
func RouteState(in RouteInput) decisions.State {
	choices := make([]decisions.Option, len(in.Choices))
	refs := make(map[string]string, len(in.Choices))
	for i, c := range in.Choices {
		choices[i] = decisions.Option{Key: c.Key, Ref: c.RouteID, Description: fmt.Sprintf("%s (%s): %s", c.Target, c.TargetKind, c.UseFor)}
		refs[c.Key] = c.RouteID
	}
	refJSON, _ := json.Marshal(refs)
	text := fmt.Sprintf("From: %s\nSubject: %s\nRoom: %s\n\n%s", in.From, in.Subject, in.Room, in.Text)
	if r := []rune(text); len(r) > routeTextCap {
		text = string(r[:routeTextCap])
	}
	return decisions.State{
		Text: text, Untrusted: true, Choices: choices,
		Facts: map[string]string{
			RouteFactConnection: cmp.Or(in.ConnectionTitle, in.Connection),
			RouteFactNotes:      in.Notes,
			RouteFactSureAt:     strconv.FormatFloat(in.SureAt, 'f', -1, 64),
			RouteFactUnsure:     in.Unsure,
			RouteFactNoFit:      in.NoFit,
			RouteFactChoices:    string(refJSON),
		},
	}
}

// RoutePick is the result of PickRoute. Best is the route with the largest
// share other than nobody; it is empty when no route has a share.
type RoutePick struct {
	Key, RouteID string
	Outcome      decisions.Outcome
	Method       string
	Share        float64
	Best         string
}

// Reason is the one sentence that tells how the pick was made.
func (p RoutePick) Reason(s decisions.State) string {
	sure := s.Facts[RouteFactSureAt]
	switch {
	case p.Outcome == RouteNobody:
		return "No choice fits this message."
	case p.Outcome == RouteAsk:
		return fmt.Sprintf("The router is not sure (top share %.0f%%, sure at %s). A person chooses.", p.Share*100, sure)
	case p.Method == MethodGuess:
		return fmt.Sprintf("The router guessed (top share %.0f%%, sure at %s).", p.Share*100, sure)
	}
	return fmt.Sprintf("The router is sure (share %.0f%%).", p.Share*100)
}

// PickRoute applies sure_at, unsure, and no_fit to the shares of an answer.
// It is pure, so a test run and a live run pick the same way.
func PickRoute(a decisions.Answer, s decisions.State) RoutePick {
	sureAt, err := strconv.ParseFloat(s.Facts[RouteFactSureAt], 64)
	if err != nil || sureAt <= 0 {
		sureAt = routing.DefaultSureAt
	}
	refs := map[string]string{}
	_ = json.Unmarshal([]byte(s.Facts[RouteFactChoices]), &refs) // the state builder wrote this JSON
	top, share := a.Value, a.Confidence
	if top == "" {
		return RoutePick{Outcome: RouteAsk}
	}
	var best, bestKey string
	var bestShare float64
	for _, o := range s.Choices {
		if p := a.Distribution[o.Key]; p > bestShare || bestKey == "" {
			best, bestKey, bestShare = refs[o.Key], o.Key, p
		}
	}
	pick := RoutePick{Key: top, RouteID: refs[top], Share: share, Best: best}
	noFit := func() RoutePick {
		pick.Key, pick.RouteID = RouteNobodyKey, ""
		if s.Facts[RouteFactNoFit] == routing.NoFitAsk {
			pick.Outcome = RouteAsk
		} else {
			pick.Outcome = RouteNobody
		}
		return pick
	}
	switch {
	case share >= sureAt && top == RouteNobodyKey:
		return noFit()
	case share >= sureAt:
		pick.Outcome, pick.Method = RouteChosen, MethodRouter
		return pick
	case s.Facts[RouteFactUnsure] != routing.UnsureGuess:
		pick.Outcome = RouteAsk
		return pick
	case best == "":
		return noFit()
	}
	// A guess takes the best route, even when nobody has the top share.
	pick.Key, pick.RouteID, pick.Share = bestKey, best, bestShare
	pick.Outcome, pick.Method = RouteChosen, MethodGuess
	return pick
}
