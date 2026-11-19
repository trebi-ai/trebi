package connectors_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func TestGateStages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := map[string]any{"text": "deploy"}
	cases := []struct {
		name    string
		gate    connectors.Gate
		in      connectors.GateInput
		matched bool
		stage   string
		errSet  bool
	}{
		{name: "open", gate: connectors.Gate{Match: "true"}, matched: true},
		{name: "room hit", gate: connectors.Gate{Rooms: []string{"r1"}, Match: "true"}, in: connectors.GateInput{Room: "r1"}, matched: true},
		{name: "room miss", gate: connectors.Gate{Rooms: []string{"r1"}, Match: "true"}, in: connectors.GateInput{Room: "r2"}, stage: connectors.StageRoom},
		{name: "sender miss", gate: connectors.Gate{Allowlist: []string{"alice"}, Match: "true"}, in: connectors.GateInput{Identity: "bob"}, stage: connectors.StageSender},
		{name: "empty identity closed", gate: connectors.Gate{Allowlist: []string{"alice"}, Match: "true"}, stage: connectors.StageSender},
		{name: "sender exact case", gate: connectors.Gate{Allowlist: []string{"Alice@x.io"}, Match: "true"}, in: connectors.GateInput{Identity: "alice@x.io"}, stage: connectors.StageSender},
		{name: "sender fold", gate: connectors.Gate{Allowlist: []string{"Alice@x.io"}, Match: "true", Fold: true}, in: connectors.GateInput{Identity: "alice@x.io"}, matched: true},
		{name: "extract error", gate: connectors.Gate{Extract: "nope(", Match: "true"}, stage: connectors.StageExtract, errSet: true},
		{name: "match false", gate: connectors.Gate{Match: "event.text === 'x'"}, stage: connectors.StageMatch},
		{name: "match error", gate: connectors.Gate{Match: "nope("}, stage: connectors.StageMatch, errSet: true},
		{name: "no match takes all", gate: connectors.Gate{}, matched: true},
		{name: "people only", gate: connectors.Gate{From: job.FromPeople}, in: connectors.GateInput{Bot: true}, stage: connectors.StageFrom},
		{name: "bots only", gate: connectors.Gate{From: job.FromBots}, in: connectors.GateInput{Bot: true}, matched: true},
		{name: "bots only, person", gate: connectors.Gate{From: job.FromBots}, stage: connectors.StageFrom},
		{name: "anyone", gate: connectors.Gate{From: job.FromAnyone}, in: connectors.GateInput{Bot: true}, matched: true},
		{name: "contains miss", gate: connectors.Gate{Contains: []string{"failure"}}, in: connectors.GateInput{Text: "all good"}, stage: connectors.StageContains},
		{name: "contains any, no case", gate: connectors.Gate{Contains: []string{"x", "FAILURE"}}, in: connectors.GateInput{Text: "Run failure in test"}, matched: true},
		{name: "from before contains", gate: connectors.Gate{From: job.FromBots, Contains: []string{"x"}}, stage: connectors.StageFrom},
		{name: "room before from", gate: connectors.Gate{Rooms: []string{"r1"}, From: job.FromBots}, in: connectors.GateInput{Room: "r2"}, stage: connectors.StageRoom},
		{name: "follow-up skips from, contains, match", gate: connectors.Gate{From: job.FromBots, Contains: []string{"x"}, Match: "false"},
			in: connectors.GateInput{FollowUp: true}, matched: true},
		{name: "follow-up keeps rooms", gate: connectors.Gate{Rooms: []string{"r1"}}, in: connectors.GateInput{Room: "r2", FollowUp: true}, stage: connectors.StageRoom},
		{name: "follow-up keeps the allowlist", gate: connectors.Gate{Allowlist: []string{"a"}}, in: connectors.GateInput{Identity: "b", FollowUp: true}, stage: connectors.StageSender},
	}
	for _, c := range cases {
		res := c.gate.Eval(ctx, c.in, payload)
		if res.Matched != c.matched || res.Stage != c.stage || (res.Error != "") != c.errSet {
			t.Errorf("%s: got %+v", c.name, res)
		}
	}
	res := connectors.Gate{Extract: "({who: event.text})", Match: "event.who === 'deploy'"}.Eval(ctx, connectors.GateInput{}, payload)
	if !res.Matched || res.Vars["who"] != "deploy" || res.Extracted.(map[string]any)["who"] != "deploy" {
		t.Fatalf("extract: %+v", res)
	}
	res = connectors.Gate{Extract: "({who: event.text})", Match: "false"}.Eval(ctx, connectors.GateInput{FollowUp: true}, payload)
	if !res.Matched || !res.FollowUp || res.Vars["who"] != "deploy" {
		t.Fatalf("follow-up extract: %+v", res)
	}
}

