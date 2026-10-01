package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestConnectorRoutes covers the connection write surface: create conflict,
// patch layers, strict decode, and the lifecycle guard on a connection
// without an events block.
func TestConnectorRoutes(t *testing.T) {
	do := startConnectorAPI(t)

	def := inlineEntry("feed", map[string]any{
		"setup":  map[string]any{"inputs": []map[string]any{{"name": "MODE"}}},
		"events": map[string]any{"command": "echo one", "format": "json", "id": "id"},
	})
	def["inputs"] = map[string]string{"MODE": "ro"}
	code, created := do("POST", "/v1/connectors", def)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, created)
	}
	id, _ := created["id"].(string)
	if !strings.HasPrefix(id, "con_") || created["slug"] != "feed" || created["type"] != "feed" {
		t.Fatalf("created row = %v", created)
	}

	// A second create with the same slug is a conflict.
	if code, body := do("POST", "/v1/connectors", def); code != http.StatusConflict || errCode(body) != "conflict" {
		t.Fatalf("duplicate create = %d %v", code, body)
	}
	// The daemon sets the id.
	withID := maps.Clone(def)
	withID["id"] = id
	withID["slug"] = "feed-x"
	if code, body := do("POST", "/v1/connectors", withID); code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("create with id = %d %v", code, body)
	}

	// A test event with a stored source id is a conflict, not a silent drop.
	if code, body := do("POST", "/v1/connectors/feed/test", map[string]any{"event": map[string]any{"id": "t1"}}); code != http.StatusOK {
		t.Fatalf("test event = %d %v", code, body)
	}
	if code, body := do("POST", "/v1/connectors/"+id+"/test", map[string]any{"event": map[string]any{"id": "t1"}}); code != http.StatusConflict || errCode(body) != "conflict" {
		t.Fatalf("duplicate test event = %d %v", code, body)
	}
	// Retrigger reuses the stored row, so the duplicate source id is no bar.
	code, list := do("GET", "/v1/events?connector=feed", nil)
	events, _ := list["events"].([]any)
	if code != http.StatusOK || len(events) != 1 {
		t.Fatalf("events = %d %v", code, list)
	}
	ev, _ := events[0].(map[string]any)
	if ev["instance"] != id || ev["slug"] != "feed" || ev["connector"] != "feed" {
		t.Fatalf("event row = %v", ev)
	}
	eventID, _ := ev["id"].(string)
	// The catch_up filter binds an int.
	for q, want := range map[string]int{"0": 1, "1": 0} {
		code, list := do("GET", "/v1/events?connector=feed&catch_up="+q, nil)
		if got, _ := list["events"].([]any); code != http.StatusOK || len(got) != want {
			t.Fatalf("catch_up=%s = %d %v; want %d rows", q, code, list, want)
		}
	}
	if code, _ := do("GET", "/v1/events?catch_up=yes", nil); code != http.StatusBadRequest {
		t.Fatalf("catch_up=yes = %d; want 400", code)
	}
	if code, body := do("POST", "/v1/events/"+eventID+"/retrigger", nil); code != http.StatusOK {
		t.Fatalf("retrigger = %d %v", code, body)
	}
	if code, body := do("POST", "/v1/events/missing/retrigger", nil); code != http.StatusNotFound {
		t.Fatalf("retrigger missing = %d %v", code, body)
	}
	// An unknown key inside a PATCH manifest fails the strict decode.
	badManifest := inlineEntry("feed", map[string]any{"events": map[string]any{"command": "x", "formt": "json"}})["manifest"]
	if code, body := do("PATCH", "/v1/connectors/feed", map[string]any{"manifest": badManifest}); code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("patch manifest unknown key = %d %v", code, body)
	}

	// An unknown top-level key fails the strict decode.
	bad := map[string]any{"type": "feed2", "nonsense": true}
	if code, body := do("POST", "/v1/connectors", bad); code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("unknown field = %d %v", code, body)
	}
	// PUT is gone: PATCH covers every field.
	if code, _ := do("PUT", "/v1/connectors/feed", def); code != http.StatusMethodNotAllowed {
		t.Fatalf("put = %d; want 405", code)
	}

	// A patch replaces the inputs as a whole.
	if code, body := do("PATCH", "/v1/connectors/feed", map[string]any{"inputs": map[string]string{"EXTRA": "1"}}); code != http.StatusOK {
		t.Fatalf("patch = %d %v", code, body)
	}
	inputs, _ := getConnector(t, do, "feed")["inputs"].(map[string]any)
	if _, kept := inputs["MODE"]; kept || inputs["EXTRA"] != "***" {
		t.Fatalf("patch inputs = %v; want a whole replace", inputs)
	}
	if code, body := do("PATCH", "/v1/connectors/feed", map[string]any{"nope": 1}); code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("patch unknown field = %d %v", code, body)
	}
	// A merge patch changes the resolved manifest, and a null removes it.
	if code, body := do("PATCH", "/v1/connectors/feed", map[string]any{"patch": map[string]any{"description": "Patched"}}); code != http.StatusOK {
		t.Fatalf("patch layer = %d %v", code, body)
	}
	if row := getConnector(t, do, id); row["description"] != "Patched" {
		t.Fatalf("patch layer not applied: %v", row["description"])
	}
	if code, body := do("PATCH", "/v1/connectors/"+id, map[string]any{"patch": nil}); code != http.StatusOK {
		t.Fatalf("patch null = %d %v", code, body)
	}
	if row := getConnector(t, do, "feed"); row["description"] != "Test connector" {
		t.Fatalf("null patch kept the layer: %v", row["description"])
	}
	// A new manifest can swap events for actions.
	swap := inlineEntry("feed", map[string]any{"actions": map[string]any{"mcp": map[string]any{"command": "npx", "args": []string{"-y", "demo"}}}})["manifest"]
	if code, body := do("PATCH", "/v1/connectors/feed", map[string]any{"manifest": swap}); code != http.StatusOK {
		t.Fatalf("patch manifest = %d %v", code, body)
	}
	row := getConnector(t, do, "feed")
	if row["events"] != nil {
		t.Fatalf("new manifest kept events: %v", row["events"])
	}
	if acts, _ := row["actions"].(map[string]any); acts == nil {
		t.Fatalf("patch dropped actions: %v", row)
	}

	// A connection without events refuses the lifecycle routes with 409.
	cliOnly := inlineEntry("cli-only", map[string]any{"actions": map[string]any{"mcp": map[string]any{"command": "npx"}}})
	if code, body := do("POST", "/v1/connectors", cliOnly); code != http.StatusCreated {
		t.Fatalf("create cli-only = %d %v", code, body)
	}
	if code, body := do("POST", "/v1/connectors/cli-only/connect", nil); code != http.StatusConflict || errCode(body) != "validation" {
		t.Fatalf("connect without events = %d %v", code, body)
	}
	if code, body := do("POST", "/v1/connectors/cli-only/restart", nil); code != http.StatusConflict {
		t.Fatalf("restart without events = %d %v", code, body)
	}

	// Reserved and unknown names answer from the guard.
	if code, _ := do("DELETE", "/v1/connectors/email", nil); code == http.StatusOK {
		t.Fatal("email is reserved; delete must not succeed")
	}
	// Native connectors have no process log.
	for _, name := range []string{"email", "webhook"} {
		if code, body := do("GET", "/v1/connectors/"+name+"/logs", nil); code != http.StatusNotFound {
			t.Fatalf("%s logs = %d %v; want 404", name, code, body)
		}
	}
	if code, body := do("GET", "/v1/connectors/ghost", nil); code != http.StatusNotFound || errCode(body) != "not_found" {
		t.Fatalf("get unknown = %d %v", code, body)
	}

	// DELETE removes the connection folder.
	dir := home.ConnectionDir(id)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("folder: %v", err)
	}
	if code, body := do("DELETE", "/v1/connectors/"+id+"?confirm=1", nil); code != http.StatusOK {
		t.Fatalf("delete = %d %v", code, body)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("delete kept the folder: %v", err)
	}
}

