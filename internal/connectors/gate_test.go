package connectors_test

import (
	"context"
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

// TestDraftGateEventProtocolShape proves a draft with the protocol event
// shape gives its room and sender, and any other value gives neither.
func TestDraftGateEventProtocolShape(t *testing.T) {
	t.Parallel()
	ev := connectors.DraftGateEvent("whatsapp", map[string]any{
		"id": "m1", "type": "message", "room": map[string]any{"id": "r1"}, "sender": map[string]any{"id": "u1", "name": "Ana"},
	})
	if ev.Room != "r1" || ev.Identity != "u1" {
		t.Fatalf("protocol draft = %+v", ev)
	}
	if ev := connectors.DraftGateEvent("", map[string]any{"room": "r1"}); ev.Room != "" || ev.Identity != "" {
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
