package connectors

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/decisions"
	"github.com/trebi-ai/trebi/internal/decisions/decisionstest"
	"github.com/trebi-ai/trebi/internal/decisions/policies"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/waittest"
)

const (
	rtA = "rt_00000000a1"
	rtB = "rt_00000000b1"
	rtC = "rt_00000000c1"
)

// routerEnv is a workspace with a routes file for the connection email, a
// router decider that replays steps, and a recorder for the wait hook.
type routerEnv struct {
	t      *testing.T
	svc    *Service
	st     *store.Store
	wsID   string
	wsPath string
	fake   *decisionstest.Fake
	mu     sync.Mutex
	waits  []RouteWait
}

func (e *routerEnv) reindex() {
	e.t.Helper()
	w, err := e.svc.ws.Get(context.Background(), e.wsID)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.svc.ws.RebuildIndex(context.Background(), w); err != nil {
		e.t.Fatal(err)
	}
}

// rewrite replaces the routes file.
func (e *routerEnv) rewrite(f routing.File) {
	e.t.Helper()
	routingtest.Write(e.t, e.wsPath, "email", f)
	e.reindex()
}

// send injects a message in a thread of the room INBOX.
func (e *routerEnv) send(src, thread, from, text string) string {
	e.t.Helper()
	id, err := e.svc.InjectNative(context.Background(), "email", map[string]any{"n": src},
		NativeMeta{SourceID: src, Identity: from, Sender: from, Room: "INBOX", Thread: thread, Type: "message", Text: text})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *routerEnv) decide() { e.svc.decidePending(context.Background()) }

// outcomes maps a route id to "<outcome>/<method>" for one event.
func (e *routerEnv) outcomes(eventID string) map[string]string {
	e.t.Helper()
	rows, err := e.st.DB().Query(`SELECT route_id, outcome, COALESCE(route_method,'') FROM event_outcomes WHERE event_id=?`, eventID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var rid, o, m string
		if err := rows.Scan(&rid, &o, &m); err != nil {
			e.t.Fatal(err)
		}
		out[rid] = o + "/" + m
	}
	return out
}

func (e *routerEnv) choice(thread string) (ConversationChoice, bool) {
	e.t.Helper()
	c, ok, err := e.svc.loadChoice(context.Background(), e.st.DB(), e.wsID, "email", "INBOX/"+thread)
	if err != nil {
		e.t.Fatal(err)
	}
	return c, ok
}

func (e *routerEnv) queued() int {
	e.t.Helper()
	var n int
	if err := e.st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *routerEnv) waitList() []RouteWait {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]RouteWait(nil), e.waits...)
}

func TestRouteKeyGmailDiscordDM(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ev   eventRecord
		want string
	}{
		{"gmail", eventRecord{room: "INBOX", root: "18f2"}, "INBOX/18f2"},
		{"discord thread", eventRecord{room: "C1", root: "T9", thread: "T9"}, "C1/T9"},
		{"discord own message", eventRecord{room: "C1", root: "M7"}, "C1/M7"},
		{"dm", eventRecord{room: "D1", root: "D1"}, "D1"},
		{"threadless channel keeps root", eventRecord{room: "G1", root: "M2", threadless: true}, "G1/M2"},
		{"no root", eventRecord{room: "R"}, "R"},
		{"nothing", eventRecord{id: "e1"}, "e1"},
	} {
		eq(t, tc.ev.routeKey(), tc.want)
	}
}

func TestRouteOnlyOne(t *testing.T) {
	e := newRouterEnv(t, func() routing.File {
		f := chosenRoutes(rtA, rtB)
		f.Routes[1].Filter = job.TriggerYAML{Rooms: []string{"elsewhere"}}
		return f
	}())
	id := e.send("m1", "t1", "ann@x.io", "hello")
	eq(t, e.outcomes(id)[rtA], "queued/only")
	eq(t, e.outcomes(id)[rtB], "no_match/")
	eq(t, e.queued(), 1)
	eq(t, e.fake.Calls(), 0)
	c, ok := e.choice("t1")
	eq(t, ok && c.Status == ChoiceChosen && c.Method == MethodOnly && c.RouteID == rtA, true)
	if topicCount(t, e.st, "event.routed") != 1 {
		t.Fatal("event.routed not appended")
	}
}

func TestRouteNothingToChoose(t *testing.T) {
	e := newRouterEnv(t, func() routing.File {
		f := chosenRoutes(rtA, rtB)
		f.Routes[0].Filter = job.TriggerYAML{Rooms: []string{"x"}}
		f.Routes[1].Filter = job.TriggerYAML{Rooms: []string{"y"}}
		return f
	}())
	id := e.send("m1", "t1", "ann@x.io", "hello")
	eq(t, e.outcomes(id)[rtA], "no_match/")
	eq(t, e.queued(), 0)
	_, ok := e.choice("t1")
	eq(t, ok, false)
}