// TestConnectorTwoOfOneType proves two connections of one type each get a
// slug, an id, and a folder, and that the events filter tells them apart by
// slug and joins them by type.
func TestConnectorTwoOfOneType(t *testing.T) {
	do := startConnectorAPI(t)
	def := inlineEntry("feed", map[string]any{"events": map[string]any{"command": "sleep 30", "format": "json", "id": "id"}})
	delete(def, "slug")
	ids := map[string]string{}
	for range 2 {
		code, row := do("POST", "/v1/connectors", def)
		if code != http.StatusCreated {
			t.Fatalf("create = %d %v", code, row)
		}
		ids[row["slug"].(string)] = row["id"].(string)
	}
	if len(ids) != 2 || ids["feed"] == "" || ids["feed"] == ids[otherSlug(ids)] {
		t.Fatalf("slugs = %v", ids)
	}
	second := otherSlug(ids)
	for slug, id := range ids {
		if _, err := os.Stat(filepath.Join(home.ConnectionDir(id), "connection.yaml")); err != nil {
			t.Fatalf("%s folder: %v", slug, err)
		}
		if code, body := do("POST", "/v1/connectors/"+slug+"/test", map[string]any{"event": map[string]any{"id": "e-" + slug}}); code != http.StatusOK {
			t.Fatalf("test %s = %d %v", slug, code, body)
		}
	}
	count := func(q string) int {
		t.Helper()
		code, list := do("GET", "/v1/events?connector="+q, nil)
		if code != http.StatusOK {
			t.Fatalf("events %s = %d %v", q, code, list)
		}
		evs, _ := list["events"].([]any)
		return len(evs)
	}
	// feed is a slug and a type; the slug wins.
	if n := count("feed"); n != 1 {
		t.Fatalf("slug feed = %d; want 1", n)
	}
	if n := count(second); n != 1 {
		t.Fatalf("slug %s = %d; want 1", second, n)
	}
	if n := count(ids[second]); n != 1 {
		t.Fatalf("id filter = %d; want 1", n)
	}
	// A title change moves no file.
	if code, body := do("PATCH", "/v1/connectors/"+second, map[string]any{"title": "Second feed"}); code != http.StatusOK || body["title"] != "Second feed" || body["changed_files"] != nil {
		t.Fatalf("title = %d %v", code, body)
	}
	// A slug that starts with con_ fails validation.
	if code, body := do("PATCH", "/v1/connectors/"+second, map[string]any{"slug": "con_x"}); code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("con_ slug = %d %v", code, body)
	}
}

