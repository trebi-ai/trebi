package connectors

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/home"
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
	jobDir := filepath.Join(wsPath, "room-job")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\ntriggers:\n  - event: chat\n    rooms: [c1]\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
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
	rows, err := st.DB().Query(`SELECT COALESCE(vars_json,''), conversation_key FROM event_queue WHERE job_id=(SELECT id FROM job_index WHERE name='room-job')`)
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
