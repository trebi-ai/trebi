package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
)

func TestEventFilterKeeps(t *testing.T) {
	t.Parallel()
	declared := []string{"a", "b"}
	some := &EventFilter{Mode: ReceiveSome, Types: []string{"a"}}
	cases := []struct {
		name string
		f    *EventFilter
		typ  string
		want bool
	}{
		{"nil keeps all", nil, "x", true},
		{"all", &EventFilter{Mode: ReceiveAll}, "b", true},
		{"none", &EventFilter{Mode: ReceiveNone}, "a", false},
		{"some kept", some, "a", true},
		{"some skipped", some, "b", false},
		{"some other off", some, "x", false},
		{"some empty type off", some, "", false},
		{"some other on", &EventFilter{Mode: ReceiveSome, Other: true}, "x", true},
		{"other does not keep a declared type", &EventFilter{Mode: ReceiveSome, Other: true}, "b", false},
	}
	for _, tc := range cases {
		if got := tc.f.Keeps(tc.typ, declared); got != tc.want {
			t.Errorf("%s: Keeps(%q) = %v", tc.name, tc.typ, got)
		}
	}
}

func TestEventFilterValidate(t *testing.T) {
	t.Parallel()
	declared := []string{"a", "b"}
	cases := []struct {
		f     EventFilter
		field string
	}{
		{EventFilter{Mode: ReceiveSome, Types: []string{"a"}, Other: true}, ""},
		{EventFilter{Mode: ReceiveSome}, ""},
		{EventFilter{Mode: ReceiveNone}, ""},
		{EventFilter{Mode: ReceiveSome, Types: []string{"nope"}}, "receive.types"},
		{EventFilter{Mode: ReceiveSome, Types: []string{"a", "a"}}, "receive.types"},
		{EventFilter{Mode: ReceiveAll, Types: []string{"a"}}, "receive.types"},
		{EventFilter{Mode: ReceiveNone, Other: true}, "receive.types"},
		{EventFilter{Mode: "most"}, "receive.mode"},
	}
	for _, tc := range cases {
		errs := tc.f.Validate(declared)
		if tc.field == "" && len(errs) > 0 || tc.field != "" && (len(errs) == 0 || errs[0].Field != tc.field) {
			t.Errorf("%+v: errs = %v, want field %q", tc.f, errs, tc.field)
		}
	}
	if errs := (EventFilter{Mode: ReceiveSome, Types: []string{"nope"}}).Validate(declared); !strings.Contains(errs[0].Message, `unknown event type "nope"`) {
		t.Errorf("message = %q", errs[0].Message)
	}
}

func TestCheckReceiveNoEvents(t *testing.T) {
	t.Parallel()
	c := Connection{Slug: "x", Receive: &EventFilter{Mode: ReceiveNone}}
	var verr *ValidationError
	if err := c.checkReceive(nil, manifest.Manifest{}); !errors.As(err, &verr) {
		t.Fatalf("err = %v", err)
	}
	c.Receive = &EventFilter{Mode: ReceiveAll}
	if err := c.checkReceive(nil, manifest.Manifest{}); err != nil || c.Receive != nil {
		t.Fatalf("all: err = %v, receive = %+v", err, c.Receive)
	}
}

func TestNamedTypesAndSkippedBy(t *testing.T) {
	t.Parallel()
	declared := []string{"screen.stable", "screen.changed"}
	if got := namedTypes(`event.type === 'screen.stable' || event.type == "screen.changed"`, declared); !slices.Equal(got, declared) {
		t.Fatalf("named = %v", got)
	}
	if got := namedTypes(`event.type.startsWith('screen')`, declared); got != nil {
		t.Fatalf("named = %v", got)
	}
	some := &EventFilter{Mode: ReceiveSome, Types: []string{"screen.stable"}}
	cases := []struct {
		f     *EventFilter
		match string
		want  []string
	}{
		{nil, "", nil},
		{some, `event.type === 'screen.changed'`, []string{"screen.changed"}},
		{some, `event.type === 'screen.stable'`, nil},
		{some, "", nil},
		{&EventFilter{Mode: ReceiveSome}, "", []string{""}},
		{&EventFilter{Mode: ReceiveNone}, "", []string{""}},
		{&EventFilter{Mode: ReceiveNone}, `event.type === 'screen.stable'`, []string{"screen.stable"}},
	}
	for _, tc := range cases {
		if got := skippedBy(tc.f, tc.match, declared); !slices.Equal(got, tc.want) {
			t.Errorf("skippedBy(%+v, %q) = %q, want %q", tc.f, tc.match, got, tc.want)
		}
	}
}