// TestConnectorSlugRewrite proves a slug change rewrites a job file that
// names the slug and lists it in changed_files, and that a failed write
// puts the files back and returns 409.
func TestConnectorSlugRewrite(t *testing.T) {
	do := startConnectorAPI(t)
	wsPath := filepath.Join(t.TempDir(), "ws")
	jobDir := filepath.Join(wsPath, "watch")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jobPath := filepath.Join(jobDir, "JOB.md")
	jobMD := "---\nharness: claude\nconnectors: [feed]\ntriggers:\n  - event: feed\n---\n\nGo.\n"
	if err := os.WriteFile(jobPath, []byte(jobMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, body := do("POST", "/v1/workspaces", map[string]string{"name": "rw", "path": wsPath}); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("workspace = %d %v", code, body)
	}
	def := inlineEntry("feed", map[string]any{"actions": map[string]any{"mcp": map[string]any{"command": "npx"}}})
	if code, body := do("POST", "/v1/connectors", def); code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, body)
	}

	// A write that fails puts the files back and keeps the slug.
	if err := os.Chmod(jobDir, 0o555); err != nil {
		t.Fatal(err)
	}
	code, body := do("PATCH", "/v1/connectors/feed", map[string]any{"slug": "news"})
	_ = os.Chmod(jobDir, 0o755)
	if code != http.StatusConflict || errCode(body) != "conflict" {
		t.Fatalf("failed rewrite = %d %v", code, body)
	}
	if got, _ := os.ReadFile(jobPath); string(got) != jobMD {
		t.Fatalf("job changed:\n%s", got)
	}
	getConnector(t, do, "feed")

	code, body = do("PATCH", "/v1/connectors/feed", map[string]any{"slug": "news"})
	if code != http.StatusOK || body["slug"] != "news" {
		t.Fatalf("rewrite = %d %v", code, body)
	}
	if files, _ := body["changed_files"].([]any); !slices.ContainsFunc(files, func(f any) bool { return f == jobPath }) {
		t.Fatalf("changed_files = %v; want %s", body["changed_files"], jobPath)
	}
	want := strings.ReplaceAll(jobMD, "feed", "news")
	if got, _ := os.ReadFile(jobPath); string(got) != want {
		t.Fatalf("job =\n%s\nwant\n%s", got, want)
	}
	if code, _ := do("GET", "/v1/connectors/feed", nil); code != http.StatusNotFound {
		t.Fatalf("old slug = %d; want 404", code)
	}
}

