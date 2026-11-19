package connectors

import (
	"context"
	"errors"
	"testing"

	"github.com/trebi-ai/trebi/internal/decisions"
	"github.com/trebi-ai/trebi/internal/decisions/decisionstest"
	"github.com/trebi-ai/trebi/internal/routing"
)

func (e *routerEnv) reroute(to string, force, ignore bool) (RerouteResult, error) {
	e.t.Helper()
	id := e.lastEvent()
	return e.svc.Reroute(context.Background(), RerouteOpts{EventID: id, Workspace: e.wsID, To: to, Force: force, Ignore: ignore})
}

func (e *routerEnv) lastEvent() string {
	e.t.Helper()
	var id string
	if err := e.st.DB().QueryRow(`SELECT id FROM events ORDER BY rowid DESC LIMIT 1`).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestRerouteToOther(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), sureAlpha())
	id := e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	res, err := e.reroute("beta", false, false)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Wrong, true)
	eq(t, res.Feedback, decisions.FeedbackDisagree)
	eq(t, res.RouteID, rtB)
	eq(t, res.PickRouteID, rtA)
	eq(t, e.outcomes(id)[rtB], "queued/you")
	eq(t, e.outcomes(id)[rtA], "not_chosen/you")
	c, _ := e.choice("t1")
	eq(t, c.Status == ChoiceChosen && c.RouteID == rtB && c.Method == MethodYou, true)
	eq(t, c.PickRouteID, rtA)
}

func TestRerouteSameAsPick(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), sureAlpha())
	e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	res, err := e.reroute(rtA, false, false)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Wrong, false)
	eq(t, res.Feedback, decisions.FeedbackAgree)
}

func TestRerouteNobody(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), sureAlpha())
	id := e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	res, err := e.reroute(routing.RuleNobody, false, false)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Wrong, true)
	eq(t, res.Feedback, decisions.FeedbackDisagree)
	c, _ := e.choice("t1")
	eq(t, c.Status, ChoiceNobody)
	eq(t, e.outcomes(id)[rtA], "not_chosen/you")
}

func TestRerouteIgnoreGivesNoFeedback(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), sureAlpha())
	e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	res, err := e.reroute(routing.RuleNobody, false, true)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Wrong, false)
	eq(t, res.Feedback, "")
	c, _ := e.choice("t1")
	eq(t, c.Status == ChoiceNobody && c.Method == MethodIgnored, true)
}

func TestRerouteGateRefuses(t *testing.T) {
	f := chosenRoutes(rtA, rtB)
	f.Routes[1].Filter.Match = "false"
	e := newRouterEnv(t, f, sureAlpha())
	e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	_, err := e.reroute("beta", false, false)
	var na *NotAcceptedError
	if !errors.As(err, &na) {
		t.Fatalf("want NotAcceptedError, got %v", err)
	}
	c, _ := e.choice("t1")
	eq(t, c.RouteID, rtA)
}

func TestRerouteForce(t *testing.T) {
	f := chosenRoutes(rtA, rtB)
	f.Routes[1].Filter.Match = "false"
	e := newRouterEnv(t, f, sureAlpha())
	id := e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	if _, err := e.reroute("beta", true, false); err != nil {
		t.Fatal(err)
	}
	eq(t, e.outcomes(id)[rtB], "queued/you")
}

func TestRerouteAmbiguousName(t *testing.T) {
	f := chosenRoutes(rtA, rtB)
	f.Routes[1].Agent = "alpha"
	e := newRouterEnv(t, f, sureAlpha())
	e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	_, err := e.reroute("alpha", false, false)
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("got %v", err)
	}
	if _, err := e.reroute(rtB, false, false); err != nil {
		t.Fatal(err)
	}
}

func TestRerouteRouteNotFound(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), sureAlpha())
	e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	if _, err := e.reroute("nope", false, false); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestRerouteRoutingOff(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), sureAlpha())
	e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	f := chosenRoutes(rtA, rtB)
	f.Mode = routing.ModeOff
	e.rewrite(f)
	if _, err := e.reroute("beta", false, false); !errors.Is(err, ErrRoutingOff) {
		t.Fatalf("got %v", err)
	}
}

func TestRerouteNextReplyFollows(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), sureAlpha())
	e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	if _, err := e.reroute("beta", false, false); err != nil {
		t.Fatal(err)
	}
	id2 := e.send("m2", "t1", "ann@x.io", "again")
	eq(t, e.outcomes(id2)[rtB], "queued/conversation")
	eq(t, e.fake.Calls(), 1)
}

func TestRerouteWaitingMovesAllEvents(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), decisionstest.Shares("route", "r1", 0.3, "r2", 0.5, "nobody", 0.2))
	id1 := e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	id2 := e.send("m2", "t1", "ann@x.io", "more")
	eq(t, e.outcomes(id1)[rtA], "route_wait/")
	res, err := e.svc.Reroute(context.Background(), RerouteOpts{EventID: id1, Workspace: e.wsID, To: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Wrong, false)
	eq(t, e.outcomes(id1)[rtB], "queued/you")
	eq(t, e.outcomes(id2)[rtB], "queued/you")
	eq(t, e.outcomes(id2)[rtA], "not_chosen/you")
	c, _ := e.choice("t1")
	eq(t, c.Status == ChoiceChosen && c.Method == MethodYou, true)
}

func sureAlpha() decisionstest.Step {
	return decisionstest.Shares("route", "r1", 0.8, "r2", 0.1, "nobody", 0.1)
}