// TestGateJSCases runs the shared extract and match cases. perception
// vendors testdata/js_cases.json byte-identical and runs the same cases.
func TestGateJSCases(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "js_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []struct {
			Name      string          `json:"name"`
			Event     any             `json:"event"`
			Extract   string          `json:"extract"`
			Match     string          `json:"match"`
			Matched   bool            `json:"matched"`
			Stage     string          `json:"stage"`
			Error     bool            `json:"error"`
			Extracted json.RawMessage `json:"extracted"`
		} `json:"cases"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&file); err != nil || len(file.Cases) == 0 {
		t.Fatalf("cases: %d, %v", len(file.Cases), err)
	}
	for _, c := range file.Cases {
		res := connectors.Gate{Extract: c.Extract, Match: c.Match}.Eval(t.Context(), connectors.GateInput{}, c.Event)
		if res.Matched != c.Matched || res.Stage != c.Stage || (res.Error != "") != c.Error {
			t.Errorf("%s: got %+v", c.Name, res)
		}
		if c.Extracted == nil {
			continue
		}
		var want any
		if err := json.Unmarshal(c.Extracted, &want); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(res.Extracted)
		if w, _ := json.Marshal(want); !bytes.Equal(got, w) {
			t.Errorf("%s: extracted %s, want %s", c.Name, got, w)
		}
	}
}

// TestDraftGateEventProtocolShape proves a draft with the protocol event
// shape gives its room and sender, and any other value gives neither.
func TestDraftGateEventProtocolShape(t *testing.T) {
	t.Parallel()
	ev := connectors.DraftGateEvent("whatsapp", false, map[string]any{
		"id": "m1", "type": "message", "room": map[string]any{"id": "r1"}, "sender": map[string]any{"id": "u1", "name": "Ana"},
	})
	if ev.Room != "r1" || ev.Identity != "u1" {
		t.Fatalf("protocol draft = %+v", ev)
	}
	if ev := connectors.DraftGateEvent("", false, map[string]any{"room": "r1"}); ev.Room != "" || ev.Identity != "" {
		t.Fatalf("plain draft = %+v", ev)
	}
}

// TestPipelineRoomScope proves a room miss records no_match and enqueues
// nothing, and a room hit enqueues.
func TestPipelineRoomScope(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	routeAgent(t, wsPath, "scoped", "email", job.TriggerYAML{Match: "true", Rooms: []string{"r1"}})
	w, err := ws.Add(ctx, wsPath, "rooms-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := st.DB().QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := svc.InjectNative(ctx, "email", map[string]any{"n": 1}, connectors.NativeMeta{SourceID: "s1", Room: "r2"}); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='agents/scoped')`); n != 0 {
		t.Fatalf("room miss enqueued %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM event_outcomes WHERE job_id=(SELECT id FROM job_index WHERE name='agents/scoped') AND outcome='no_match'`); n != 1 {
		t.Fatalf("room miss no_match rows = %d", n)
	}
	if _, err := svc.InjectNative(ctx, "email", map[string]any{"n": 2}, connectors.NativeMeta{SourceID: "s2", Room: "r1"}); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='agents/scoped')`); n != 1 {
		t.Fatalf("room hit enqueued %d", n)
	}
}

const typedJob = `---
harness: fake
inputs:
  body: {type: string, label: Email text, required: true}
  tone: {type: string, default: short}
---