// TestConnectorInputMaskRoundTrip proves the masked GET inputs can go back
// on PATCH without a loss of the stored value.
func TestConnectorInputMaskRoundTrip(t *testing.T) {
	do := startConnectorAPI(t)
	def := inlineEntry("api", map[string]any{
		"setup": map[string]any{"inputs": []map[string]any{{"name": "API_KEY"}}},
		"actions": map[string]any{"mcp": map[string]any{
			"url": "https://mcp.example.com", "headers": map[string]string{"X-Key": "${API_KEY}"},
		}},
	})
	def["inputs"] = map[string]string{"API_KEY": "abc"}
	code, created := do("POST", "/v1/connectors", def)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, created)
	}
	stored := func() string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(home.ConnectionDir(created["id"].(string)), "connection.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if in := getConnector(t, do, "api")["inputs"].(map[string]any); in["API_KEY"] != "***" {
		t.Fatalf("GET inputs = %v", in)
	}
	if code, body := do("PATCH", "/v1/connectors/api", map[string]any{"inputs": map[string]string{"API_KEY": "***"}}); code != http.StatusOK {
		t.Fatalf("patch = %d %v", code, body)
	}
	if !strings.Contains(stored(), "API_KEY: abc") {
		t.Fatalf("patch lost the input:\n%s", stored())
	}
	if code, body := do("PATCH", "/v1/connectors/api", map[string]any{"inputs": map[string]string{"NEW": "***"}}); code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("mask without value = %d %v", code, body)
	}
}

// TestConnectorPatchKeepsEvents proves a patch restarts the supervisor but
// leaves the connector cursor alone.
func TestConnectorPatchKeepsEvents(t *testing.T) {
	do := startConnectorAPI(t)

	def := inlineEntry("keep", map[string]any{"events": map[string]any{"command": "sleep 30", "format": "json", "id": "id"}})
	if code, body := do("POST", "/v1/connectors", def); code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, body)
	}
	if code, body := do("PUT", "/v1/connectors/keep/cursor", map[string]string{"last_event_id": "ev-7"}); code != http.StatusOK {
		t.Fatalf("cursor put = %d %v", code, body)
	}
	if code, body := do("PATCH", "/v1/connectors/keep", map[string]any{"inputs": map[string]string{"A": "1"}}); code != http.StatusOK {
		t.Fatalf("patch = %d %v", code, body)
	}
	code, cur := do("GET", "/v1/connectors/keep/cursor", nil)
	if code != http.StatusOK {
		t.Fatalf("cursor get = %d %v", code, cur)
	}
	if cur["last_event_id"] != "ev-7" || cur["slug"] != "keep" {
		t.Fatalf("patch moved the cursor: %v", cur)
	}
}

// TestConnectorBadFolder proves a folder whose connection.yaml does not load
// gives that row a load_error, while the other connections work.
func TestConnectorBadFolder(t *testing.T) {
	do := startConnectorAPI(t)
	good := inlineEntry("feed", map[string]any{"actions": map[string]any{"mcp": map[string]any{"command": "npx"}}})
	if code, body := do("POST", "/v1/connectors", good); code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, body)
	}
	badID := "con_01k6a0000000000000000000zz"
	if err := os.MkdirAll(home.ConnectionDir(badID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.ConnectionDir(badID), "connection.yaml"), []byte("id: "+badID+"\nnonsense: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, list := do("GET", "/v1/connectors", nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d %v", code, list)
	}
	var sawBad, sawGood bool
	for _, r := range list["connectors"].([]any) {
		row := r.(map[string]any)
		switch row["id"] {
		case badID:
			sawBad = row["load_error"] != nil
		default:
			sawGood = sawGood || (row["slug"] == "feed" && row["load_error"] == nil)
		}
	}
	if !sawBad || !sawGood {
		t.Fatalf("list = %v", list)
	}
	if code, body := do("PATCH", "/v1/connectors/"+badID, map[string]any{"title": "x"}); code != http.StatusConflict || errCode(body) != "conflict" {
		t.Fatalf("patch of a bad folder = %d %v", code, body)
	}
	// Delete is the way out of a folder that does not load.
	if code, body := do("DELETE", "/v1/connectors/"+badID+"?confirm=1", nil); code != http.StatusOK {
		t.Fatalf("delete of a bad folder = %d %v", code, body)
	}
	if _, err := os.Stat(home.ConnectionDir(badID)); !os.IsNotExist(err) {
		t.Fatalf("delete kept the bad folder: %v", err)
	}
}

