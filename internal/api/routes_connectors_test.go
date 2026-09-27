package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestConnectorRoutes covers the write surface of Part E: create conflict,
// replace, patch removal, strict decode, and the lifecycle guard on a
// connector without an events block.
func TestConnectorRoutes(t *testing.T) {
	do := startConnectorAPI(t, "")

	def := map[string]any{
		"name": "feed",
		"events": map[string]any{
			"command": "echo one",
			"format":  "json",
			"id":      "id",
		},
		"env": map[string]string{"MODE": "ro"},
	}
	if code, body := do("POST", "/v1/connectors", def); code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, body)
	}

	// A second create of the same name is a conflict.
	if code, body := do("POST", "/v1/connectors", def); code != http.StatusConflict || errCode(body) != "conflict" {
		t.Fatalf("duplicate create = %d %v", code, body)
	}

	// A test event with a stored source id is a conflict, not a silent drop.
	if code, body := do("POST", "/v1/connectors/feed/test", map[string]any{"event": map[string]any{"id": "t1"}}); code != http.StatusOK {
		t.Fatalf("test event = %d %v", code, body)
	}
	if code, body := do("POST", "/v1/connectors/feed/test", map[string]any{"event": map[string]any{"id": "t1"}}); code != http.StatusConflict || errCode(body) != "conflict" {
		t.Fatalf("duplicate test event = %d %v", code, body)
	}
	// Retrigger reuses the stored row, so the duplicate source id is no bar.
	code, list := do("GET", "/v1/events?connector=feed", nil)
	events, _ := list["events"].([]any)
	if code != http.StatusOK || len(events) != 1 {
		t.Fatalf("events = %d %v", code, list)
	}
	eventID, _ := events[0].(map[string]any)["id"].(string)
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
	// An unknown key inside a PATCH block fails the strict decode.
	if code, body := do("PATCH", "/v1/connectors/feed", map[string]any{"events": map[string]any{"command": "x", "formt": "json"}}); code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("patch block unknown key = %d %v", code, body)
	}

	// An unknown top-level key fails the strict decode.
	bad := map[string]any{"name": "feed2", "nonsense": true}
	if code, body := do("POST", "/v1/connectors", bad); code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("unknown field = %d %v", code, body)
	}

	// A replace swaps the whole definition.
	replaced := map[string]any{
		"events": map[string]any{"command": "echo two", "format": "json", "id": "id"},
		"env":    map[string]string{"MODE": "rw"},
	}
	if code, body := do("PUT", "/v1/connectors/feed", replaced); code != http.StatusOK {
		t.Fatalf("replace = %d %v", code, body)
	}
	if row := getConnector(t, do, "feed"); row["env"] == nil {
		t.Fatalf("replace dropped env: %v", row)
	}

	// A patch replaces a whole block, and a JSON null removes it.
	if code, body := do("PATCH", "/v1/connectors/feed", map[string]any{"env": map[string]string{"EXTRA": "1"}}); code != http.StatusOK {
		t.Fatalf("patch = %d %v", code, body)
	}
	row := getConnector(t, do, "feed")
	env, _ := row["env"].(map[string]any)
	if _, kept := env["MODE"]; kept || env["EXTRA"] != "***" {
		t.Fatalf("patch env = %v; want a whole-block replace", env)
	}
	if code, body := do("PATCH", "/v1/connectors/feed", map[string]any{"nope": 1}); code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("patch unknown field = %d %v", code, body)
	}
	// Null removal has to leave a valid definition, so the same patch adds
	// the action block that replaces events.
	swap := map[string]any{
		"events":  nil,
		"actions": map[string]any{"mcp": map[string]any{"command": "npx -y demo"}},
	}
	if code, body := do("PATCH", "/v1/connectors/feed", swap); code != http.StatusOK {
		t.Fatalf("patch null events = %d %v", code, body)
	}
	row = getConnector(t, do, "feed")
	if row["events"] != nil {
		t.Fatalf("null events kept the block: %v", row["events"])
	}
	if acts, _ := row["actions"].(map[string]any); acts == nil {
		t.Fatalf("patch dropped actions: %v", row)
	}

	// A connector without events refuses the lifecycle routes with 409.
	cliOnly := map[string]any{"name": "cli-only", "actions": map[string]any{"mcp": map[string]any{"command": "npx"}}}
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
}