func TestRouteRuleFirstFit(t *testing.T) {
	f := chosenRoutes(rtA, rtB)
	f.Rules = []routing.Rule{
		{Field: routing.RuleSenderIs, Value: "other@x.io", To: "alpha"},
		{Field: routing.RuleSenderEnds, Value: "@x.io", To: "beta"},
		{Field: routing.RuleTextHas, Value: "hello", To: "alpha"},
	}
	e := newRouterEnv(t, f)
	id := e.send("m1", "t1", "ann@x.io", "hello")
	eq(t, e.outcomes(id)[rtB], "queued/rule")
	eq(t, e.outcomes(id)[rtA], "not_chosen/rule")
	var idx int
	if err := e.st.DB().QueryRow(`SELECT rule_index FROM event_outcomes WHERE event_id=? AND route_id=?`, id, rtB).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	eq(t, idx, 2)
	eq(t, e.fake.Calls(), 0)
}

func TestRouteRuleSkipsTargetNotCandidate(t *testing.T) {
	f := chosenRoutes(rtA, rtB, rtC)
	f.Routes[2].Filter = job.TriggerYAML{Rooms: []string{"elsewhere"}}
	f.Rules = []routing.Rule{
		{Field: routing.RuleTextHas, Value: "hello", To: "gamma"},
		{Field: routing.RuleTextHas, Value: "hello", To: "beta"},
	}
	e := newRouterEnv(t, f)
	id := e.send("m1", "t1", "ann@x.io", "hello")
	eq(t, e.outcomes(id)[rtB], "queued/rule")
}

func TestRouteRuleNobody(t *testing.T) {
	f := chosenRoutes(rtA, rtB)
	f.Rules = []routing.Rule{{Field: routing.RuleSenderIs, Value: "spam@x.io", To: routing.RuleNobody}}
	e := newRouterEnv(t, f)
	id := e.send("m1", "t1", "spam@x.io", "buy now")
	eq(t, e.outcomes(id)[rtA], "not_chosen/rule")
	eq(t, e.outcomes(id)[rtB], "not_chosen/rule")
	eq(t, e.queued(), 0)
	c, _ := e.choice("t1")
	eq(t, c.Status, ChoiceNobody)
}

func TestRouterSure(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), decisionstest.Shares("route", "r1", 0.8, "r2", 0.1, "nobody", 0.1))
	id := e.send("m1", "t1", "ann@x.io", "hello")
	eq(t, e.outcomes(id)[rtA], "route_wait/")
	e.decide()
	eq(t, e.outcomes(id)[rtA], "queued/router")
	eq(t, e.outcomes(id)[rtB], "not_chosen/router")
	c, _ := e.choice("t1")
	eq(t, c.Status == ChoiceChosen && c.RouteID == rtA && c.PickRouteID == rtA && c.DecisionID != "", true)
	var scope, did string
	if err := e.st.DB().QueryRow(`SELECT COALESCE(scope,''), COALESCE(decision_id,'') FROM decisions d, event_outcomes o WHERE o.event_id=? AND o.route_id=? AND d.id=o.decision_id`, id, rtA).Scan(&scope, &did); err != nil {
		t.Fatal(err)
	}
	eq(t, scope, "email")
	eq(t, did, c.DecisionID)
	if topicCount(t, e.st, "event.routed") != 2 {
		t.Fatal("want event.routed for waiting and routed")
	}
}

func TestRouterUnsureGuess(t *testing.T) {
	f := chosenRoutes(rtA, rtB)
	f.Unsure = routing.UnsureGuess
	e := newRouterEnv(t, f, decisionstest.Shares("route", "r1", 0.3, "r2", 0.5, "nobody", 0.2))
	id := e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	eq(t, e.outcomes(id)[rtB], "queued/guess")
	eq(t, len(e.waitList()), 0)
}

func TestRouterUnsureAsk(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), decisionstest.Shares("route", "r1", 0.3, "r2", 0.5, "nobody", 0.2))
	id := e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	eq(t, e.outcomes(id)[rtA], "route_wait/")
	w := e.waitList()
	eq(t, len(w), 1)
	eq(t, w[0].EventID, id)
	eq(t, w[0].Failed, false)
	eq(t, w[0].Choices[0].RouteID, rtB)
	c, _ := e.choice("t1")
	eq(t, c.Status == ChoiceWaiting && c.InboxID == "inb_1", true)
	ids, err := e.svc.WaitingDecisionIDs(context.Background(), e.wsID, "email")
	if err != nil || len(ids) != 1 || ids[0] != c.DecisionID {
		t.Fatalf("waiting ids = %v, %v", ids, err)
	}
}

