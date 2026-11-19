package policies_test

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/decisions"
	"github.com/trebi-ai/trebi/internal/decisions/policies"
)

func TestPickRouteTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		unsure, noFit string
		dist          map[string]float64
		outcome       decisions.Outcome
		method        string
		route         string
	}{
		{"sure route", "ask", "drop", map[string]float64{"r1": 0.8, "r2": 0.1, "nobody": 0.1}, policies.RouteChosen, policies.MethodRouter, "rt_1111111111"},
		{"sure nobody drop", "ask", "drop", map[string]float64{"r1": 0.1, "nobody": 0.9}, policies.RouteNobody, "", ""},
		{"sure nobody ask", "ask", "ask", map[string]float64{"r1": 0.1, "nobody": 0.9}, policies.RouteAsk, "", ""},
		{"unsure ask", "ask", "drop", map[string]float64{"r1": 0.5, "r2": 0.3, "nobody": 0.2}, policies.RouteAsk, "", ""},
		{"unsure guess", "guess", "drop", map[string]float64{"r1": 0.2, "r2": 0.5, "nobody": 0.3}, policies.RouteChosen, policies.MethodGuess, "rt_2222222222"},
		{"guess when nobody leads", "guess", "drop", map[string]float64{"r1": 0.3, "r2": 0.1, "nobody": 0.6}, policies.RouteChosen, policies.MethodGuess, "rt_1111111111"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := policies.RouteState(routeInput(tc.unsure, tc.noFit))
			q := decisions.Question{ID: "route", Type: decisions.TypeChoice, Shares: true,
				Options: append(append([]decisions.Option{}, state.Choices...), decisions.Option{Key: "nobody"})}
			a := q.Normalize(decisions.Answer{ID: "route", Distribution: tc.dist})
			got := policies.PickRoute(a, state)
			if got.Outcome != tc.outcome || got.Method != tc.method || (tc.outcome == policies.RouteChosen && got.RouteID != tc.route) {
				t.Fatalf("pick = %+v", got)
			}
		})
	}
}

func TestPickRouteGuessWithoutRoutes(t *testing.T) {
	t.Parallel()
	in := routeInput("guess", "ask")
	in.Choices = nil
	state := policies.RouteState(in)
	a := decisions.Answer{ID: "route", Value: "nobody", Confidence: 0.5, Distribution: map[string]float64{"nobody": 0.5}}
	if got := policies.PickRoute(a, state); got.Outcome != policies.RouteAsk || got.Best != "" {
		t.Fatalf("pick = %+v", got)
	}
}

func TestRouteBuildLimits(t *testing.T) {
	t.Parallel()
	p := policies.Route()
	for _, n := range []int{1, 31} {
		in := routeInput("ask", "drop")
		in.Choices = nil
		for i := range n {
			in.Choices = append(in.Choices, policies.RouteChoice{Key: "r" + string(rune('a'+i%26)) + string(rune('a'+i/26)), RouteID: "rt_x", Target: "t", TargetKind: "agent"})
		}
		if _, err := p.Build(policies.RouteState(in)); err == nil {
			t.Fatalf("%d choices: want an error", n)
		}
	}
	qs, err := p.Build(policies.RouteState(routeInput("ask", "drop")))
	if err != nil || len(qs) != 1 || len(qs[0].Options) != 3 || qs[0].Options[2].Key != policies.RouteNobodyKey || qs[0].Validate() != nil {
		t.Fatalf("build = %+v, %v", qs, err)
	}
}

func TestRouteStateCaps(t *testing.T) {
	t.Parallel()
	in := routeInput("ask", "drop")
	in.Text = strings.Repeat("x", 9000)
	st := policies.RouteState(in)
	if len([]rune(st.Text)) != 4000 || !st.Untrusted {
		t.Fatalf("text %d, untrusted %v", len([]rune(st.Text)), st.Untrusted)
	}
	if st.Choices[0].Ref != "rt_1111111111" || !strings.Contains(st.Facts[policies.RouteFactChoices], "rt_2222222222") {
		t.Fatalf("refs = %+v %v", st.Choices, st.Facts)
	}
}

func routeInput(unsure, noFit string) policies.RouteInput {
	return policies.RouteInput{Connection: "gmail", SureAt: 0.7, Unsure: unsure, NoFit: noFit, Text: "hello", Choices: []policies.RouteChoice{
		{Key: "r1", RouteID: "rt_1111111111", Target: "a", TargetKind: "agent", UseFor: "x"},
		{Key: "r2", RouteID: "rt_2222222222", Target: "b", TargetKind: "job", UseFor: "y"},
	}}
}
