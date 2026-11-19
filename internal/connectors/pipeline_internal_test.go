package connectors

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
)

func TestFillLinesMeta(t *testing.T) {
	full := chatEvents("x")
	cases := []struct {
		name    string
		ev      manifest.Events
		payload string
		want    eventRecord
	}{
		{
			name: "every field",
			ev:   full,
			payload: `{"id":"m1","kind":"message","text":"hi","thread_id":"t1",
				"channel":{"id":"c1","name":"General"},"user":{"id":7,"name":"Ann"}}`,
			want: eventRecord{
				sourceID: "m1", identity: "7", sender: "Ann", eventType: "message", room: "c1", roomName: "General",
				thread: "t1", text: "hi", root: "t1", builtins: true,
			},
		},
		{
			name:    "identity path wins over the sender id",
			ev:      func() manifest.Events { e := full; e.Identity = "user.name"; return e }(),
			payload: `{"id":"m1","user":{"id":"u1","name":"Ann"}}`,
			want:    eventRecord{sourceID: "m1", identity: "Ann", sender: "Ann", text: "line", root: "m1", builtins: true},
		},
		{
			name:    "sender id without a name",
			ev:      full,
			payload: `{"id":"m1","user":{"id":"u1"}}`,
			want:    eventRecord{sourceID: "m1", identity: "u1", sender: "u1", text: "line", root: "m1", builtins: true},
		},
		{
			name:    "missing paths stay empty",
			ev:      full,
			payload: `{"other":1}`,
			want:    eventRecord{text: "line", builtins: true},
		},
		{
			name:    "no message keys",
			ev:      manifest.Events{Command: "x", Format: manifest.FormatJSON, ID: "id", Identity: "who"},
			payload: `{"id":"m1","who":"bob","text":"hi"}`,
			want:    eventRecord{sourceID: "m1", identity: "bob", text: "line", root: "m1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := eventRecord{payload: decodeLine(&tc.ev, strings.Join(strings.Fields(tc.payload), " ")), text: "line"}
			if err := fillLinesMeta(&rec, manifest.Manifest{Events: &tc.ev}); err != nil {
				t.Fatal(err)
			}
			rec.payload = nil
			if rec != tc.want {
				t.Fatalf("got  %+v\nwant %+v", rec, tc.want)
			}
		})
	}
}

// TestLinesMappedColumns proves the mapped fields reach the events columns,
// and a missing path stays NULL.
func TestLinesMappedColumns(t *testing.T) {
	st, svc := newInternalService(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, linesEntry("chat", chatEvents("sleep 30"))); err != nil {
		t.Fatal(err)
	}
	full := map[string]any{
		"id": "m1", "kind": "message", "text": "hi", "thread_id": "t1",
		"channel": map[string]any{"id": "c1", "name": "General"}, "user": map[string]any{"id": "u1", "name": "Ann"},
	}
	id, err := svc.InjectEvent(ctx, "chat", full)
	if err != nil {
		t.Fatal(err)
	}
	cols := func(id string) []sql.NullString {
		t.Helper()
		out := make([]sql.NullString, 7)
		if err := st.DB().QueryRow(`SELECT identity, room, thread, sender, event_type, text, root FROM events WHERE id=?`, id).
			Scan(&out[0], &out[1], &out[2], &out[3], &out[4], &out[5], &out[6]); err != nil {
			t.Fatal(err)
		}
		return out
	}
	want := []string{"u1", "c1", "t1", "Ann", "message", "hi", "t1"}
	for i, c := range cols(id) {
		if !c.Valid || c.String != want[i] {
			t.Fatalf("column %d = %+v, want %q", i, c, want[i])
		}
	}

	id, err = svc.InjectEvent(ctx, "chat", map[string]any{"id": "m2"})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cols(id)[:5] {
		if c.Valid {
			t.Fatalf("column %d = %q, want NULL", i, c.String)
		}
	}

	// A stored event gets the built-in vars back.
	ev, err := svc.StoredGateEvent(ctx, mustEventID(t, st, "m1"))
	if err != nil {
		t.Fatal(err)
	}
	if v := ev.Vars(nil); v["room_name"] != "General" || v["sender"] != "Ann" || v["text"] != "hi" {
		t.Fatalf("vars = %v", v)
	}
}