Reply to {{body}} in a {{tone}} tone.
`

func TestGateTypesAndInputs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j, err := job.ParseBytes("reply/JOB.md", []byte(typedJob))
	if err != nil || j.ParseError != "" {
		t.Fatalf("parse: %v %s", err, j.ParseError)
	}
	auto := routing.Route{Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Types: []string{"mail.received"}}}.Trigger("email")
	manual := routing.Route{Gets: routing.GetsByHand, Inputs: job.InputMap{{Name: "body", Value: "{{data.text}}"}}}.Trigger("email")
	in := connectors.GateInput{Type: "mail.received"}

	res := connectors.GateFor(auto, "email").WithInputs(j, auto).Eval(ctx, connectors.GateInput{Type: "mail.sent"}, map[string]any{})
	if res.Matched || res.Stage != connectors.StageType {
		t.Fatalf("type miss: %+v", res)
	}
	res = connectors.GateFor(auto, "email").WithInputs(j, auto).Eval(ctx, in, map[string]any{"data": map[string]any{}})
	if res.Matched || res.Stage != connectors.StageInputs || res.Error != "Email text is empty" {
		t.Fatalf("automatic empty input: %+v", res)
	}
	res = connectors.GateFor(auto, "email").WithInputs(j, auto).Eval(ctx, in, map[string]any{"data": map[string]any{"body": "hi"}})
	if !res.Matched || res.Filled == nil || res.Filled.Values["body"] != "hi" || res.Filled.Values["tone"] != "short" {
		t.Fatalf("automatic fill: %+v", res)
	}
	res = connectors.GateFor(manual, "email").WithInputs(j, manual).Eval(ctx, in, map[string]any{"data": map[string]any{}})
	if !res.Matched || res.Filled == nil || len(res.Filled.Missing) != 1 || res.Filled.Missing[0] != "body" {
		t.Fatalf("manual keeps the empty input for the form: %+v", res)
	}
	res = connectors.GateFor(manual, "email").WithInputs(j, manual).Eval(ctx, in, map[string]any{"data": map[string]any{"text": "from map"}})
	if res.Filled.Values["body"] != "from map" || res.Filled.Sources["body"] != job.SourceMap {
		t.Fatalf("manual map fill: %+v", res.Filled)
	}
}

// TestPipelineManualOutcome proves that a manual trigger stores outcome
// manual and no queue row, and that an automatic trigger with an empty
// required input stores error with the reason.
func TestPipelineManualOutcome(t *testing.T) {
	st, ws, svc := openStack(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	writeAgent(t, wsPath, "mailer")
	for name, md := range map[string]string{
		"by-hand": "---\nharness: fake\ninputs:\n  body: {type: string, required: true}\n---\n\nReply to {{body}}.\n",
		"auto":    "---\nharness: fake\ninputs:\n  body: {type: string, label: Email text, required: true}\n---\n\nReply to {{body}}.\n",
	} {
		dir := filepath.Join(wsPath, "agents", "mailer", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "JOB.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	routingtest.Write(t, wsPath, "email", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{Job: "mailer/by-hand", Gets: routing.GetsByHand},
		{Job: "mailer/auto", Gets: routing.GetsEveryMessage},
	}})
	w, err := ws.Add(ctx, wsPath, "manual-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	id, err := svc.InjectNative(ctx, "email", map[string]any{"subject": "hi"}, connectors.NativeMeta{SourceID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	outcome := func(name string) (string, string, string) {
		t.Helper()
		var o, msg, qid string
		if err := st.DB().QueryRow(`SELECT outcome, COALESCE(error_message,''), COALESCE(queue_id,'') FROM event_outcomes
			WHERE event_id=? AND job_id=(SELECT id FROM job_index WHERE name=?)`, id, name).Scan(&o, &msg, &qid); err != nil {
			t.Fatal(err)
		}
		return o, msg, qid
	}
	if o, _, qid := outcome("agents/mailer/by-hand"); o != "manual" || qid != "" {
		t.Fatalf("manual trigger: outcome %q queue %q", o, qid)
	}
	if o, msg, _ := outcome("agents/mailer/auto"); o != "error" || msg != "Email text is empty" {
		t.Fatalf("automatic trigger: outcome %q message %q", o, msg)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_queue`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("queue rows = %d, %v", n, err)
	}

	ev, err := svc.StoredGateEvent(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(svc.EventTriggers(ctx, ev)); got != 2 {
		t.Fatalf("event triggers = %d", got)
	}
	for _, et := range svc.EventTriggers(ctx, ev) {
		tr, res, err := svc.HandRoute(ctx, ev, w.ID, et.Row.Name, "", false)
		if err != nil || tr.Route != et.Route.ID || !res.Matched || len(res.Filled.Missing) != 1 {
			t.Fatalf("%s: hand route %+v %+v %v", et.Row.Name, tr, res, err)
		}
	}
}

// TestDraftGateEventThreadless proves a draft takes the lane rule of a
// channel without threads.
func TestDraftGateEventThreadless(t *testing.T) {
	t.Parallel()
	payload := map[string]any{
		"id": "m9", "type": "message", "room": map[string]any{"id": "r9", "kind": "group"}, "sender": map[string]any{"id": "u1"},
	}
	if got := connectors.DraftGateEvent("whatsapp", true, payload).ConversationKey(true, nil); got != "r9" {
		t.Fatalf("threadless key = %q, want r9", got)
	}
	if got := connectors.DraftGateEvent("whatsapp", false, payload).ConversationKey(true, nil); got != "r9/m9" {
		t.Fatalf("threaded key = %q, want r9/m9", got)
	}
}