// TestReceivePipeline proves that a skipped type is not stored, starts no
// job, moves the cursor, and is counted; and that a change back to all keeps
// the process and does not replay the skipped event.
func TestReceivePipeline(t *testing.T) {
	st, svc := newInternalService(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	writeAgentFile(t, wsPath, "chat-job")
	routingtest.Write(t, wsPath, "chat", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{Agent: "chat-job", Gets: routing.GetsEveryMessage},
	}})
	w, err := svc.ws.Add(ctx, wsPath, "chat-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home.Base(), "events.jsonl")
	c := typedChat(t, file)
	c.Receive = &EventFilter{Mode: ReceiveSome, Types: []string{"message"}}
	created, err := svc.Create(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := st.DB().QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	cursor := func() string {
		pos, _ := NewFolder(created.ID).Cursor()
		return pos.LastEventID
	}
	appendLines(t, file, `{"id":"m1","kind":"message","channel":{"id":"c1"}}`, `{"id":"t1","kind":"typing","channel":{"id":"c1"}}`)
	waitFor(t, func() bool { return cursor() == "t1" }, "the cursor did not move past the skipped event")
	if n := count(`SELECT COUNT(*) FROM events WHERE source_id='m1'`); n != 1 {
		t.Fatalf("kept rows = %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM events WHERE source_id='t1'`); n != 0 {
		t.Fatalf("skipped rows = %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM event_queue`); n != 1 {
		t.Fatalf("queue rows = %d, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM channel_threads WHERE instance=?`, created.ID); n != 1 {
		t.Fatalf("thread rows = %d", n)
	}
	if n := count(`SELECT skipped FROM connection_event_counts WHERE instance=? AND type='typing'`, created.ID); n != 1 {
		t.Fatalf("typing skipped = %d", n)
	}
	if n := count(`SELECT kept FROM connection_event_counts WHERE instance=? AND type='message'`, created.ID); n != 1 {
		t.Fatalf("message kept = %d", n)
	}
	pid := svc.procByName(created.ID).snapshot().pid

	if _, err := svc.Patch(ctx, "chat", ConnectionPatch{Receive: json.RawMessage(`{"mode":"all"}`)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Lookup("chat"); got.Receive != nil {
		t.Fatalf("receive = %+v, want nil for all", got.Receive)
	}
	appendLines(t, file, `{"id":"t2","kind":"typing","channel":{"id":"c1"}}`)
	waitFor(t, func() bool { return count(`SELECT COUNT(*) FROM events WHERE source_id='t2'`) == 1 }, "t2 not stored")
	if n := count(`SELECT COUNT(*) FROM events WHERE source_id='t1'`); n != 0 {
		t.Fatal("the skipped event came back")
	}
	if got := svc.procByName(created.ID).snapshot().pid; got != pid {
		t.Fatalf("pid %d -> %d: the receive change restarted the process", pid, got)
	}
}

// TestReceiveRecord proves the record default, the validation, the row
// fields, and the prune after the manifest drops a type.
func TestReceiveRecord(t *testing.T) {
	_, svc := newInternalService(t)
	ctx := context.Background()
	file := filepath.Join(home.Base(), "events.jsonl")
	created, err := svc.Create(ctx, typedChat(t, file))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(NewFolder(created.ID).Dir(), "connection.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "receive") {
		t.Fatalf("connection.yaml has receive:\n%s", raw)
	}
	if err := svc.Load(); err != nil {
		t.Fatal(err)
	}
	row, err := svc.Get("chat")
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := row["receive"].(EventFilter); f.Mode != ReceiveAll {
		t.Fatalf("receive = %#v", row["receive"])
	}
	counts, _ := row["event_counts"].([]map[string]any)
	if len(counts) != 2 || counts[0]["type"] != "message" || counts[1]["description"] != "Someone types" {
		t.Fatalf("event_counts = %v", counts)
	}

	var verr *ValidationError
	if _, err := svc.Patch(ctx, "chat", ConnectionPatch{Receive: json.RawMessage(`{"mode":"some","types":["nope"]}`)}); !errors.As(err, &verr) {
		t.Fatalf("unknown type: err = %v", err)
	}
	if _, err := svc.Patch(ctx, "chat", ConnectionPatch{Receive: json.RawMessage(`{"mode":"some","types":["message","typing"]}`)}); err != nil {
		t.Fatal(err)
	}
	m := *typedChat(t, file).Manifest
	m.Events.Types = m.Events.Types[:1]
	b, _ := json.Marshal(m)
	if _, err := svc.Patch(ctx, "chat", ConnectionPatch{Manifest: b}); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Lookup("chat")
	if got.Receive == nil || !slices.Equal(got.Receive.Types, []string{"message"}) {
		t.Fatalf("receive after the type went = %+v", got.Receive)
	}
}

// TestInjectEventSkipped proves that connector test refuses a skipped type.
func TestInjectEventSkipped(t *testing.T) {
	_, svc := newInternalService(t)
	ctx := context.Background()
	c := typedChat(t, filepath.Join(home.Base(), "events.jsonl"))
	c.Receive = &EventFilter{Mode: ReceiveSome, Types: []string{"message"}}
	if _, err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	var verr *ValidationError
	_, err := svc.InjectEvent(ctx, "chat", map[string]any{"id": "t1", "kind": "typing"})
	if !errors.As(err, &verr) || !strings.Contains(err.Error(), "chat skips typing. Receive it first.") {
		t.Fatalf("err = %v", err)
	}
	if _, err := svc.InjectEvent(ctx, "chat", map[string]any{"id": "m1", "kind": "message"}); err != nil {
		t.Fatal(err)
	}
}

// TestEventCounts proves the hour buckets, the share of the last hour, the
// rows of declared, other, and silent types, and the seven-day prune.
func TestEventCounts(t *testing.T) {
	st, svc := newInternalService(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 24, 13, 30, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	add := func(at time.Time, typ string, skipped bool) {
		t.Helper()
		if err := countType(ctx, st.DB(), "con_x", typ, at.Format(time.RFC3339Nano), skipped); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		add(now.Add(-5*time.Minute), "message", false)
	}
	for range 4 {
		add(now.Add(-time.Hour), "message", true) // 12:30, half of the 12:00 bucket counts
	}
	add(now.Add(-10*time.Minute), "surprise", false)
	add(now.Add(-8*24*time.Hour), "message", false)

	counts := svc.typeCounts(now)["con_x"]
	if c := counts["message"]; c.kept != 3 || c.skipped != 2 {
		t.Fatalf("message = %+v", c)
	}
	m := manifest.Manifest{Events: &manifest.Events{Types: []manifest.EventType{{Type: "message"}, {Type: "typing"}}}}
	rows := eventCountsRow(m, counts)
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = fmt.Sprintf("%s:%v:%d:%d", r["type"], r["declared"], r["kept"], r["skipped"])
	}
	if want := []string{"message:true:3:2", "typing:true:0:0", "surprise:false:1:0"}; !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	n, err := svc.PruneEventCounts(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
}

// TestDoctorEventSkipped proves the finding for a job that starts on a type
// that its connection skips.
func TestDoctorEventSkipped(t *testing.T) {
	_, svc := newInternalService(t)
	ctx := context.Background()
	wsPath := filepath.Join(home.Base(), "ws")
	writeAgentFile(t, wsPath, "typing-job")
	routingtest.Write(t, wsPath, "chat", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{Agent: "typing-job", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Match: "event.kind === 'typing'"}},
	}})
	w, err := svc.ws.Add(ctx, wsPath, "doc-ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ws.RebuildIndex(ctx, w); err != nil {
		t.Fatal(err)
	}
	c := typedChat(t, filepath.Join(home.Base(), "events.jsonl"))
	c.Receive = &EventFilter{Mode: ReceiveSome, Types: []string{"message"}}
	if _, err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, f := range svc.Doctor(ctx) {
		if f.Code == "connector_event_skipped" {
			found = append(found, f.Message)
		}
	}
	if len(found) != 1 || found[0] != "agents/typing-job starts on typing from chat, and chat skips it." {
		t.Fatalf("findings = %q", found)
	}
	row, err := svc.Get("chat")
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := row["event_jobs"].([]map[string]any)
	if len(jobs) != 1 || jobs[0]["job"] != "agents/typing-job" || jobs[0]["type"] != "typing" || jobs[0]["workspace"] != w.ID {
		t.Fatalf("event_jobs = %v", jobs)
	}
}

// typedChat is a lines connection that declares the types message and
// typing, and reads its events from a file.
func typedChat(t *testing.T, file string) Connection {
	t.Helper()
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ev := chatEvents("tail -n +1 -f " + file)
	ev.Types = []manifest.EventType{{Type: "message", Description: "A message"}, {Type: "typing", Description: "Someone types"}}
	return linesEntry("chat", ev)
}

func appendLines(t *testing.T, file string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}
