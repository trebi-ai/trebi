package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/waittest"
)

func TestRulesTestStages(t *testing.T) {
	_, base, dir := startAPI(t)
	seedGateEvents(t, dir)
	cases := []struct {
		name  string
		body  map[string]any
		match bool
		stage string
	}{
		{"event_id match", map[string]any{"event_id": "e1", "match": "event.data.text === 'hi 0'"}, true, ""},
		{"room miss", map[string]any{"event_id": "e1", "match": "true", "rooms": []string{"r2"}}, false, "room"},
		{"sender miss", map[string]any{"event_id": "e1", "match": "true", "allowlist": []string{"u2"}}, false, "sender"},
		{"extract error", map[string]any{"event_id": "e1", "match": "true", "extract": "nope("}, false, "extract"},
		{"match false", map[string]any{"event_id": "e1", "match": "false"}, false, "match"},
		{"draft protocol shape", map[string]any{"match": "true", "rooms": []string{"r9"},
			"event": map[string]any{"type": "message", "room": map[string]any{"id": "r9"}, "sender": map[string]any{"id": "u9"}}}, true, ""},
		{"draft plain fails room", map[string]any{"match": "true", "rooms": []string{"r9"}, "event": map[string]any{"x": 1}}, false, "room"},
		{"no match takes all", map[string]any{"event": draft("deploy now", false)}, true, ""},
		{"contains hit", map[string]any{"contains": []string{"Deploy"}, "event": draft("deploy now", false)}, true, ""},
		{"contains miss", map[string]any{"contains": []string{"rollback"}, "event": draft("deploy now", false)}, false, "contains"},
		{"bot by default", map[string]any{"event": draft("deploy now", true)}, true, ""},
		{"people only", map[string]any{"from": "people", "event": draft("deploy now", true)}, false, "from"},
		{"bots only", map[string]any{"from": "bots", "event": draft("deploy now", false)}, false, "from"},
		{"anyone", map[string]any{"from": "anyone", "event": draft("deploy now", true)}, true, ""},
	}
	for _, c := range cases {
		code, out, _ := doJSON(t, "POST", base+"/v1/rules/test", c.body)
		if code != 200 || out["matched"] != c.match || out["stage"] != c.stage {
			t.Errorf("%s: %d %v", c.name, code, out)
		}
	}
	code, out, _ := doJSON(t, "POST", base+"/v1/rules/test", map[string]any{
		"event_id": "e1", "match": "event.who === 'Ana'", "extract": "({who: event.sender.name})",
	})
	ex, _ := out["extracted"].(map[string]any)
	if code != 200 || out["room"] != "r1" || out["identity"] != "u1" || ex["who"] != "Ana" || out["matched"] != true {
		t.Fatalf("extracted: %d %v", code, out)
	}
	code, out, _ = doJSON(t, "POST", base+"/v1/rules/test", map[string]any{"replies": "continue", "event": draft("deploy now", false)})
	vars, _ := out["vars"].(map[string]any)
	if code != 200 || out["conversation_key"] != "r9/m9" || out["follow_up"] != false || vars["text"] != "deploy now" {
		t.Fatalf("conversation key: %d %v", code, out)
	}
	if code, out, _ := doJSON(t, "POST", base+"/v1/rules/test", map[string]any{"batch": "15m", "event": draft("deploy now", false)}); code != 200 || out["batch"] != "15m" {
		t.Fatalf("batch: %d %v", code, out)
	}
	if code, out, _ := doJSON(t, "POST", base+"/v1/rules/test", map[string]any{"batch": "30s", "event": draft("deploy now", false)}); code != 400 || errCode(out) != "validation" {
		t.Fatalf("short batch: %d %v", code, out)
	}
	if code, out, _ := doJSON(t, "POST", base+"/v1/rules/test", map[string]any{"event_id": "nope", "match": "true"}); code != 404 {
		t.Fatalf("missing event: %d %v", code, out)
	}
	if code, out, _ := doJSON(t, "POST", base+"/v1/rules/test", map[string]any{"event_id": "e1", "event": map[string]any{}, "match": "true"}); code != 400 {
		t.Fatalf("both event and event_id: %d %v", code, out)
	}
}

func TestRulesTestBatch(t *testing.T) {
	_, base, dir := startAPI(t)
	seedGateEvents(t, dir)
	code, out, _ := doJSON(t, "POST", base+"/v1/rules/test-batch", map[string]any{
		"match": "true", "rooms": []string{"r1"}, "event_ids": []string{"e1", "e2", "gone"},
	})
	if code != 200 {
		t.Fatalf("batch: %d %v", code, out)
	}
	rows, _ := out["results"].([]any)
	if len(rows) != 3 {
		t.Fatalf("rows = %v", rows)
	}
	want := []struct {
		id      string
		matched bool
		stage   string
		err     string
	}{{"e1", true, "", ""}, {"e2", false, "room", ""}, {"gone", false, "", "event not found"}}
	for i, w := range want {
		r, _ := rows[i].(map[string]any)
		if r["event_id"] != w.id || r["matched"] != w.matched || r["stage"] != w.stage || r["error"] != w.err {
			t.Errorf("row %d = %v", i, r)
		}
	}
	ids := make([]string, 51)
	for i := range ids {
		ids[i] = fmt.Sprintf("e%d", i)
	}
	if code, out, _ := doJSON(t, "POST", base+"/v1/rules/test-batch", map[string]any{"match": "true", "event_ids": ids}); code != 400 || errCode(out) != "validation" {
		t.Fatalf("cap: %d %v", code, out)
	}
	if code, _, _ := doJSON(t, "POST", base+"/v1/rules/test-batch", map[string]any{"match": "true"}); code != 400 {
		t.Fatalf("empty: %d", code)
	}
}

