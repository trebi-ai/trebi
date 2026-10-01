package connectors_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
)

func TestGateStages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := map[string]any{"text": "deploy"}
	cases := []struct {
		name     string
		gate     connectors.Gate
		room, id string
		matched  bool
		stage    string
		errSet   bool
	}{
		{name: "open", gate: connectors.Gate{Match: "true"}, matched: true},
		{name: "room hit", gate: connectors.Gate{Rooms: []string{"r1"}, Match: "true"}, room: "r1", matched: true},
		{name: "room miss", gate: connectors.Gate{Rooms: []string{"r1"}, Match: "true"}, room: "r2", stage: connectors.StageRoom},
		{name: "sender miss", gate: connectors.Gate{Allowlist: []string{"alice"}, Match: "true"}, id: "bob", stage: connectors.StageSender},
		{name: "empty identity closed", gate: connectors.Gate{Allowlist: []string{"alice"}, Match: "true"}, stage: connectors.StageSender},
		{name: "sender exact case", gate: connectors.Gate{Allowlist: []string{"Alice@x.io"}, Match: "true"}, id: "alice@x.io", stage: connectors.StageSender},
		{name: "sender fold", gate: connectors.Gate{Allowlist: []string{"Alice@x.io"}, Match: "true", Fold: true}, id: "alice@x.io", matched: true},
		{name: "extract error", gate: connectors.Gate{Extract: "nope(", Match: "true"}, stage: connectors.StageExtract, errSet: true},
		{name: "match false", gate: connectors.Gate{Match: "event.text === 'x'"}, stage: connectors.StageMatch},
		{name: "match error", gate: connectors.Gate{Match: "nope("}, stage: connectors.StageMatch, errSet: true},
	}
	for _, c := range cases {
		res := c.gate.Eval(ctx, c.room, c.id, payload)
		if res.Matched != c.matched || res.Stage != c.stage || (res.Error != "") != c.errSet {
			t.Errorf("%s: got %+v", c.name, res)
		}
	}
	res := connectors.Gate{Extract: "({who: event.text})", Match: "event.who === 'deploy'"}.Eval(ctx, "", "", payload)
	if !res.Matched || res.Vars["who"] != "deploy" || res.Extracted.(map[string]any)["who"] != "deploy" {
		t.Fatalf("extract: %+v", res)
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
	if n := count(`SELECT COUNT(*) FROM event_queue WHERE job='scoped'`); n != 0 {
		t.Fatalf("room miss enqueued %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM event_outcomes WHERE job='scoped' AND outcome='no_match'`); n != 1 {
		t.Fatalf("room miss no_match rows = %d", n)
	}
	if _, err := svc.InjectNative(ctx, "email", map[string]any{"n": 2}, connectors.NativeMeta{SourceID: "s2", Room: "r1"}); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM event_queue WHERE job='scoped'`); n != 1 {
		t.Fatalf("room hit enqueued %d", n)
	}
}
