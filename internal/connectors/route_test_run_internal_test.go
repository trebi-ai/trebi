package connectors

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/trebi-ai/trebi/internal/decisions/decisionstest"
	"github.com/trebi-ai/trebi/internal/routing"
)

func (e *routerEnv) rowCounts() []int {
	e.t.Helper()
	var out []int
	for _, tbl := range []string{"event_outcomes", "route_choices", "decisions", "event_queue", "events"} {
		var n int
		if err := e.st.DB().QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func (e *routerEnv) routeTest(in RouteTestInput) RouteTestReport {
	e.t.Helper()
	in.Workspace, in.Connection = e.wsID, "email"
	rep, err := e.svc.RouteTest(context.Background(), in)
	if err != nil {
		e.t.Fatal(err)
	}
	return rep
}

func TestRouteTestStoresNothing(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), sureAlpha())
	id := e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	before, calls := e.rowCounts(), e.fake.Calls()
	rules := []routing.Rule{{Field: routing.RuleTextHas, Value: "invoice", To: "beta"}}
	e.routeTest(RouteTestInput{EventIDs: []string{id}, Text: "an invoice", Routing: &routing.Patch{Rules: &rules}})
	e.routeTest(RouteTestInput{Text: "other words"})
	eq(t, fmt.Sprint(e.rowCounts()), fmt.Sprint(before))
	eq(t, e.fake.Calls(), calls+1)
}

func TestRouteTestDraftRule(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), sureAlpha())
	rep := e.routeTest(RouteTestInput{Text: "an invoice", From: "ann@x.io"})
	eq(t, rep.Results[0].RouteID, rtA)
	eq(t, rep.Results[0].Method, "router")
	rules := []routing.Rule{{Field: routing.RuleTextHas, Value: "invoice", To: "beta"}}
	rep = e.routeTest(RouteTestInput{Text: "an invoice", From: "ann@x.io", Routing: &routing.Patch{Rules: &rules}})
	r := rep.Results[0]
	eq(t, r.RouteID, rtB)
	eq(t, r.Method, "rule")
	eq(t, r.RuleIndex, 1)
	eq(t, e.fake.Calls(), 1)
}

func TestRouteTestText(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), decisionstest.Shares("route", "r1", 0.8, "r2", 0.1, "nobody", 0.1))
	r := e.routeTest(RouteTestInput{Text: "hello", From: "ann@x.io", Subject: "Hi"}).Results[0]
	eq(t, r.Outcome, "chosen")
	eq(t, r.Target, "alpha")
	eq(t, r.TargetKind, "agent")
	eq(t, len(r.Shares), 2)
	eq(t, r.Decider, "fake")
	sum := r.NobodyShare
	for _, s := range r.Shares {
		sum += s.Share
	}
	eq(t, math.Abs(sum-1) < 1e-9, true)
	var steps []string
	for _, s := range r.Steps {
		steps = append(steps, s.Step)
	}
	eq(t, fmt.Sprint(steps), "[every_message filters conversation only rules router]")
	e2 := newRouterEnv(t, chosenRoutes(rtA, rtB), decisionstest.Shares("route", "r1", 0.3, "r2", 0.5, "nobody", 0.2))
	eq(t, e2.routeTest(RouteTestInput{Text: "hello"}).Results[0].Outcome, "ask")
}

func TestRouteTestBefore(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), sureAlpha())
	id := e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	r := e.routeTest(RouteTestInput{EventIDs: []string{id, "01NOPE"}}).Results
	eq(t, r[0].Before != nil && r[0].Before.RouteID == rtA && r[0].Before.Method == "router", true)
	eq(t, r[0].Method, "conversation")
	eq(t, r[1].Outcome, "not_found")
}

func TestRouteTestOffAndOnlyOne(t *testing.T) {
	f := chosenRoutes(rtA, rtB)
	f.Mode = routing.ModeOff
	f.Routes[0].Gets = routing.GetsEveryMessage
	e := newRouterEnv(t, f)
	r := e.routeTest(RouteTestInput{Text: "hello"}).Results[0]
	eq(t, r.Outcome, "off")
	eq(t, r.Steps[0].Result, "pass")
	mode := routing.ModeOn
	f2 := chosenRoutes(rtA)
	e2 := newRouterEnv(t, f2)
	r = e2.routeTest(RouteTestInput{Text: "hello", Routing: &routing.Patch{Mode: &mode}}).Results[0]
	eq(t, r.Method, "only")
	eq(t, r.RouteID, rtA)
}

func TestRouteTestLimit21(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), sureAlpha())
	ids := make([]string, 21)
	for i := range ids {
		ids[i] = fmt.Sprint("01EV", i)
	}
	ctx := context.Background()
	if _, err := e.svc.RouteTest(ctx, RouteTestInput{Workspace: e.wsID, Connection: "email", EventIDs: ids}); !errors.Is(err, ErrBadInput) {
		t.Fatalf("21 ids: %v", err)
	}
	if _, err := e.svc.RouteTest(ctx, RouteTestInput{Workspace: e.wsID, Connection: "email"}); !errors.Is(err, ErrBadInput) {
		t.Fatalf("empty: %v", err)
	}
	bad := "maybe"
	if _, err := e.svc.RouteTest(ctx, RouteTestInput{Workspace: e.wsID, Connection: "email", Text: "x", Routing: &routing.Patch{Mode: &bad}}); !errors.Is(err, ErrBadInput) {
		t.Fatalf("bad draft: %v", err)
	}
}
