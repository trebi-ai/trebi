package api_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/ghfake"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/registry"
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
	writeAgentFile(t, wsPath, "ops")
	jobDir := filepath.Join(wsPath, "agents", "ops", "watch")
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
	// The index writes the job ID first; the rewrite keeps that line.
	raw, err := os.ReadFile(jobPath)
	if err != nil {
		t.Fatal(err)
	}
	idLine, rest, ok := strings.Cut(strings.TrimPrefix(string(raw), "---\n"), "\n")
	if !strings.HasPrefix(string(raw), "---\nid: jb_") || !ok || "---\n"+rest != jobMD {
		t.Fatalf("indexed job:\n%s", raw)
	}
	jobMD = string(raw)
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
	want := "---\n" + idLine + "\n" + strings.ReplaceAll(rest, "feed", "news")
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
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "agents/ops/j")
	code, ws := do("POST", "/v1/workspaces", map[string]string{"name": "ws", "path": wsDir})
	if code != http.StatusCreated {
		t.Fatalf("workspace = %d %v", code, ws)
	}
	code, res := do("POST", "/v1/rules/fire", map[string]any{
		"connector": "feed", "workspace": ws["id"], "job": "agents/ops/j", "match": "true", "event": map[string]any{"x": 1},
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

// TestConnectorSubscriptionRoutes proves the record routes of the watch
// list, 404 for an unknown id, and 400 unsupported without a form.
func TestConnectorSubscriptionRoutes(t *testing.T) {
	do := startConnectorAPI(t)
	events := map[string]any{"command": "sleep 30", "protocol": "trebi-connector/1", "types": []map[string]any{{"type": "push"}, {"type": "issue"}}}
	watch := maps.Clone(events)
	watch["subscriptions"] = map[string]any{
		"action": "Watch a repository", "modes": []string{"api", "poll"}, "webhook": map[string]any{},
		"fields": []map[string]any{{"name": "repo", "label": "Repository", "required": true, "room": true}},
	}
	for name, ev := range map[string]any{"gh": watch, "flat": events} {
		if code, body := do("POST", "/v1/connectors", inlineEntry(name, map[string]any{"events": ev})); code != 201 {
			t.Fatalf("create %s %d %v", name, code, body)
		}
	}
	code, body := do("GET", "/v1/connectors/flat/subscriptions", nil)
	if e, _ := body["error"].(map[string]any); code != 400 || e["code"] != "unsupported" {
		t.Fatalf("flat %d %v", code, body)
	}
	code, body = do("POST", "/v1/connectors/gh/subscriptions", map[string]any{"values": map[string]any{}})
	if code != 400 {
		t.Fatalf("missing field %d %v", code, body)
	}
	code, body = do("POST", "/v1/connectors/gh/subscriptions", map[string]any{"values": map[string]any{"repo": "a/b"}, "types": []string{"push"}})
	subs, _ := body["subscriptions"].([]any)
	if code != 201 || len(subs) != 1 || body["action"] != "Watch a repository" {
		t.Fatalf("subscribe %d %v", code, body)
	}
	row, _ := subs[0].(map[string]any)
	id, _ := row["id"].(string)
	if row["state"] != "pending" || !strings.HasPrefix(id, "sub_") {
		t.Fatalf("row %v", row)
	}
	if code, body = do("PATCH", "/v1/connectors/gh/subscriptions/"+id, map[string]any{"types": []string{}}); code != 200 {
		t.Fatalf("patch %d %v", code, body)
	}
	if list, _ := getConnector(t, do, "gh")["subscriptions"].([]any); len(list) != 1 {
		t.Fatalf("record %v", list)
	}
	if code, body = do("PATCH", "/v1/connectors/gh/subscriptions/sub_nope", map[string]any{}); code != 404 {
		t.Fatalf("unknown %d %v", code, body)
	}
	if code, body = do("DELETE", "/v1/connectors/gh/subscriptions/"+id, nil); code != 200 {
		t.Fatalf("delete %d %v", code, body)
	}
	if list, _ := getConnector(t, do, "gh")["subscriptions"].([]any); len(list) != 0 {
		t.Fatalf("record after delete %v", list)
	}
	for _, path := range []string{"/v1/connectors/gh/subscriptions/sync", "/v1/connectors/gh/subscriptions/options"} {
		if code, body = do("POST", path, map[string]any{"field": "repo"}); code < 400 || code >= 500 {
			t.Fatalf("%s %d %v", path, code, body)
		}
	}
}

// TestConnectorLinkRoutes covers the link check and the install from a link,
// with each error code of the link routes.
func TestConnectorLinkRoutes(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go")
	}
	gh := ghfake.New()
	t.Setenv(connectors.EnvGitHubAPI, gh.API.URL)
	t.Setenv("PATH", filepath.Dir(gobin)+":/usr/bin:/bin") // no gh: a 404 must not send a real login
	sha := gh.Push("acme/tools", "main", linkFixture(t, "buildable", "vitally/"))
	gh.Push("acme/tools", "empty", linkFixture(t, "nomanifest", "vitally/"))
	base := startCatalogAPIWith(t, func(fixture string) registry.Config {
		return registry.Config{Registries: []config.Registry{{Name: "official", URL: fixture}}}
	})
	link := "https://github.com/acme/tools/tree/main/vitally"

	code, body, _ := doJSON(t, "POST", base+"/v1/connectors/sources/check", map[string]any{"url": link})
	src, _ := body["source"].(map[string]any)
	if code != 200 || body["can_install"] != true || src["commit"] != sha || body["id"] == "" {
		t.Fatalf("check %d %v", code, body)
	}
	checkID := body["id"]
	cases := []struct {
		name, path string
		body       map[string]any
		code       int
		errCode    string
	}{
		{"bad link", "sources/check", map[string]any{"url": base + "/healthz"}, 400, "validation"},
		{"no url", "sources/check", map[string]any{}, 400, "validation"},
		{"missing repo", "sources/check", map[string]any{"url": "acme/nope"}, 404, "not_found"},
		{"source with name", "install", map[string]any{"source": map[string]any{"url": link}, "name": "vitally"}, 400, "validation"},
		{"source with track", "install", map[string]any{"source": map[string]any{"url": link}, "track": "minor"}, 400, "validation"},
		{"failed check", "install", map[string]any{"source": map[string]any{"url": "https://github.com/acme/tools/tree/empty/vitally"}}, 422, "validation"},
	}
	for _, tc := range cases {
		code, body, _ := doJSON(t, "POST", base+"/v1/connectors/"+tc.path, tc.body)
		e, _ := body["error"].(map[string]any)
		if code != tc.code || e["code"] != tc.errCode {
			t.Fatalf("%s: %d %v", tc.name, code, body)
		}
		if tc.code == 422 {
			if d, _ := e["details"].(map[string]any); d["reason"] != connectors.ReasonCheckFailed || d["results"] == nil {
				t.Fatalf("%s: details %v", tc.name, e["details"])
			}
		}
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/install", map[string]any{
		"source": map[string]any{"url": link, "check_id": checkID}, "slug": "vitally-work",
	})
	if code != 201 || body["from"] != "github" || body["slug"] != "vitally-work" || body["publisher"] != connectors.PublisherUnverified {
		t.Fatalf("install %d %v", code, body)
	}
	if src, _ := body["source"].(map[string]any); src["commit"] != sha {
		t.Fatalf("row source %v", body["source"])
	}

	gh.RateLimit(true)
	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/sources/check", map[string]any{"url": link})
	e, _ := body["error"].(map[string]any)
	if d, _ := e["details"].(map[string]any); code != 429 || e["code"] != "rate_limited" || d["retry_after"].(float64) <= 0 {
		t.Fatalf("rate limit %d %v", code, body)
	}
	gh.RateLimit(false)
	gh.Close()
	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/sources/check", map[string]any{"url": link})
	if e, _ := body["error"].(map[string]any); code != 502 || e["code"] != "upstream" {
		t.Fatalf("down %d %v", code, body)
	}
}