// TestConnectorSenders proves the senders route groups stored events per
// identity, newest first, and filters on the name.
func TestConnectorSenders(t *testing.T) {
	_, base, dir := startAPI(t)
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	now := time.Now().UTC()
	for i, ev := range []struct{ identity, name string }{
		{"ana@x.io", "Ana"}, {"bo@x.io", "Bo"}, {"ana@x.io", "Ana"}, {"", ""},
	} {
		id := fmt.Sprintf("m%d", i)
		at := now.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		if _, err := st.DB().Exec(`INSERT INTO events (id, connector, instance, received_at, trace_id, identity, sender)
			VALUES (?,?,?,?,?,NULLIF(?,''),NULLIF(?,''))`, id, "email", "email", at, id, ev.identity, ev.name); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	code, out, _ := doJSON(t, "GET", base+"/v1/connectors/email/senders", nil)
	rows, _ := out["senders"].([]any)
	if code != 200 || out["id"] != "email" || len(rows) != 2 {
		t.Fatalf("senders: %d %v", code, out)
	}
	first, _ := rows[0].(map[string]any)
	if first["identity"] != "ana@x.io" || first["name"] != "Ana" || first["count"] != float64(2) || first["last_at"] == "" {
		t.Fatalf("first = %v", first)
	}
	_, out, _ = doJSON(t, "GET", base+"/v1/connectors/email/senders?q=Bo", nil)
	if rows, _ := out["senders"].([]any); len(rows) != 1 {
		t.Fatalf("filtered = %v", out)
	}
	if code, _, _ := doJSON(t, "GET", base+"/v1/connectors/nope/senders", nil); code != 404 {
		t.Fatalf("unknown connector: %d", code)
	}
}

// TestConnectorDependents proves the dependents route lists the event
// triggers and the room bindings of a connector.
func TestConnectorDependents(t *testing.T) {
	_, base, dir := startAPI(t)
	wsPath := filepath.Join(dir, "deps")
	jobDir := filepath.Join(wsPath, "agents", "triage", "mailer")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	// The room binding below names this agent.
	if err := os.MkdirAll(filepath.Join(wsPath, "agents", "triage"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "agents", "triage", "AGENT.md"), []byte("---\ndescription: Test agent.\nharness: fake\n---\n\nTriage.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	routingtest.Write(t, wsPath, "email", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{{
		ID: "rt_00000000d1", Job: "triage/mailer", Gets: routing.GetsEveryMessage,
		Filter: job.TriggerYAML{Rooms: []string{"inbox"}, Allowlist: []string{"a@x.io"}},
	}}})
	wsID := addWorkspace(t, base, wsPath, "deps")
	if code, out, _ := doJSON(t, "POST", base+"/v1/connectors/email/rooms/inbox/bind", map[string]any{"workspace": wsID, "operator": "agents/triage"}); code != 200 {
		t.Fatalf("bind: %d %v", code, out)
	}
	code, out, _ := doJSON(t, "GET", base+"/v1/connectors/email/dependents", nil)
	if code != 200 || out["id"] != "email" || out["slug"] != "email" {
		t.Fatalf("dependents: %d %v", code, out)
	}
	triggers, _ := out["triggers"].([]any)
	bindings, _ := out["bindings"].([]any)
	if len(triggers) != 1 || len(bindings) != 1 {
		t.Fatalf("dependents = %v", out)
	}
	tr, _ := triggers[0].(map[string]any)
	if tr["kind"] != "job" || tr["workspace"] != wsID || tr["name"] != "agents/triage/mailer" || tr["route_id"] != "rt_00000000d1" ||
		tr["active"] != true || tr["gets"] != "every_message" || tr["senders"] != float64(1) {
		t.Fatalf("trigger = %v", tr)
	}
	if rooms, _ := tr["rooms"].([]any); len(rooms) != 1 || rooms[0] != "inbox" {
		t.Fatalf("rooms = %v", tr["rooms"])
	}
	b, _ := bindings[0].(map[string]any)
	if b["workspace"] != wsID || !strings.HasPrefix(fmt.Sprint(b["operator_id"]), "agt_") || b["operator_name"] != "triage" || b["room"] != "inbox" || b["thread"] != "" || b["conversation_id"] == "" {
		t.Fatalf("binding = %v", b)
	}
	if code, _, _ := doJSON(t, "GET", base+"/v1/connectors/nope/dependents", nil); code != 404 {
		t.Fatalf("unknown connector: %d", code)
	}
}

// seedGateEvents stores two protocol-shaped events: e1 in room r1 from u1,
// e2 in room r2 from u2.
func seedGateEvents(t *testing.T, dir string) {
	t.Helper()
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	defer st.Close()
	now := time.Now().UTC()
	for i, ev := range []struct{ id, room, identity, name string }{
		{"e1", "r1", "u1", "Ana"}, {"e2", "r2", "u2", "Bo"},
	} {
		raw := fmt.Sprintf(`{"id":%q,"type":"message","room":{"id":%q},"sender":{"id":%q,"name":%q},"data":{"text":"hi %d"}}`,
			ev.id, ev.room, ev.identity, ev.name, i)
		at := now.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		if _, err := st.DB().Exec(`INSERT INTO events (id, connector, instance, received_at, raw_json, trace_id, room, identity, sender)
			VALUES (?,?,?,?,?,?,?,?,?)`, ev.id, "whatsapp", "con_wa", at, raw, ev.id, ev.room, ev.identity, ev.name); err != nil {
			t.Fatal(err)
		}
	}
}