func TestRouterNoFitDrop(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), decisionstest.Shares("route", "r1", 0.05, "r2", 0.05, "nobody", 0.9))
	id := e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	eq(t, e.outcomes(id)[rtA], "not_chosen/router")
	c, _ := e.choice("t1")
	eq(t, c.Status, ChoiceNobody)
	eq(t, len(e.waitList()), 0)
}

func TestRouterFailClosed(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), decisionstest.Step{Err: errors.New("boom")})
	id := e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	c, _ := e.choice("t1")
	eq(t, c.Status, ChoiceWaiting)
	w := e.waitList()
	eq(t, len(w) == 1 && w[0].Failed, true)
	eq(t, e.outcomes(id)[rtA], "route_wait/")
}

func TestSameConversationNoCall(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), decisionstest.Shares("route", "r1", 0.8, "r2", 0.1, "nobody", 0.1))
	e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	id2 := e.send("m2", "t1", "ann@x.io", "and more")
	eq(t, e.outcomes(id2)[rtA], "queued/conversation")
	eq(t, e.outcomes(id2)[rtB], "not_chosen/conversation")
	eq(t, e.fake.Calls(), 1)
	eq(t, e.queued(), 2)
}

func TestSameConversationSenderBlocked(t *testing.T) {
	f := chosenRoutes(rtA, rtB)
	f.Routes[0].Filter = job.TriggerYAML{Allowlist: []string{"boss@x.io"}}
	e := newRouterEnv(t, f, decisionstest.Shares("route", "r1", 0.8, "r2", 0.1, "nobody", 0.1))
	e.send("m1", "t1", "boss@x.io", "hello")
	e.decide()
	id2 := e.send("m2", "t1", "eve@x.io", "let me in")
	eq(t, e.outcomes(id2)[rtA], "blocked/conversation")
	eq(t, e.queued(), 1)
}

func TestReplyGoneTargetRoutesAgain(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB, rtC),
		decisionstest.Shares("route", "r1", 0.8, "r2", 0.1, "r3", 0.05, "nobody", 0.05),
		decisionstest.Shares("route", "r1", 0.1, "r2", 0.8, "nobody", 0.1))
	e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	e.rewrite(chosenRoutes(rtB, rtC))
	id2 := e.send("m2", "t1", "ann@x.io", "again")
	eq(t, e.outcomes(id2)[rtB], "route_wait/")
	e.decide()
	eq(t, e.fake.Calls(), 2)
	eq(t, e.outcomes(id2)[rtC], "queued/router")
	eq(t, e.outcomes(id2)[rtB], "not_chosen/router")
	c, _ := e.choice("t1")
	eq(t, c.Status == ChoiceChosen && c.RouteID == rtC, true)
}