// linkFixture reads one folder of the connector link fixtures into a fake
// tree under prefix.
func linkFixture(t *testing.T, name, prefix string) map[string]ghfake.File {
	t.Helper()
	root := filepath.Join("..", "connectors", "testdata", "sources", name)
	out := map[string]ghfake.File{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		out[prefix+filepath.ToSlash(rel)] = ghfake.File{Body: string(b)}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestConnectorPackageRoutes installs packages from the catalog and from a
// link with no connection, lists them, and checks that gc keeps a held
// package and removes an unheld one.
func TestConnectorPackageRoutes(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go")
	}
	gh := ghfake.New()
	t.Cleanup(gh.Close)
	t.Setenv(connectors.EnvGitHubAPI, gh.API.URL)
	t.Setenv("PATH", filepath.Dir(gobin)+":/usr/bin:/bin") // no gh: a 404 must not send a real login
	sha := gh.Push("acme/tools", "main", linkFixture(t, "buildable", "vitally/"))
	catURL, pub := trebiCatalogFixture(t)
	base := startCatalogAPIWith(t, func(fixture string) registry.Config {
		return registry.Config{
			Registries:  []config.Registry{{Name: "trebi", URL: catURL, Kind: config.RegistryTrebi}, {Name: "official", URL: fixture}},
			CatalogKeys: []ed25519.PublicKey{pub},
		}
	})
	link := "https://github.com/acme/tools/tree/main/vitally"

	bad := []map[string]any{
		{},
		{"source": map[string]any{}},
		{"source": map[string]any{"url": link, "catalog": "demo"}},
		{"source": map[string]any{"catalog": "demo", "check_id": "chk_x"}},
		{"source": map[string]any{"url": link, "version": "1.0.0"}},
		{"source": map[string]any{"catalog": "demo"}, "slug": "x"},
	}
	for _, b := range bad {
		if code, body, _ := doJSON(t, "POST", base+"/v1/connectors/packages", b); code != 400 || errCode(body) != "validation" {
			t.Fatalf("bad install %v: %d %v", b, code, body)
		}
	}

	code, old, _ := doJSON(t, "POST", base+"/v1/connectors/packages", map[string]any{"source": map[string]any{"catalog": "demo", "version": "1.0.0"}})
	if code != 201 || old["type"] != "demo" || old["version"] != "1.0.0" || !strings.HasPrefix(old["digest"].(string), "sha256:") {
		t.Fatalf("catalog install %d %v", code, old)
	}
	if _, err := os.Stat(old["dir"].(string)); err != nil {
		t.Fatalf("package dir: %v", err)
	}
	code, newest, _ := doJSON(t, "POST", base+"/v1/connectors/packages", map[string]any{"source": map[string]any{"catalog": "demo"}})
	if code != 201 || newest["version"] != "1.1.0" {
		t.Fatalf("latest install %d %v", code, newest)
	}
	code, lp, _ := doJSON(t, "POST", base+"/v1/connectors/packages", map[string]any{"source": map[string]any{"url": link}})
	if code != 201 || lp["type"] != "vitally" || lp["version"] != "0.1.0+g"+sha[:12] || lp["bin"] == "" {
		t.Fatalf("link install %d %v", code, lp)
	}
	if _, err := os.Stat(lp["bin"].(string)); err != nil {
		t.Fatalf("link program: %v", err)
	}

	code, body, _ := doJSON(t, "GET", base+"/v1/connectors", nil)
	for _, row := range body["connectors"].([]any) {
		if r := row.(map[string]any); r["type"] == "demo" || r["type"] == "vitally" {
			t.Fatalf("a package install created a connection: %v", r)
		}
	}
	if code != 200 {
		t.Fatalf("list connectors %d", code)
	}

	for _, b := range []map[string]any{
		{"holder": "Bad Name", "packages": []string{}},
		{"holder": "perception", "packages": []string{"demo"}},
		{"holder": "perception", "packages": []string{"demo@1.0.0"}, "extra": true},
	} {
		if code, body, _ := doJSON(t, "PUT", base+"/v1/connectors/packages/keep", b); code != 400 || errCode(body) != "validation" {
			t.Fatalf("bad keep %v: %d %v", b, code, body)
		}
	}
	lref := "vitally@" + lp["version"].(string)
	code, body, _ = doJSON(t, "PUT", base+"/v1/connectors/packages/keep", map[string]any{"holder": "perception", "packages": []string{"demo@1.0.0", lref, "demo@1.0.0"}})
	if kept, _ := body["packages"].([]any); code != 200 || len(kept) != 2 {
		t.Fatalf("keep %d %v", code, body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/connectors/packages", nil)
	pkgs, _ := body["packages"].([]any)
	if code != 200 || len(pkgs) != 3 {
		t.Fatalf("list %d %v", code, body)
	}
	holders := map[string]any{}
	for _, p := range pkgs {
		row := p.(map[string]any)
		holders[row["type"].(string)+"@"+row["version"].(string)] = row["holders"]
	}
	if h, _ := holders["demo@1.0.0"].([]any); len(h) != 1 || h[0] != "perception" {
		t.Fatalf("holders %v", holders)
	}
	if h, _ := holders["demo@1.1.0"].([]any); len(h) != 0 {
		t.Fatalf("holders %v", holders)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/gc", map[string]any{})
	if removed, _ := body["removed"].([]any); code != 200 || len(removed) != 1 || removed[0] != "demo@1.1.0" {
		t.Fatalf("gc %d %v", code, body)
	}
	if _, err := os.Stat(old["dir"].(string)); err != nil {
		t.Fatalf("gc removed a held package: %v", err)
	}
	if _, err := os.Stat(newest["dir"].(string)); !os.IsNotExist(err) {
		t.Fatalf("gc kept an unheld package: %v", err)
	}

	if code, body, _ := doJSON(t, "PUT", base+"/v1/connectors/packages/keep", map[string]any{"holder": "perception", "packages": []string{}}); code != 200 {
		t.Fatalf("release %d %v", code, body)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/connectors/gc", map[string]any{"dry_run": true})
	if removed, _ := body["removed"].([]any); code != 200 || len(removed) != 2 {
		t.Fatalf("gc after release %d %v", code, body)
	}
}

// TestJobValidateTriggerAccess proves validate tells the access of the runs
// that an event trigger starts: read only without connectors, and no issue
// when the job names the connection.
func TestJobValidateTriggerAccess(t *testing.T) {
	do := startConnectorAPI(t)
	def := inlineEntry("mail", map[string]any{
		"events": map[string]any{"command": "sleep 30", "protocol": "trebi-connector/1", "types": []map[string]any{{"type": "message"}}},
		"actions": map[string]any{"operations": []map[string]any{
			{"name": "search", "label": "Search mail", "readonly": true, "command": []string{"true"}},
			{"name": "label", "label": "Add a label", "command": []string{"true"}},
		}},
	})
	if code, body := do("POST", "/v1/connectors", def); code != http.StatusCreated {
		t.Fatalf("create %d %v", code, body)
	}
	access := func(md string) []string {
		code, v := do("POST", "/v1/jobs/validate", map[string]any{"name": "agents/ops/watch", "markdown": md})
		if code != http.StatusOK {
			t.Fatalf("validate %d %v", code, v)
		}
		var out []string
		for _, e := range v["issues"].([]any) {
			is := e.(map[string]any)
			if strings.HasPrefix(is["code"].(string), "trigger_access") {
				out = append(out, is["path"].(string)+"|"+is["code"].(string)+"|"+is["message"].(string))
			}
		}
		return out
	}
	got := access("---\nharness: fake\ntriggers:\n  - event: mail\n---\n\nRun.\n")
	if want := "triggers[0].event|trigger_access_read_only|Runs that mail starts can read it: Search mail. They cannot change it."; len(got) != 1 || got[0] != want {
		t.Fatalf("issues = %v", got)
	}
	if got := access("---\nharness: fake\nconnectors: [mail]\ntriggers:\n  - event: mail\n---\n\nRun.\n"); len(got) != 0 {
		t.Fatalf("named: issues = %v", got)
	}
}