// TestDispatchFromRoutes proves routing off: every route that passes queues
// (two agents on one room both run), a miss writes no_match, a by-hand route
// writes manual, and a route that gets nothing writes nothing.
func TestDispatchFromRoutes(t *testing.T) {
	rooms := func(r ...string) job.TriggerYAML { return job.TriggerYAML{Rooms: r} }
	st, svc, _ := routeStack(t, []string{"a1", "a2", "a3", "a4", "a5"}, "email", routing.File{Routes: []routing.Route{
		{Agent: "a1", Gets: routing.GetsEveryMessage, Filter: rooms("r1")},
		{Agent: "a2", Gets: routing.GetsEveryMessage, Filter: rooms("r1")},
		{Agent: "a3", Gets: routing.GetsEveryMessage, Filter: rooms("r9")},
		{Agent: "a4", Gets: routing.GetsByHand},
		{Agent: "a5", Gets: routing.GetsOff},
	}})
	id, err := svc.InjectNative(context.Background(), "email", map[string]any{"n": 1}, connectors.NativeMeta{SourceID: "m1", Room: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"agents/a1:queued", "agents/a2:queued", "agents/a3:no_match", "agents/a4:manual"}
	if got := outcomes(t, st, id); !slices.Equal(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
}

// TestDispatchModeOnNoRouter proves that with mode on and no decider, a route
// that gets every message still queues, and the chosen routes that pass wait.
func TestDispatchModeOnNoRouter(t *testing.T) {
	st, svc, _ := routeStack(t, []string{"x", "y", "z"}, "email", routing.File{Mode: routing.ModeOn, Routes: []routing.Route{
		{Agent: "x", Gets: routing.GetsEveryMessage},
		{Agent: "y", Gets: routing.GetsChosen, UseFor: "bugs"},
		{Agent: "z", Gets: routing.GetsChosen, UseFor: "billing"},
	}})
	id, err := svc.InjectNative(context.Background(), "email", map[string]any{"n": 1}, connectors.NativeMeta{SourceID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"agents/x:queued", "agents/y:route_wait", "agents/z:route_wait"}
	if got := outcomes(t, st, id); !slices.Equal(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
}

// TestOutcomeRouteID proves two routes of one job write two outcome rows
// with the two route ids.
func TestOutcomeRouteID(t *testing.T) {
	st, svc, _ := routeStack(t, []string{"ops"}, "email", routing.File{Routes: []routing.Route{
		{ID: "rt_00000000f1", Agent: "ops", Gets: routing.GetsEveryMessage},
		{ID: "rt_00000000f2", Agent: "ops", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Contains: []string{"zzz"}}},
	}})
	id, err := svc.InjectNative(context.Background(), "email", map[string]any{"n": 1}, connectors.NativeMeta{SourceID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.DB().Query(`SELECT route_id, outcome FROM event_outcomes WHERE event_id=? ORDER BY route_id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var rid, o string
		if err := rows.Scan(&rid, &o); err != nil {
			t.Fatal(err)
		}
		got = append(got, rid+":"+o)
	}
	if want := []string{"rt_00000000f1:queued", "rt_00000000f2:no_match"}; !slices.Equal(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
}

// TestHandRoute proves the choice of route for a by-hand run.
func TestHandRoute(t *testing.T) {
	_, svc, w := routeStack(t, []string{"ops", "other"}, "email", routing.File{Routes: []routing.Route{
		{ID: "rt_00000000a1", Agent: "ops", Gets: routing.GetsByHand},
		{ID: "rt_00000000a2", Agent: "ops", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Match: "false"}},
		{ID: "rt_00000000a3", Agent: "other", Gets: routing.GetsEveryMessage},
	}})
	ctx := context.Background()
	id, err := svc.InjectNative(ctx, "email", map[string]any{"n": 1}, connectors.NativeMeta{SourceID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := svc.StoredGateEvent(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, route string
		force       bool
		want        string
		err         error
	}{
		{"first by hand", "", false, "rt_00000000a1", nil},
		{"by id", "rt_00000000a2", false, "", connectors.ErrNotAccepted},
		{"by id, forced", "rt_00000000a2", true, "rt_00000000a2", nil},
		{"route of another target", "rt_00000000a3", false, "", connectors.ErrNotAccepted},
	} {
		tr, _, err := svc.HandRoute(ctx, ev, w.ID, "agents/ops", tc.route, tc.force)
		if !errors.Is(err, tc.err) || tr.Route != tc.want {
			t.Errorf("%s: route %q, err %v; want %q, %v", tc.name, tr.Route, err, tc.want, tc.err)
		}
	}
}

// TestRouteIssues proves the route check reads the manifest of the
// connection: an unknown event type is an issue, and the email connection
// has no manifest check.
func TestRouteIssues(t *testing.T) {
	_, ws, svc := openStack(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	writeAgent(t, wsPath, "ops")
	if _, err := svc.Create(ctx, inline("src", func(m *manifest.Manifest) {
		m.Events = &manifest.Events{Command: "true", Format: manifest.FormatJSON, Types: []manifest.EventType{{Type: "message"}}}
	})); err != nil {
		t.Fatal(err)
	}
	routingtest.Write(t, wsPath, "src", routing.File{Routes: []routing.Route{
		{ID: "rt_00000000b1", Agent: "ops", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Types: []string{"nope"}}},
	}})
	w, err := ws.Add(ctx, wsPath, "issue-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	_, issues := svc.JobRouteIssues(ctx, w.ID, "agents/ops")
	if !slices.ContainsFunc(issues, func(is job.Issue) bool { return is.Code == job.CodeEventTypeUnknown }) {
		t.Fatalf("issues = %+v", issues)
	}
	routingtest.Write(t, wsPath, "email", routing.File{Routes: []routing.Route{
		{ID: "rt_00000000b2", Agent: "ops", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Types: []string{"nope"}}},
	}})
	routingtest.Write(t, wsPath, "src", routing.File{})
	if _, issues := svc.JobRouteIssues(ctx, w.ID, "agents/ops"); slices.ContainsFunc(issues, func(is job.Issue) bool { return is.Code == job.CodeEventTypeUnknown }) {
		t.Fatalf("email issues = %+v", issues)
	}
}

// TestListRoutingProblems proves a connection row counts the problems of
// its routes file in the workspace, and a workspace with no file counts none.
func TestListRoutingProblems(t *testing.T) {
	_, ws, svc := openStack(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	writeAgent(t, wsPath, "ops")
	if _, err := svc.Create(ctx, inline("src", func(m *manifest.Manifest) {
		m.Events = &manifest.Events{Command: "true", Format: manifest.FormatJSON, Types: []manifest.EventType{{Type: "message"}}}
	})); err != nil {
		t.Fatal(err)
	}
	routingtest.Write(t, wsPath, "src", routing.File{Routes: []routing.Route{
		{ID: "rt_00000000c1", Agent: "ops", Gets: routing.GetsOff},
		{ID: "rt_00000000c2", Agent: "gone", Gets: routing.GetsOff},
	}})
	w, err := ws.Add(ctx, wsPath, "count-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	count := func(workspaceID string) any {
		for _, row := range svc.List(workspaceID) {
			if row["slug"] == "src" {
				return row["routing_problems"]
			}
		}
		t.Fatal("no src row")
		return nil
	}
	if got := count(w.ID); got != 1 {
		t.Fatalf("routing_problems = %v, want 1", got)
	}
	if got := count(""); got != 0 {
		t.Fatalf("no workspace: routing_problems = %v, want 0", got)
	}
}

// routeStack builds a workspace of the given agents, writes the file of one
// connection, and indexes it.
func routeStack(t *testing.T, agents []string, conn string, f routing.File) (*store.Store, *connectors.Service, *workspaces.Workspace) {
	t.Helper()
	st, ws, svc := openStack(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	for _, a := range agents {
		writeAgent(t, wsPath, a)
	}
	routingtest.Write(t, wsPath, conn, f)
	w, err := ws.Add(ctx, wsPath, "route-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	return st, svc, w
}

// outcomes lists "<job>:<outcome>" of the event, sorted.
func outcomes(t *testing.T, st *store.Store, eventID string) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT COALESCE((SELECT name FROM job_index WHERE id=o.job_id), o.job_id), o.outcome
		FROM event_outcomes o WHERE o.event_id=?`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, o string
		if err := rows.Scan(&name, &o); err != nil {
			t.Fatal(err)
		}
		out = append(out, name+":"+o)
	}
	slices.Sort(out)
	return out
}