// TestLinesUnknownTypeDropped proves a type that events.types does not
// declare is dropped with a log line, and a known type is stored with its
// thread row.
func TestLinesUnknownTypeDropped(t *testing.T) {
	st, svc := newInternalService(t)
	ctx := context.Background()
	var logs syncBuffer
	svc.log = slog.New(slog.NewTextHandler(&logs, nil))
	ev := chatEvents(`printf '{"id":"a","kind":"nope"}\n{"id":"b","kind":"message","channel":{"id":"c1"},"thread_id":"t1"}\n'; sleep 30`)
	ev.Types = []manifest.EventType{{Type: "message"}}
	if _, err := svc.Create(ctx, linesEntry("chat", ev)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	count := func(q string) int {
		var n int
		if err := st.DB().QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	waitFor(t, func() bool { return count(`SELECT COUNT(*) FROM channel_threads WHERE instance<>''`) == 1 }, "no thread row")
	if n := count(`SELECT COUNT(*) FROM events WHERE source_id='b'`); n != 1 {
		t.Fatalf("known type rows = %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM events WHERE source_id='a'`); n != 0 {
		t.Fatalf("unknown type rows = %d", n)
	}
	if !strings.Contains(logs.String(), "reason=unknown_type type=nope") {
		t.Fatalf("log = %q", logs.String())
	}
	if _, err := svc.InjectEvent(ctx, "chat", map[string]any{"id": "c", "kind": "nope"}); err == nil {
		t.Fatal("inject of an unknown type passed")
	}
}

// TestLinesRoomsTrigger proves a trigger with rooms: fires on a lines event
// of that room only, with the built-in vars.
func TestLinesRoomsTrigger(t *testing.T) {
	st, svc := newInternalService(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	writeAgentFile(t, wsPath, "room")
	routingtest.Write(t, wsPath, "chat", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{Agent: "room", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Rooms: []string{"c1"}}},
	}})
	w, err := svc.ws.Add(ctx, wsPath, "room-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, linesEntry("chat", chatEvents("sleep 30"))); err != nil {
		t.Fatal(err)
	}
	for _, room := range []string{"c2", "c1"} {
		if _, err := svc.InjectEvent(ctx, "chat", map[string]any{
			"id": "m-" + room, "text": "hello", "channel": map[string]any{"id": room}, "user": map[string]any{"id": "u1"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.DB().Query(`SELECT COALESCE(vars_json,''), conversation_key FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='room')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []map[string]string
	var keys []string
	for rows.Next() {
		var raw, key string
		if err := rows.Scan(&raw, &key); err != nil {
			t.Fatal(err)
		}
		var v map[string]string
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["room"] != "c1" || got[0]["text"] != "hello" || got[0]["sender_id"] != "u1" || keys[0] != "c1" {
		t.Fatalf("queue = %v keys = %v", got, keys)
	}
}

// chatEvents is a lines events block that maps every message field.
func chatEvents(command string) manifest.Events {
	return manifest.Events{
		Command: command, Format: manifest.FormatJSON, Restart: manifest.RestartNever,
		ID: "id", Room: "channel.id", RoomName: "channel.name", Thread: "thread_id",
		Sender: "user.id", SenderName: "user.name", Type: "kind", Text: "text",
	}
}

func mustEventID(t *testing.T, st interface{ DB() *sql.DB }, sourceID string) string {
	t.Helper()
	var id string
	if err := st.DB().QueryRow(`SELECT id FROM events WHERE source_id=?`, sourceID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// syncBuffer is a log sink that the supervisor goroutine and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestConversationKeyThreadless(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		rec       eventRecord
		continues bool
		want      string
	}{
		{"threaded group message", eventRecord{room: "r1", root: "e1", sourceID: "e1"}, true, "r1/e1"},
		{"threadless group message", eventRecord{room: "r1", root: "e1", sourceID: "e1", threadless: true}, true, "r1"},
		{"threadless quote-reply", eventRecord{room: "r1", root: "e1", sourceID: "e2", replyTo: "e1", threadless: true}, true, "r1"},
		{"threadless DM", eventRecord{room: "r1", root: "r1", sourceID: "e3", threadless: true}, true, "r1"},
		{"threadless thread", eventRecord{room: "r1", root: "t1", thread: "t1", sourceID: "e4", threadless: true}, true, "r1/t1"},
		{"threadless without continues", eventRecord{room: "r1", root: "e1", sourceID: "e1", threadless: true}, false, "r1"},
	}
	for _, c := range cases {
		if got := c.rec.conversationKey(c.continues); got != c.want {
			t.Errorf("%s: key = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestReplyClaimSkipsOwnerTrigger: a room that a reply job claims goes to
// the job, and the owner agent records no_match with the reason. Other rooms
// and other agents are not touched. A turned-off reply trigger claims nothing.
func TestReplyClaimSkipsOwnerTrigger(t *testing.T) {
	svc, wsID := replyClaimFixture(t)
	ctx := context.Background()
	cases := []struct {
		room        string
		wantQueued  []string
		wantSkipped bool
	}{
		{"r1", []string{"ellis/italy", "other"}, true},
		{"r2", []string{"ellis"}, false},
		{"r3", []string{"ellis"}, false},
	}
	for _, tc := range cases {
		if _, err := svc.InjectEvent(ctx, "chat", map[string]any{
			"id": "m-" + tc.room, "text": "hi", "channel": map[string]any{"id": tc.room}, "user": map[string]any{"id": "u1"},
		}); err != nil {
			t.Fatal(err)
		}
		evID := mustEventID(t, svc.st, "m-"+tc.room)
		rows, err := svc.st.DB().Query(`SELECT j.name, o.outcome, COALESCE(o.error_message,'') FROM event_outcomes o
			JOIN job_index j ON j.id = o.job_id WHERE o.event_id=? ORDER BY j.name`, evID)
		if err != nil {
			t.Fatal(err)
		}
		var queued []string
		skipped := false
		for rows.Next() {
			var name, outcome, msg string
			if err := rows.Scan(&name, &outcome, &msg); err != nil {
				t.Fatal(err)
			}
			switch {
			case outcome == "queued":
				queued = append(queued, name)
			case name == "ellis" && outcome == "no_match" && msg == "room is handled by job ellis/italy":
				skipped = true
			}
		}
		_ = rows.Close()
		if strings.Join(queued, ",") != strings.Join(tc.wantQueued, ",") || skipped != tc.wantSkipped {
			t.Fatalf("room %s: queued %v skipped %v", tc.room, queued, skipped)
		}
	}
	if rj, ok := svc.ReplyJob(ctx, wsID, "chat", "r1"); !ok || rj.Name != "ellis/italy" {
		t.Fatalf("ReplyJob r1 = %+v %v", rj, ok)
	}
	if _, ok := svc.ReplyJob(ctx, wsID, "chat", "r3"); ok {
		t.Fatal("a turned-off trigger must not claim r3")
	}
}

// TestReplyRoomIssues: a second job on a claimed room gets reply_room_taken
// on the job. The claimant itself and another room get none.
func TestReplyRoomIssues(t *testing.T) {
	svc, wsID := replyClaimFixture(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	write := func(rooms ...string) {
		routingtest.Write(t, wsPath, "chat", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
			{Job: "ellis/italy", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Rooms: []string{"r1"}, Chat: job.ChatReply}},
			{Job: "ellis/second", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Rooms: rooms, Chat: job.ChatReply}},
		}})
	}
	write("r1", "r2")
	if err := os.MkdirAll(filepath.Join(wsPath, "agents/ellis/second"), 0o755); err != nil {
		t.Fatal(err)
	}
	rewriteMD(t, "agents/ellis/second/JOB.md", "---\nharness: fake\n---\n\nSecond.\n")
	w, err := svc.ws.Get(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	codes := func(name string) []job.Issue {
		_, issues := svc.JobRouteIssues(ctx, wsID, name)
		var out []job.Issue
		for _, is := range issues {
			if is.Code == job.CodeReplyRoomTaken {
				out = append(out, is)
			}
		}
		return out
	}
	got := codes("ellis/second")
	if len(got) != 1 || !strings.Contains(got[0].Message, "ellis/italy") {
		t.Fatalf("second job issues = %+v", got)
	}
	if got := codes("ellis/italy"); len(got) != 0 {
		t.Fatalf("claimant issues = %+v", got)
	}
	write("r9")
	if got := codes("ellis/second"); len(got) != 0 {
		t.Fatalf("free room issues = %+v", got)
	}
}

// replyClaimFixture is a workspace where agent ellis hears every room, its
// job italy replies in room r1, its job off has a turned-off reply trigger
// on room r3, and agent other hears room r1.
func replyClaimFixture(t *testing.T) (*Service, string) {
	t.Helper()
	_, svc := newInternalService(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	files := map[string]string{
		"agents/ellis/AGENT.md":     "---\nharness: fake\n---\n\nEllis.\n",
		"agents/ellis/italy/JOB.md": "---\nharness: fake\n---\n\nItaly.\n",
		"agents/ellis/off/JOB.md":   "---\nharness: fake\n---\n\nOff.\n",
		"agents/other/AGENT.md":     "---\nharness: fake\n---\n\nOther.\n",
	}
	for rel, md := range files {
		p := filepath.Join(wsPath, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	routingtest.Write(t, wsPath, "chat", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{Agent: "ellis", Gets: routing.GetsEveryMessage},
		{Job: "ellis/italy", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Rooms: []string{"r1"}, Chat: job.ChatReply}},
		{Job: "ellis/off", Gets: routing.GetsOff, Filter: job.TriggerYAML{Rooms: []string{"r3"}, Chat: job.ChatReply}},
		{Agent: "other", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Rooms: []string{"r1"}}},
	}})
	w, err := svc.ws.Add(ctx, wsPath, "claim-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, linesEntry("chat", chatEvents("sleep 30"))); err != nil {
		t.Fatal(err)
	}
	return svc, w.ID
}

// TestTriggerIndexSkipsArchived: an archived job, and each job of an archived
// agent, leave the trigger index.
func TestTriggerIndexSkipsArchived(t *testing.T) {
	svc, wsID := replyClaimFixture(t)
	ctx := context.Background()
	rebuild := func() {
		t.Helper()
		w, err := svc.ws.Get(ctx, wsID)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.ws.RebuildIndex(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(triggerOwners(svc, "chat"), ","); got != "ellis,ellis/italy,ellis/off,other" {
		t.Fatalf("before: %s", got)
	}
	rewriteMD(t, "agents/ellis/italy/JOB.md", "---\nharness: fake\narchived_at: 2026-11-01T10:00:00Z\n---\n\nItaly.\n")
	rebuild()
	if got := strings.Join(triggerOwners(svc, "chat"), ","); got != "ellis,ellis/off,other" {
		t.Fatalf("job archived: %s", got)
	}
	rewriteMD(t, "agents/ellis/AGENT.md", "---\nharness: fake\narchived_at: 2026-11-01T10:00:00Z\n---\n\nEllis.\n")
	rebuild()
	if got := strings.Join(triggerOwners(svc, "chat"), ","); got != "other" {
		t.Fatalf("agent archived: %s", got)
	}
}

// TestReplyJobSkipsArchived: an archived job does not claim its room.
func TestReplyJobSkipsArchived(t *testing.T) {
	svc, wsID := replyClaimFixture(t)
	ctx := context.Background()
	if rj, ok := svc.ReplyJob(ctx, wsID, "chat", "r1"); !ok || rj.Name != "ellis/italy" {
		t.Fatalf("before: %+v %v", rj, ok)
	}
	rewriteMD(t, "agents/ellis/italy/JOB.md", "---\nharness: fake\narchived_at: 2026-11-01T10:00:00Z\n---\n\nItaly.\n")
	w, err := svc.ws.Get(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	if rj, ok := svc.ReplyJob(ctx, wsID, "chat", "r1"); ok {
		t.Fatalf("archived job claims r1: %+v", rj)
	}
}

func rewriteMD(t *testing.T, rel, md string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home.Base(), "ws", rel), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func triggerOwners(svc *Service, event string) []string {
	var names []string
	for _, ct := range svc.triggerIndex(context.Background())[event] {
		names = append(names, ct.row.Name)
	}
	sort.Strings(names)
	return names
}

// TestTriggerIndexClaimsFileOrder: two jobs reply in one room. The first
// route in the file claims it, and a swap of the routes moves the claim.
func TestTriggerIndexClaimsFileOrder(t *testing.T) {
	svc, wsID := replyClaimFixture(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	if err := os.MkdirAll(filepath.Join(wsPath, "agents/ellis/second"), 0o755); err != nil {
		t.Fatal(err)
	}
	rewriteMD(t, "agents/ellis/second/JOB.md", "---\nharness: fake\n---\n\nSecond.\n")
	w, err := svc.ws.Get(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	reply := job.TriggerYAML{Rooms: []string{"r1"}, Chat: job.ChatReply}
	italy := routing.Route{Job: "ellis/italy", Gets: routing.GetsEveryMessage, Filter: reply}
	second := routing.Route{Job: "ellis/second", Gets: routing.GetsEveryMessage, Filter: reply}
	for _, tc := range []struct {
		routes []routing.Route
		want   string
	}{
		{[]routing.Route{italy, second}, "ellis/italy"},
		{[]routing.Route{second, italy}, "ellis/second"},
	} {
		routingtest.Write(t, wsPath, "chat", routing.File{Routes: tc.routes})
		if rj, ok := svc.ReplyJob(ctx, wsID, "chat", "r1"); !ok || rj.Name != tc.want {
			t.Fatalf("claim = %+v %v, want %s", rj, ok, tc.want)
		}
	}
}

// TestTriggerFingerprintRoutes: a change of a routes file rebuilds the index.
func TestTriggerFingerprintRoutes(t *testing.T) {
	svc, wsID := replyClaimFixture(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	before := svc.triggerIndex(ctx)
	if got := len(before["chat"]); got != 4 {
		t.Fatalf("routes = %d, want 4", got)
	}
	routingtest.Write(t, wsPath, "chat", routing.File{Routes: []routing.Route{{Agent: "ellis", Gets: routing.GetsEveryMessage}}})
	if got := len(svc.triggerIndex(ctx)["chat"]); got != 1 {
		t.Fatalf("routes after the edit = %d, want 1 (workspace %s)", got, wsID)
	}
}

// writeAgentFile writes agents/<name>/AGENT.md with no triggers.
func writeAgentFile(t *testing.T, wsPath, name string) {
	t.Helper()
	dir := filepath.Join(wsPath, "agents", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("---\nharness: fake\n---\n\nRun.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