// TestConnectorHeaderMaskRoundTrip proves the masked GET row can go back on
// PATCH actions_merge and on PUT without a loss of the stored header.
func TestConnectorHeaderMaskRoundTrip(t *testing.T) {
	do := startConnectorAPI(t, "")
	def := map[string]any{"name": "api", "actions": map[string]any{"mcp": map[string]any{
		"url": "https://mcp.example.com", "headers": map[string]string{"X-Key": "abc"},
	}}}
	if code, body := do("POST", "/v1/connectors", def); code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, body)
	}
	stored := func() string {
		t.Helper()
		b, err := os.ReadFile(home.ConnectorsPath())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	mcp := getConnector(t, do, "api")["actions"].(map[string]any)["mcp"].(map[string]any)
	if mcp["headers"].(map[string]any)["X-Key"] != "***" {
		t.Fatalf("GET headers = %v", mcp["headers"])
	}
	merge := map[string]any{"actions_merge": map[string]any{"mcp": map[string]any{"url": "https://mcp2.example.com"}}}
	if code, body := do("PATCH", "/v1/connectors/api", merge); code != http.StatusOK {
		t.Fatalf("merge = %d %v", code, body)
	}
	if !strings.Contains(stored(), "X-Key: abc") || !strings.Contains(stored(), "mcp2.example.com") {
		t.Fatalf("merge lost the header:\n%s", stored())
	}
	put := map[string]any{"actions": map[string]any{"mcp": map[string]any{
		"url": "https://mcp.example.com", "headers": map[string]string{"X-Key": "***"},
	}}}
	if code, body := do("PUT", "/v1/connectors/api", put); code != http.StatusOK {
		t.Fatalf("put = %d %v", code, body)
	}
	if !strings.Contains(stored(), "X-Key: abc") {
		t.Fatalf("put lost the header:\n%s", stored())
	}
	put["actions"].(map[string]any)["mcp"].(map[string]any)["headers"] = map[string]string{"New": "***"}
	if code, body := do("PUT", "/v1/connectors/api", put); code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("mask without value = %d %v", code, body)
	}
}

// TestConnectorPatchKeepsCursor proves a patch restarts the supervisor but
// leaves the cursor row alone.
func TestConnectorPatchKeepsEvents(t *testing.T) {
	do := startConnectorAPI(t, "")

	def := map[string]any{"name": "keep", "events": map[string]any{"command": "sleep 30", "format": "json", "id": "id"}}
	if code, body := do("POST", "/v1/connectors", def); code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, body)
	}
	if code, body := do("PUT", "/v1/connectors/keep/cursor", map[string]string{"last_event_id": "ev-7"}); code != http.StatusOK {
		t.Fatalf("cursor put = %d %v", code, body)
	}
	if code, body := do("PATCH", "/v1/connectors/keep", map[string]any{"env": map[string]string{"A": "1"}}); code != http.StatusOK {
		t.Fatalf("patch = %d %v", code, body)
	}
	code, cur := do("GET", "/v1/connectors/keep/cursor", nil)
	if code != http.StatusOK {
		t.Fatalf("cursor get = %d %v", code, cur)
	}
	if cur["last_event_id"] != "ev-7" {
		t.Fatalf("patch moved the cursor: %v", cur)
	}
}

// TestConnectorWriteAfterFailedLoad proves a connectors.yaml that does not
// load blocks every write: the file stays as the user left it.
func TestConnectorWriteAfterFailedLoad(t *testing.T) {
	body := "mail:\n  type: stream\n  events: {command: x}\n"
	do := startConnectorAPI(t, body)
	def := map[string]any{"name": "feed", "events": map[string]any{"command": "echo"}}
	if code, resp := do("POST", "/v1/connectors", def); code != http.StatusConflict || errCode(resp) != "conflict" {
		t.Fatalf("create after failed load = %d %v", code, resp)
	}
	if code, resp := do("DELETE", "/v1/connectors/mail?confirm=1", nil); code != http.StatusConflict {
		t.Fatalf("delete after failed load = %d %v", code, resp)
	}
	got, err := os.ReadFile(home.ConnectorsPath())
	if err != nil || string(got) != body {
		t.Fatalf("file changed: %q %v", got, err)
	}
	code, list := do("GET", "/v1/connectors", nil)
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(list["load_error"]), "type") {
		t.Fatalf("list load_error = %d %v", code, list)
	}
}

// startConnectorAPI serves the API over a fresh home. A non-empty
// connectorsYAML is written before the server starts.
func startConnectorAPI(t *testing.T, connectorsYAML string) func(method, path string, body any) (int, map[string]any) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	if connectorsYAML != "" {
		if err := os.WriteFile(home.ConnectorsPath(), []byte(connectorsYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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
