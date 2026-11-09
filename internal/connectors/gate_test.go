package connectors_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
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
	jobDir := filepath.Join(wsPath, "scoped")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\ntriggers:\n  - event: email\n    match: \"true\"\n    rooms: [\"r1\"]\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
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
	if n := count(`SELECT COUNT(*) FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='scoped')`); n != 0 {
		t.Fatalf("room miss enqueued %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM event_outcomes WHERE job_id=(SELECT id FROM job_index WHERE name='scoped') AND outcome='no_match'`); n != 1 {
		t.Fatalf("room miss no_match rows = %d", n)
	}
	if _, err := svc.InjectNative(ctx, "email", map[string]any{"n": 2}, connectors.NativeMeta{SourceID: "s2", Room: "r1"}); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='scoped')`); n != 1 {
		t.Fatalf("room hit enqueued %d", n)
	}
}

const typedJob = `---
harness: fake
inputs:
  body: {type: string, label: Email text, required: true}
  tone: {type: string, default: short}
triggers:
  - event: email
    types: [mail.received]
  - event: email
    manual: true
    inputs: {body: "{{data.text}}"}
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
	auto, manual := j.Triggers[0], j.Triggers[1]
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
	for name, md := range map[string]string{
		"by-hand": "---\nharness: fake\ninputs:\n  body: {type: string, required: true}\ntriggers:\n  - event: email\n    manual: true\n---\n\nReply to {{body}}.\n",
		"auto":    "---\nharness: fake\ninputs:\n  body: {type: string, label: Email text, required: true}\ntriggers:\n  - event: email\n---\n\nReply to {{body}}.\n",
	} {
		dir := filepath.Join(wsPath, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "JOB.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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
	if o, _, qid := outcome("by-hand"); o != "manual" || qid != "" {
		t.Fatalf("manual trigger: outcome %q queue %q", o, qid)
	}
	if o, msg, _ := outcome("auto"); o != "error" || msg != "Email text is empty" {
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
	idx := 0
	for _, et := range svc.EventTriggers(ctx, ev) {
		tr, res, err := ev.HandTrigger(ctx, et.Job, &idx, false)
		if err != nil || tr.Index != 0 || !res.Matched || len(res.Filled.Missing) != 1 {
			t.Fatalf("%s: hand trigger %+v %+v %v", et.Row.Name, tr, res, err)
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