// TestRulesFireConnection proves rules/fire with a slug stores the type and
// the connection id on the event row, and the id on the queue row.
func TestRulesFireConnection(t *testing.T) {
	do := startConnectorAPI(t)
	code, row := do("POST", "/v1/connectors", inlineEntry("feed", map[string]any{"actions": map[string]any{"mcp": map[string]any{"command": "npx"}}}))
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, row)
	}
	id := row["id"].(string)
	code, res := do("POST", "/v1/rules/fire", map[string]any{
		"connector": "feed", "workspace": "ws", "job": "j", "match": "true", "event": map[string]any{"x": 1},
	})
	if code != http.StatusOK || res["outcome"] != "queued" {
		t.Fatalf("fire = %d %v", code, res)
	}
	code, ev := do("GET", "/v1/events/"+res["event_id"].(string), nil)
	if code != http.StatusOK || ev["connector"] != "feed" || ev["instance"] != id || ev["slug"] != "feed" {
		t.Fatalf("event = %d %v", code, ev)
	}
	code, q := do("GET", "/v1/queue", nil)
	items, _ := q["items"].([]any)
	if code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["connector"] != id {
		t.Fatalf("queue = %d %v", code, q)
	}
}

// inlineEntry is one inline connection; blocks go into the manifest. The
// slug is the type.
func inlineEntry(name string, blocks map[string]any) map[string]any {
	m := map[string]any{"schema": "trebi-connector/1", "name": name, "title": name, "description": "Test connector", "version": "1.0.0"}
	maps.Copy(m, blocks)
	return map[string]any{"type": name, "slug": name, "from": "inline", "manifest": m}
}

// otherSlug returns the key of ids that is not feed.
func otherSlug(ids map[string]string) string {
	for k := range ids {
		if k != "feed" {
			return k
		}
	}
	return ""
}

// startConnectorAPI serves the API over a fresh home.
func startConnectorAPI(t *testing.T) func(method, path string, body any) (int, map[string]any) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, api.Options{})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr
	client := &http.Client{Timeout: 10 * time.Second}
	return func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, base+path, r)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return resp.StatusCode, m
	}
}

// getConnector reads one connector row and fails the test on a bad status.
func getConnector(t *testing.T, do func(string, string, any) (int, map[string]any), name string) map[string]any {
	t.Helper()
	code, row := do("GET", "/v1/connectors/"+name, nil)
	if code != http.StatusOK {
		t.Fatalf("get %s = %d %v", name, code, row)
	}
	return row
}

// TestConnectorThreadsGuard proves a connector without the threads feature
// answers 400 unsupported on the threads route and on a thread bind.
func TestConnectorThreadsGuard(t *testing.T) {
	do := startConnectorAPI(t)
	def := inlineEntry("flat", map[string]any{
		"events":  map[string]any{"command": "sleep 30", "protocol": "trebi-connector/1", "types": []map[string]any{{"type": "message"}}},
		"channel": map[string]any{"features": []string{"typing"}},
	})
	if code, body := do("POST", "/v1/connectors", def); code != 201 {
		t.Fatalf("create %d %v", code, body)
	}
	code, body := do("GET", "/v1/connectors/flat/rooms/r1/threads", nil)
	e, _ := body["error"].(map[string]any)
	if code != 400 || e["code"] != "unsupported" || e["message"] != "connector flat has no threads" {
		t.Fatalf("threads %d %v", code, body)
	}
	code, body = do("POST", "/v1/workspaces", map[string]any{"path": t.TempDir(), "name": "flatws"})
	if code != 201 {
		t.Fatalf("workspace %d %v", code, body)
	}
	ws, _ := body["id"].(string)
	code, body = do("POST", "/v1/connectors/flat/rooms/r1/bind", map[string]any{"workspace": ws, "thread": "t1"})
	e, _ = body["error"].(map[string]any)
	if code != 400 || e["code"] != "unsupported" {
		t.Fatalf("thread bind %d %v", code, body)
	}
	if code, body = do("POST", "/v1/connectors/flat/rooms/r1/bind", map[string]any{"workspace": ws}); code != 200 {
		t.Fatalf("room bind %d %v", code, body)
	}
}
