package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/trebi-ai/trebi/internal/store"
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
	jobDir := filepath.Join(wsPath, "mailer")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nharness: fake\ntriggers:\n  - event: email\n    rooms: [\"inbox\"]\n    allowlist: [\"a@x.io\"]\n---\n\nRun.\n"
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
	if tr["kind"] != "job" || tr["workspace"] != wsID || tr["name"] != "mailer" || tr["on_index"] != float64(0) ||
		tr["active"] != true || tr["disabled"] != false || tr["senders"] != float64(1) {
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
	writeJobFile(t, wsDir, "watch")
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
		VALUES (?,?,?,?,?,?,?,?,?)`, "q1", "discord", wsID, indexedJobID(t, st, wsID, "watch"), "job", "r1/e1", "pending", now, now); err != nil {
		t.Fatal(err)
	}
	st.Close()

	gate := map[string]any{"from": "bots", "contains": []string{"failure"}, "replies": "continue", "workspace": wsID, "job": "watch"}
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
	if _, err := st.DB().Exec(`INSERT INTO event_outcomes (event_id, workspace_id, job_id, on_index, outcome, created_at)
		VALUES (?,?,?,?,?,?)`, e3, wsA, "j1", 0, "queued", now); err != nil {
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
		case <-time.After(5 * time.Second):
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