// draft is a trebi-connector/1 message event for a rules test.
func draft(text string, bot bool) map[string]any {
	return map[string]any{
		"id": "m9", "type": "message", "ts": "2026-10-01T10:00:00Z", "text": text,
		"room": map[string]any{"id": "r9"}, "sender": map[string]any{"id": "u9", "bot": bot},
	}
}

// TestRulesTestFollowUp proves that a reply in an open conversation of the
// job skips from and contains, and that a bot reply stops at from.
func TestRulesTestFollowUp(t *testing.T) {
	_, base, dir := startAPI(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "agents/ops/watch")
	wsID := addWorkspace(t, base, wsDir, "ops")

	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, ev := range []struct {
		id, text, root string
		bot            bool
	}{{"e1", "build failure", "", true}, {"e2", "why?", "e1", false}, {"e3", "retrying", "e1", true}} {
		raw := fmt.Sprintf(`{"id":%q,"type":"message","text":%q,"room":{"id":"r1"},"sender":{"id":"s","bot":%t}}`, ev.id, ev.text, ev.bot)
		if _, err := st.DB().Exec(`INSERT INTO events (id, connector, instance, received_at, raw_json, trace_id, source_id, room, identity, text, root)
			VALUES (?,?,?,?,?,?,?,?,?,?,NULLIF(?,''))`, ev.id, "discord", "con_dc", now, raw, ev.id, ev.id, "r1", "s", ev.text, ev.root); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().Exec(`INSERT INTO event_queue (id, connector, workspace_id, job_id, kind, conversation_key, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, "q1", "discord", wsID, indexedJobID(t, st, wsID, "agents/ops/watch"), "job", "r1/e1", "pending", now, now); err != nil {
		t.Fatal(err)
	}
	st.Close()

	gate := map[string]any{"from": "bots", "contains": []string{"failure"}, "replies": "continue", "workspace": wsID, "job": "agents/ops/watch"}
	tests := []struct {
		name     string
		edit     func(map[string]any)
		id       string
		matched  bool
		followUp bool
		stage    string
	}{
		{"person reply", func(map[string]any) {}, "e2", true, true, ""},
		{"bot reply", func(map[string]any) {}, "e3", false, true, "from"},
		{"first message", func(map[string]any) {}, "e1", true, false, ""},
		{"no job", func(b map[string]any) { delete(b, "job") }, "e2", false, false, "from"},
	}
	for _, tt := range tests {
		body := map[string]any{"event_id": tt.id}
		for k, v := range gate {
			body[k] = v
		}
		tt.edit(body)
		code, out, _ := doJSON(t, "POST", base+"/v1/rules/test", body)
		if code != 200 {
			t.Fatalf("%s: %d %v", tt.name, code, out)
		}
		fu, _ := out["follow_up"].(bool)
		if out["matched"] != tt.matched || fu != tt.followUp || out["stage"] != tt.stage {
			t.Errorf("%s = %v", tt.name, out)
		}
	}
	if code, out, _ := doJSON(t, "POST", base+"/v1/rules/test", map[string]any{"event_id": "e2", "replies": "continue", "job": "nope"}); code != 404 {
		t.Fatalf("unknown job: %d %v", code, out)
	}
}

func TestEventsAfter(t *testing.T) {
	st, base, wsA, _ := scopeFixture(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	e1 := seedEvent(t, st, "feed", "one", now, 0)
	e2 := seedEvent(t, st, "other", "two", old, 0)
	e3 := seedEvent(t, st, "feed", "three", now, 1)
	e4 := seedEvent(t, st, "other", "four", now, 0)
	e5 := seedEvent(t, st, "feed", "five", now, 0)
	if _, err := st.DB().Exec(`INSERT INTO event_outcomes (event_id, workspace_id, job_id, route_id, outcome, created_at)
		VALUES (?,?,?,?,?,?)`, e3, wsA, "j1", "", "queued", now); err != nil {
		t.Fatal(err)
	}
	zero := "00000000000000000000000000"
	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		{"ascending, cursor row excluded", "after=" + e1, []string{e2, e3, e4, e5}},
		{"from the oldest", "after=" + zero, []string{e1, e2, e3, e4, e5}},
		{"connector", "after=" + e1 + "&connector=feed", []string{e3, e5}},
		{"workspace", "after=" + zero + "&workspace=alpha", []string{e3}},
		{"since", "after=" + zero + "&since=1h", []string{e1, e3, e4, e5}},
		{"catch_up", "after=" + zero + "&catch_up=1", []string{e3}},
		{"q", "after=" + zero + "&q=FOUR", []string{e4}},
		{"page", "after=" + e1 + "&limit=2", []string{e2, e3}},
		{"end", "after=" + e5, nil},
	} {
		got := scopeGet(t, base, "/v1/events?"+tc.query)
		if ids := eventIDs(t, got); !slices.Equal(ids, tc.want) {
			t.Errorf("%s: ids %v, want %v", tc.name, ids, tc.want)
		}
	}
	page := scopeGet(t, base, "/v1/events?after="+e1+"&limit=2")
	if page["next_after"] != e3 {
		t.Fatalf("next_after %v, want %s", page["next_after"], e3)
	}
	if end := scopeGet(t, base, "/v1/events?after="+e5); end["next_after"] != e5 {
		t.Fatalf("empty page next_after %v, want %s", end["next_after"], e5)
	}
	// A row has the GET /v1/events/{id} shape.
	row := scopeArr(t, scopeGet(t, base, "/v1/events?after="+e2+"&limit=1"), "events")[0]
	detail := scopeGet(t, base, "/v1/events/"+e3)
	if !reflect.DeepEqual(row, detail) {
		t.Fatalf("after row %v\n differs from detail %v", row, detail)
	}
	for _, q := range []string{"after=nope", "after=" + e1 + "&cursor=" + e2} {
		if code, out, _ := doJSON(t, "GET", base+"/v1/events?"+q, nil); code != 400 || errCode(out) != "validation" {
			t.Errorf("%s: %d %v", q, code, out)
		}
	}
}

func TestEventsFollow(t *testing.T) {
	st, base, _, _ := scopeFixture(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	start := seedEvent(t, st, "feed", "before", now, 0)

	follow := func(after string) (<-chan string, context.CancelFunc) {
		ctx, cancel := context.WithCancel(t.Context())
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v1/events?follow=1&after="+after, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-ndjson" {
			t.Fatalf("follow: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		ids := make(chan string, 16)
		go func() {
			defer resp.Body.Close()
			defer close(ids)
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				var row map[string]any
				if json.Unmarshal(sc.Bytes(), &row) != nil {
					return
				}
				if id, _ := row["id"].(string); id != "" {
					ids <- id
				}
			}
		}()
		return ids, cancel
	}
	next := func(ids <-chan string) string {
		t.Helper()
		select {
		case id := <-ids:
			return id
		case <-time.After(waittest.D(5 * time.Second)):
			t.Fatal("no event line")
			return ""
		}
	}

	ids, cancel := follow(start)
	var want []string
	for _, s := range []string{"a", "b", "c"} {
		want = append(want, seedEvent(t, st, "feed", s, now, 0))
	}
	var got []string
	for range want {
		got = append(got, next(ids))
	}
	cancel()
	if !slices.Equal(got, want) {
		t.Fatalf("follow ids %v, want %v", got, want)
	}

	// A reconnect from the last id sees only rows written after it.
	ids, cancel = follow(got[len(got)-1])
	defer cancel()
	fresh := seedEvent(t, st, "feed", "d", now, 0)
	if id := next(ids); id != fresh {
		t.Fatalf("reconnect first id %s, want %s (no duplicates)", id, fresh)
	}
}

// seedEvent writes one events row and wakes the bus, as the pipeline does.
func seedEvent(t *testing.T, st *store.Store, connector, summary, at string, catchUp int) string {
	t.Helper()
	id := ulid.Make().String()
	if _, err := st.DB().Exec(`INSERT INTO events (id, connector, instance, received_at, summary, raw_json, trace_id, catch_up)
		VALUES (?,?,?,?,?,?,?,?)`, id, connector, connector, at, summary, `{"s":"`+summary+`"}`, "t", catchUp); err != nil {
		t.Fatal(err)
	}
	st.NotifyBus()
	return id
}

// eventIDs returns the ids of the rows under "events".
func eventIDs(t *testing.T, m map[string]any) []string {
	t.Helper()
	var ids []string
	for _, row := range scopeArr(t, m, "events") {
		ids = append(ids, row["id"].(string))
	}
	return ids
}

// TestRunFromEvent proves the hand run path: the event jobs route, a dry run
// with input sources, the 422 shapes, and the stored outcome of a hand run.
func TestRunFromEvent(t *testing.T) {
	s, base, dir := startAPI(t)
	wsDir := t.TempDir()
	wsID := addWorkspace(t, base, wsDir, "event-run-ws")
	ensureAgent(t, base, wsID, "ops")
	md := "---\nharness: fake\ninputs:\n  body: {type: string, label: Email text, required: true}\n" +
		"  tone: {type: string, default: short}\n" +
		"---\n\nReply to {{body}} in a {{tone}} tone.\n"
	if code, body, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{"workspace": wsID, "name": "agents/ops/reply", "markdown": md}); code != 201 {
		t.Fatalf("create %d %v", code, body)
	}
	for name, md := range map[string]string{
		"agents/ops/other": "---\nharness: fake\n---\n\nDo work.\n",
		"agents/ops/picky": "---\nharness: fake\n---\n\nDo work.\n",
		"agents/ops/typed": "---\nharness: fake\n---\n\nDo work.\n",
	} {
		if code, body, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{"workspace": wsID, "name": name, "markdown": md}); code != 201 {
			t.Fatalf("create %s %d %v", name, code, body)
		}
	}
	routingtest.Write(t, wsDir, "email", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{ID: "rt_00000000a1", Job: "ops/reply", Gets: routing.GetsByHand, Inputs: job.InputMap{{Name: "body", Value: "{{data.text}}"}}},
		{ID: "rt_00000000a2", Job: "ops/picky", Gets: routing.GetsByHand, Filter: job.TriggerYAML{Match: "false"}},
	}})
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	t.Cleanup(func() { _ = st.Close() })
	conn := connectors.New(st, s.Workspaces(), nil)
	ctx := context.Background()
	empty, err := conn.InjectNative(ctx, "email", map[string]any{"subject": "hi"}, connectors.NativeMeta{SourceID: "m0"})
	if err != nil {
		t.Fatal(err)
	}
	evID, err := conn.InjectNative(ctx, "email", map[string]any{"subject": "hi", "text": "Can we meet?"},
		connectors.NativeMeta{SourceID: "m1", Type: "mail.received"})
	if err != nil {
		t.Fatal(err)
	}

	code, jobs, _ := doJSON(t, "GET", base+"/v1/events/"+evID+"/jobs", nil)
	if code != 200 {
		t.Fatalf("event jobs %d %v", code, jobs)
	}
	byJob := map[string]map[string]any{}
	for _, row := range scopeArr(t, jobs, "jobs") {
		byJob[row["job"].(string)] = row
	}
	if r := byJob["agents/ops/reply"]; len(byJob) != 2 || r["manual"] != true || r["matched"] != true || r["ready"] != true {
		t.Fatalf("event jobs = %v", byJob)
	}
	if p := byJob["agents/ops/picky"]; p["matched"] != false || p["stage"] != "match" {
		t.Fatalf("picky = %v", p)
	}

	run := func(extra map[string]any) (int, map[string]any) {
		t.Helper()
		body := map[string]any{"job": "agents/ops/reply", "workspace": wsID, "event": map[string]any{"id": evID}}
		maps.Copy(body, extra)
		code, out, _ := doJSON(t, "POST", base+"/v1/runs", body)
		return code, out
	}
	if code, out := run(map[string]any{"job": "agents/ops/picky"}); code != 422 || errCode(out) != "event_not_accepted" {
		t.Fatalf("match gate: %d %v", code, out)
	}
	if code, out := run(map[string]any{"job": "agents/ops/picky", "force": true, "dry_run": true}); code != 200 {
		t.Fatalf("force skips match: %d %v", code, out)
	}
	code, dry := run(map[string]any{"dry_run": true})
	if code != 200 {
		t.Fatalf("dry run %d %v", code, dry)
	}
	in, _ := dry["inputs"].(map[string]any)
	body, _ := in["body"].(map[string]any)
	tone, _ := in["tone"].(map[string]any)
	if body["value"] != "Can we meet?" || body["source"] != "map" || tone["value"] != "short" || tone["source"] != "default" {
		t.Fatalf("dry run inputs = %v", in)
	}
	if !strings.Contains(fmt.Sprint(dry["prompt_preview"]), "Can we meet? in a short tone.") {
		t.Fatalf("dry run prompt = %v", dry["prompt_preview"])
	}

	code, sim, _ := doJSON(t, "POST", base+"/v1/jobs/simulate", map[string]any{"workspace": wsID, "job": "agents/ops/reply", "preview": true})
	if code != 200 {
		t.Fatalf("simulate %d %v", code, sim)
	}
	sum, _ := sim["summary"].(map[string]any)
	if sum["events"] != float64(2) || sum["matched"] != float64(2) || sum["ready"] != float64(1) || fmt.Sprint(sum["missing"]) != "map[body:1]" {
		t.Fatalf("simulate summary = %v", sum)
	}
	for _, row := range scopeArr(t, sim, "results") {
		ready := row["event_id"] == evID
		if row["ready"] != ready || row["manual"] != true || (ready && !strings.Contains(fmt.Sprint(row["prompt_preview"]), "Can we meet?")) {
			t.Fatalf("simulate row = %v", row)
		}
	}
	code, sim, _ = doJSON(t, "POST", base+"/v1/jobs/simulate", map[string]any{"workspace": wsID, "job": "agents/ops/reply",
		"event_ids": []string{empty}, "inputs": map[string]any{"body": "typed"}})
	if code != 200 || scopeArr(t, sim, "results")[0]["ready"] != true || fmt.Sprint(scopeArr(t, sim, "results")[0]["asks"]) != "[body]" {
		t.Fatalf("simulate with inputs: %d %v", code, sim)
	}
	routingtest.Write(t, wsDir, "email", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{ID: "rt_00000000a1", Job: "ops/reply", Gets: routing.GetsByHand, Inputs: job.InputMap{{Name: "body", Value: "{{data.text}}"}}},
		{ID: "rt_00000000a2", Job: "ops/picky", Gets: routing.GetsByHand, Filter: job.TriggerYAML{Match: "false"}},
		{ID: "rt_00000000a3", Job: "ops/typed", Gets: routing.GetsEveryMessage, Filter: job.TriggerYAML{Types: []string{"mail.nope"}}},
	}})
	draft := "---\ninputs:\n  n: {type: integer}\n---\n\n{{n}}\n"
	code, sim, _ = doJSON(t, "POST", base+"/v1/jobs/simulate", map[string]any{"workspace": wsID, "job": "agents/ops/typed", "content": draft, "event_ids": []string{evID}})
	if code != 200 || !strings.Contains(mustJSONString(t, sim), `"stage":"type"`) {
		t.Fatalf("simulate draft: %d %v", code, sim)
	}
	if code, out, _ := doJSON(t, "POST", base+"/v1/jobs/simulate", map[string]any{"workspace": wsID}); code != 400 {
		t.Fatalf("simulate no job and no content: %d %v", code, out)
	}

	code, rt, _ := doJSON(t, "POST", base+"/v1/rules/test", map[string]any{"event_id": evID, "workspace": wsID, "job": "agents/ops/reply"})
	rtIn, _ := rt["inputs"].(map[string]any)
	if code != 200 || rt["matched"] != true || fmt.Sprint(rtIn["body"]) != "map[source:map value:Can we meet?]" {
		t.Fatalf("rules test fill: %d %v", code, rt)
	}

	code, out, _ := doJSON(t, "POST", base+"/v1/runs", map[string]any{"job": "agents/ops/reply", "workspace": wsID,
		"event": map[string]any{"id": empty}})
	if code != 422 || errCode(out) != "invalid" {
		t.Fatalf("empty input: %d %v", code, out)
	}
	if code, out = run(map[string]any{"job": "agents/ops/other"}); code != 422 || errCode(out) != "event_not_accepted" {
		t.Fatalf("other job: %d %v", code, out)
	}

	code, list, _ := doJSON(t, "GET", base+"/v1/events?outcome=manual", nil)
	if code != 200 || !slices.Contains(eventIDs(t, list), evID) {
		t.Fatalf("outcome=manual before run: %d %v", code, list)
	}
	code, list, _ = doJSON(t, "GET", base+"/v1/events?accepts="+wsID+"/agents/ops/reply", nil)
	if code != 200 || !slices.Contains(eventIDs(t, list), evID) {
		t.Fatalf("accepts: %d %v", code, list)
	}

	code, created := run(map[string]any{"inputs": map[string]any{"tone": "warm"}})
	if code != 201 {
		t.Fatalf("hand run %d %v", code, created)
	}
	trig, _ := created["trigger"].(map[string]any)
	if trig["event_id"] != evID || trig["source"] != "manual" || trig["route_id"] != "rt_00000000a1" {
		t.Fatalf("run trigger = %v", trig)
	}
	if inputs, _ := created["inputs"].(map[string]any); inputs["tone"] != "warm" || inputs["body"] != "Can we meet?" {
		t.Fatalf("run inputs = %v", created["inputs"])
	}
	if !strings.Contains(fmt.Sprint(created["prompt_preview"]), "in a warm tone.") {
		t.Fatalf("run prompt = %v", created["prompt_preview"])
	}

	code, list, _ = doJSON(t, "GET", base+"/v1/events?outcome=manual", nil)
	if code != 200 || slices.Contains(eventIDs(t, list), evID) {
		t.Fatalf("outcome=manual after run: %d %v", code, list)
	}
	code, ev, _ := doJSON(t, "GET", base+"/v1/events/"+evID, nil)
	if code != 200 || !strings.Contains(mustJSONString(t, ev), `"by":"manual"`) {
		t.Fatalf("event after run: %d %v", code, ev)
	}
}

// TestRulesTestThreadless proves the rules test gives the room key on a
// channel without threads, for a stored event and for a draft.
func TestRulesTestThreadless(t *testing.T) {
	_, base, dir := startAPI(t)
	def := inlineEntry("chat", map[string]any{
		"events":  map[string]any{"command": "sleep 30", "protocol": "trebi-connector/1", "types": []map[string]any{{"type": "message"}}},
		"channel": map[string]any{},
	})
	code, conn, _ := doJSON(t, "POST", base+"/v1/connectors", def)
	id, _ := conn["id"].(string)
	if code != 201 || id == "" {
		t.Fatalf("create: %d %v", code, conn)
	}
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	raw := `{"id":"g1","type":"message","room":{"id":"g","kind":"group"},"sender":{"id":"u1"},"text":"deploy"}`
	if _, err := st.DB().Exec(`INSERT INTO events (id, connector, instance, received_at, raw_json, trace_id, room, identity, source_id, root)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, "ev_g1", "chat", id, time.Now().UTC().Format(time.RFC3339Nano), raw, "ev_g1", "g", "u1", "g1", "g1"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	code, out, _ := doJSON(t, "POST", base+"/v1/rules/test", map[string]any{"event_id": "ev_g1", "replies": "continue"})
	if code != 200 || out["conversation_key"] != "g" || out["follow_up"] != false {
		t.Fatalf("stored: %d %v", code, out)
	}
	code, out, _ = doJSON(t, "POST", base+"/v1/rules/test", map[string]any{"connector": "chat", "replies": "continue", "event": draft("deploy now", false)})
	if code != 200 || out["conversation_key"] != "r9" {
		t.Fatalf("draft: %d %v", code, out)
	}
}

// TestRunWithEventRoute proves that route_id of the event of a run picks the
// route, that the first by-hand route is the default, and that a route that
// gets nothing or does not exist is refused.
func TestRunWithEventRoute(t *testing.T) {
	base, wsID, evID := routeEventFixture(t, twoHandRoutes())
	run := func(routeID string, dry bool) (int, map[string]any) {
		t.Helper()
		ev := map[string]any{"id": evID}
		if routeID != "" {
			ev["route_id"] = routeID
		}
		code, out, _ := doJSON(t, "POST", base+"/v1/runs", map[string]any{"job": "agents/ops/reply", "workspace": wsID, "event": ev, "dry_run": dry})
		return code, out
	}
	bodyOf := func(out map[string]any) any {
		in, _ := out["inputs"].(map[string]any)
		if b, ok := in["body"].(map[string]any); ok { // a dry run gives the fill detail
			return b["value"]
		}
		return in["body"]
	}
	for _, tc := range []struct{ route, want string }{
		{"", "Can we meet?"}, {"rt_00000000a1", "Can we meet?"}, {"rt_00000000a2", "alt Can we meet?"},
	} {
		if code, out := run(tc.route, true); code != 200 || bodyOf(out) != tc.want {
			t.Fatalf("dry run route %q: %d %v, want body %q", tc.route, code, out, tc.want)
		}
	}
	for _, route := range []string{"rt_00000000a3", "rt_00000000ff"} {
		if code, out := run(route, true); code != 422 || errCode(out) != "event_not_accepted" {
			t.Fatalf("route %s: %d %v", route, code, out)
		}
	}
	code, out := run("rt_00000000a2", false)
	trig, _ := out["trigger"].(map[string]any)
	if code != 201 || trig["route_id"] != "rt_00000000a2" || trig["event_id"] != evID || bodyOf(out) != "alt Can we meet?" {
		t.Fatalf("run: %d %v", code, out)
	}
}

// TestEventOutcomesRoute proves that each outcome of an event carries the id
// of its route, so two routes of one job give two outcomes.
func TestEventOutcomesRoute(t *testing.T) {
	base, _, evID := routeEventFixture(t, twoHandRoutes())
	code, ev, _ := doJSON(t, "GET", base+"/v1/events/"+evID, nil)
	if code != 200 {
		t.Fatalf("event: %d %v", code, ev)
	}
	var got []string
	for _, o := range scopeArr(t, ev, "outcomes") {
		if o["outcome"] != "manual" || o["job_name"] != "agents/ops/reply" {
			t.Fatalf("outcome = %v", o)
		}
		got = append(got, fmt.Sprint(o["route_id"]))
	}
	if want := []string{"rt_00000000a1", "rt_00000000a2"}; !slices.Equal(got, want) {
		t.Fatalf("outcome routes = %v, want %v", got, want)
	}
	code, list, _ := doJSON(t, "GET", base+"/v1/events?outcome=manual", nil)
	if code != 200 || !slices.Contains(eventIDs(t, list), evID) {
		t.Fatalf("list: %d %v", code, list)
	}
	if got := listedRoutes(t, list, evID); !slices.Equal(got, []string{"rt_00000000a1", "rt_00000000a2"}) {
		t.Fatalf("listed routes = %v", got)
	}
	code, jobs, _ := doJSON(t, "GET", base+"/v1/events/"+evID+"/jobs", nil)
	if code != 200 {
		t.Fatalf("event jobs: %d %v", code, jobs)
	}
	for _, row := range scopeArr(t, jobs, "jobs") {
		if row["outcome"] != "manual" || (row["route_id"] != "rt_00000000a1" && row["route_id"] != "rt_00000000a2") {
			t.Fatalf("event job = %v", row)
		}
	}
}

// TestRecordHandRun proves that a run that a person starts points the
// outcome of its own route at the queue row, and leaves the other route
// alone. The outcome stays manual.
func TestRecordHandRun(t *testing.T) {
	base, wsID, evID := routeEventFixture(t, twoHandRoutes())
	code, out, _ := doJSON(t, "POST", base+"/v1/runs", map[string]any{"job": "agents/ops/reply", "workspace": wsID,
		"event": map[string]any{"id": evID, "route_id": "rt_00000000a2"}})
	if code != 201 {
		t.Fatalf("run: %d %v", code, out)
	}
	_, ev, _ := doJSON(t, "GET", base+"/v1/events/"+evID, nil)
	byRoute := map[string]map[string]any{}
	for _, o := range scopeArr(t, ev, "outcomes") {
		byRoute[fmt.Sprint(o["route_id"])] = o
	}
	a1, a2 := byRoute["rt_00000000a1"], byRoute["rt_00000000a2"]
	if len(byRoute) != 2 || a1["queue_id"] != nil || a1["outcome"] != "manual" {
		t.Fatalf("other route changed: %v", byRoute)
	}
	if a2["queue_id"] == nil || a2["queue_id"] == "" || a2["by"] != "manual" || a2["run_id"] != out["id"] {
		t.Fatalf("route a2 outcome = %v, run = %v", a2, out["id"])
	}
	// Only the route that has no run is still waiting for a person.
	_, list, _ := doJSON(t, "GET", base+"/v1/events?outcome=manual", nil)
	if got := listedRoutes(t, list, evID); !slices.Equal(got, []string{"rt_00000000a1"}) {
		t.Fatalf("manual routes after run = %v", got)
	}
}

// TestSimulateRoutes proves that simulate runs the routes of a job: the
// route that gets nothing is not simulated, route_id picks one route, and a
// draft with job set uses the routes of that job.
func TestSimulateRoutes(t *testing.T) {
	base, wsID, evID := routeEventFixture(t, twoHandRoutes())
	sim := func(extra map[string]any) map[string]any {
		t.Helper()
		body := map[string]any{"workspace": wsID, "job": "agents/ops/reply", "preview": true}
		maps.Copy(body, extra)
		code, out, _ := doJSON(t, "POST", base+"/v1/jobs/simulate", body)
		if code != 200 {
			t.Fatalf("simulate %v: %d %v", extra, code, out)
		}
		return out
	}
	row := func(out map[string]any) map[string]any {
		t.Helper()
		rows := scopeArr(t, out, "results")
		if len(rows) != 1 || rows[0]["event_id"] != evID {
			t.Fatalf("results = %v", rows)
		}
		return rows[0]
	}
	if r := row(sim(nil)); r["route_id"] != "rt_00000000a1" || r["manual"] != true || r["ready"] != true ||
		!strings.Contains(fmt.Sprint(r["prompt_preview"]), "Reply to Can we meet?.") {
		t.Fatalf("default route = %v", r)
	}
	if r := row(sim(map[string]any{"route_id": "rt_00000000a2"})); r["route_id"] != "rt_00000000a2" ||
		!strings.Contains(fmt.Sprint(r["prompt_preview"]), "Reply to alt Can we meet?.") {
		t.Fatalf("route a2 = %v", r)
	}
	// A route that gets nothing gives no events and no result.
	if out := sim(map[string]any{"route_id": "rt_00000000a3"}); len(scopeArr(t, out, "results")) != 0 {
		t.Fatalf("off route = %v", out)
	}
	r := row(sim(map[string]any{"event_ids": []string{evID}, "route_id": "rt_00000000ff"}))
	if g, _ := r["gate"].(map[string]any); g["matched"] != false || r["route_id"] != "" {
		t.Fatalf("unknown route = %v", r)
	}
	draft := "---\ninputs:\n  body: {type: string, required: true}\n---\n\nDraft for {{body}}.\n"
	if r := row(sim(map[string]any{"content": draft, "route_id": "rt_00000000a2"})); r["route_id"] != "rt_00000000a2" ||
		!strings.Contains(fmt.Sprint(r["prompt_preview"]), "Draft for alt Can we meet?.") {
		t.Fatalf("draft = %v", r)
	}
}

// TestConnectorDependentsRoutes proves that the dependents of a connector
// list the routes with their target, route id, gets, and broken reason.
func TestConnectorDependentsRoutes(t *testing.T) {
	_, base, _ := startAPI(t)
	wsPath := t.TempDir()
	ensureFiles := map[string]string{
		"agents/triage/AGENT.md":      "---\ndescription: Test agent.\nharness: fake\n---\n\nTriage.\n",
		"agents/triage/mailer/JOB.md": "---\nharness: fake\n---\n\nRun.\n",
	}
	for rel, md := range ensureFiles {
		p := filepath.Join(wsPath, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	routingtest.Write(t, wsPath, "email", routing.File{Mode: routing.ModeOff, Routes: []routing.Route{
		{ID: "rt_00000000b1", Agent: "triage", Gets: routing.GetsEveryMessage},
		{ID: "rt_00000000b2", Job: "triage/mailer", Gets: routing.GetsByHand},
		{ID: "rt_00000000b3", Assistant: true, Gets: routing.GetsOff},
		{ID: "rt_00000000b4", Agent: "ghost", Gets: routing.GetsEveryMessage},
	}})
	wsID := addWorkspace(t, base, wsPath, "dependents-routes")
	code, out, _ := doJSON(t, "GET", base+"/v1/connectors/email/dependents", nil)
	if code != 200 {
		t.Fatalf("dependents: %d %v", code, out)
	}
	byRoute := map[string]map[string]any{}
	for _, tr := range scopeArr(t, out, "triggers") {
		byRoute[fmt.Sprint(tr["route_id"])] = tr
	}
	type want struct{ kind, name, target, gets string }
	for id, w := range map[string]want{
		"rt_00000000b1": {"agent", "agents/triage", "triage", "every_message"},
		"rt_00000000b2": {"job", "agents/triage/mailer", "triage/mailer", "by_hand"},
		"rt_00000000b3": {"assistant", "assistant", "assistant", "off"},
		"rt_00000000b4": {"agent", "agents/ghost", "ghost", "every_message"},
	} {
		tr := byRoute[id]
		if tr["workspace"] != wsID || tr["kind"] != w.kind || tr["name"] != w.name || tr["target"] != w.target || tr["gets"] != w.gets {
			t.Fatalf("route %s = %v, want %+v", id, tr, w)
		}
		if broken := id == "rt_00000000b4"; (tr["broken"] != nil && tr["broken"] != "") != broken {
			t.Fatalf("route %s broken = %v", id, tr["broken"])
		}
	}
}

// routeEventFixture is a workspace with the job agents/ops/reply, the given
// routes of the email connection, and one stored email event.
func routeEventFixture(t *testing.T, routes []routing.Route) (base, wsID, evID string) {
	t.Helper()
	s, base, dir := startAPI(t)
	wsDir := t.TempDir()
	wsID = addWorkspace(t, base, wsDir, "route-event-ws")
	ensureAgent(t, base, wsID, "ops")
	md := "---\nharness: fake\ninputs:\n  body: {type: string, required: true}\n---\n\nReply to {{body}}.\n"
	if code, body, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{"workspace": wsID, "name": "agents/ops/reply", "markdown": md}); code != 201 {
		t.Fatalf("create %d %v", code, body)
	}
	routingtest.Write(t, wsDir, "email", routing.File{Mode: routing.ModeOff, Routes: routes})
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	t.Cleanup(func() { _ = st.Close() })
	evID, err := connectors.New(st, s.Workspaces(), nil).InjectNative(context.Background(), "email",
		map[string]any{"subject": "hi", "text": "Can we meet?"}, connectors.NativeMeta{SourceID: "m1", Type: "mail.received"})
	if err != nil {
		t.Fatal(err)
	}
	return base, wsID, evID
}

// twoHandRoutes are two by-hand routes of the reply job that fill the body
// in different ways, and a third route that gets nothing.
func twoHandRoutes() []routing.Route {
	return []routing.Route{
		{ID: "rt_00000000a1", Job: "ops/reply", Gets: routing.GetsByHand, Inputs: job.InputMap{{Name: "body", Value: "{{data.text}}"}}},
		{ID: "rt_00000000a2", Job: "ops/reply", Gets: routing.GetsByHand, Inputs: job.InputMap{{Name: "body", Value: "alt {{data.text}}"}}},
		{ID: "rt_00000000a3", Job: "ops/reply", Gets: routing.GetsOff, Inputs: job.InputMap{{Name: "body", Value: "off {{data.text}}"}}},
	}
}

// listedRoutes is the route ids of the rows of one event in an event list.
func listedRoutes(t *testing.T, list map[string]any, evID string) []string {
	t.Helper()
	var got []string
	for _, row := range scopeArr(t, list, "events") {
		if row["id"] == evID {
			got = append(got, fmt.Sprint(row["route_id"]))
		}
	}
	return got
}