func TestWaitingConversationJoins(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), decisionstest.Shares("route", "r1", 0.8, "r2", 0.1, "nobody", 0.1))
	id1 := e.send("m1", "t1", "ann@x.io", "hello")
	id2 := e.send("m2", "t1", "ann@x.io", "one more thing")
	eq(t, e.outcomes(id2)[rtA], "route_wait/")
	eq(t, e.fake.Calls(), 0)
	e.decide()
	eq(t, e.fake.Calls(), 1)
	eq(t, e.outcomes(id1)[rtA], "queued/router")
	eq(t, e.outcomes(id2)[rtA], "queued/router")
	eq(t, e.outcomes(id2)[rtB], "not_chosen/router")
	var first, second string
	rows, err := e.st.DB().Query(`SELECT event_id FROM event_queue ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	rows.Next()
	_ = rows.Scan(&first)
	rows.Next()
	_ = rows.Scan(&second)
	eq(t, first, id1)
	eq(t, second, id2)
}

func TestNobodyConversationRoutesAgain(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB),
		decisionstest.Shares("route", "r1", 0.05, "r2", 0.05, "nobody", 0.9),
		decisionstest.Shares("route", "r1", 0.9, "r2", 0.05, "nobody", 0.05))
	e.send("m1", "t1", "ann@x.io", "hello")
	e.decide()
	id2 := e.send("m2", "t1", "ann@x.io", "actually, help")
	e.decide()
	eq(t, e.fake.Calls(), 2)
	eq(t, e.outcomes(id2)[rtA], "queued/router")
}

func TestRouterSurvivesRestart(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB), decisionstest.Shares("route", "r1", 0.8, "r2", 0.1, "nobody", 0.1))
	id := e.send("m1", "t1", "ann@x.io", "hello")
	c, _ := e.choice("t1")
	eq(t, c.Status, ChoiceDeciding)

	ds := decisions.NewService(e.st, nil, func() config.DecisionsConfig { return config.DecisionsConfig{Decider: "fake"} })
	ds.Register(policies.Route())
	ds.AddDecider("fake", e.fake)
	next := New(e.st, e.svc.ws, nil).WithRouteDecider(ds)
	next.routerEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	next.wg.Add(1)
	go next.runRouter(ctx)
	t.Cleanup(func() {
		cancel()
		next.wg.Wait()
	})
	waittest.Until(t, "the new service decides", func() bool { return e.outcomes(id)[rtA] == "queued/router" })
}

func TestEveryMessageStillQueues(t *testing.T) {
	f := chosenRoutes(rtB, rtC)
	f.Routes = append([]routing.Route{{ID: rtA, Agent: "alpha", Gets: routing.GetsEveryMessage}}, f.Routes...)
	e := newRouterEnv(t, f, decisionstest.Shares("route", "r1", 0.8, "r2", 0.1, "nobody", 0.1))
	id := e.send("m1", "t1", "ann@x.io", "hello")
	eq(t, e.outcomes(id)[rtA], "queued/")
	eq(t, e.outcomes(id)[rtB], "route_wait/")
	e.decide()
	eq(t, e.outcomes(id)[rtB], "queued/router")
	eq(t, e.queued(), 2)
}

func TestModeOffUnchanged(t *testing.T) {
	f := chosenRoutes(rtA, rtB)
	f.Mode = routing.ModeOff
	e := newRouterEnv(t, f)
	id := e.send("m1", "t1", "ann@x.io", "hello")
	eq(t, e.outcomes(id)[rtA], "queued/")
	eq(t, e.outcomes(id)[rtB], "queued/")
	eq(t, e.fake.Calls(), 0)
	_, ok := e.choice("t1")
	eq(t, ok, false)
}

func TestPruneChoicesKeepsWaiting(t *testing.T) {
	e := newRouterEnv(t, chosenRoutes(rtA, rtB))
	old := time.Now().Add(-200 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	for _, st := range []string{"chosen", "waiting", "deciding", "nobody"} {
		if _, err := e.st.DB().Exec(`INSERT INTO route_choices (workspace_id, connection, conversation_key, status, method, first_event_id, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?)`, e.wsID, "email", st, st, "router", "e1", old, old); err != nil {
			t.Fatal(err)
		}
	}
	n, err := e.svc.PruneChoices(context.Background(), time.Now())
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	var left int
	if err := e.st.DB().QueryRow(`SELECT COUNT(*) FROM route_choices WHERE status IN ('waiting','deciding')`).Scan(&left); err != nil || left != 2 {
		t.Fatalf("kept %d, %v", left, err)
	}
}

func newRouterEnv(t *testing.T, f routing.File, steps ...decisionstest.Step) *routerEnv {
	t.Helper()
	st, svc := newInternalService(t)
	e := &routerEnv{t: t, svc: svc, st: st, wsPath: filepath.Join(home.Base(), "ws"), fake: decisionstest.New(steps...)}
	for _, r := range f.Routes {
		if r.Agent != "" {
			writeAgentFile(t, e.wsPath, r.Agent)
		}
	}
	routingtest.Write(t, e.wsPath, "email", f)
	w, err := svc.ws.Add(context.Background(), e.wsPath, "route-ws")
	if err != nil {
		t.Fatal(err)
	}
	e.wsID = w.ID
	e.reindex()
	ds := decisions.NewService(st, nil, func() config.DecisionsConfig { return config.DecisionsConfig{Decider: "fake"} })
	ds.Register(policies.Route())
	ds.AddDecider("fake", e.fake)
	svc.WithRouteDecider(ds).OnRouteWait(func(_ context.Context, w RouteWait) (string, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.waits = append(e.waits, w)
		return "inb_1", nil
	})
	return e
}

func chosenRoutes(ids ...string) routing.File {
	f := routing.File{Mode: routing.ModeOn, SureAt: 0.7, Unsure: routing.UnsureAsk, NoFit: routing.NoFitDrop}
	names := map[string]string{rtA: "alpha", rtB: "beta", rtC: "gamma"}
	for _, id := range ids {
		f.Routes = append(f.Routes, routing.Route{ID: id, Agent: names[id], Gets: routing.GetsChosen, UseFor: "work of " + names[id]})
	}
	return f
}

func eq(t *testing.T, got, want any) {
	t.Helper()
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}
