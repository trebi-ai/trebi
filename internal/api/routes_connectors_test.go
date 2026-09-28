package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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

// inlineEntry is one inline connector entry; blocks go into the manifest.
func inlineEntry(name string, blocks map[string]any) map[string]any {
	m := map[string]any{"schema": "trebi-connector/1", "name": name, "title": name, "description": "Test connector", "version": "1.0.0"}
	maps.Copy(m, blocks)
	return map[string]any{"name": name, "from": "inline", "manifest": m}
}

// TestConnectorRoutes covers the entry write surface: create conflict,
// replace, patch layers, strict decode, and the lifecycle guard on a
// connector without an events block.
func TestConnectorRoutes(t *testing.T) {
	do := startConnectorAPI(t, "")

	def := inlineEntry("feed", map[string]any{
		"setup":  map[string]any{"inputs": []map[string]any{{"name": "MODE"}}},
		"events": map[string]any{"command": "echo one", "format": "json", "id": "id"},
	})
	def["inputs"] = map[string]string{"MODE": "ro"}
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
	// An unknown key inside a PATCH manifest fails the strict decode.
	badManifest := inlineEntry("feed", map[string]any{"events": map[string]any{"command": "x", "formt": "json"}})["manifest"]
	if code, body := do("PATCH", "/v1/connectors/feed", map[string]any{"manifest": badManifest}); code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("patch manifest unknown key = %d %v", code, body)
	}

	// An unknown top-level key fails the strict decode.
	bad := map[string]any{"name": "feed2", "nonsense": true}
	if code, body := do("POST", "/v1/connectors", bad); code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("unknown field = %d %v", code, body)
	}

	// A replace swaps the whole entry.
	replaced := inlineEntry("feed", map[string]any{
		"setup":  map[string]any{"inputs": []map[string]any{{"name": "MODE"}}},
		"events": map[string]any{"command": "echo two", "format": "json", "id": "id"},
	})
	replaced["inputs"] = map[string]string{"MODE": "rw"}
	if code, body := do("PUT", "/v1/connectors/feed", replaced); code != http.StatusOK {
		t.Fatalf("replace = %d %v", code, body)
	}
	if row := getConnector(t, do, "feed"); row["inputs"].(map[string]any)["MODE"] != "***" {
		t.Fatalf("replace inputs: %v", row["inputs"])
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
	if row := getConnector(t, do, "feed"); row["description"] != "Patched" {
		t.Fatalf("patch layer not applied: %v", row["description"])
	}
	if code, body := do("PATCH", "/v1/connectors/feed", map[string]any{"patch": nil}); code != http.StatusOK {
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

	// A connector without events refuses the lifecycle routes with 409.
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
}

// TestConnectorInputMaskRoundTrip proves the masked GET inputs can go back
// on PATCH and on PUT without a loss of the stored value.
func TestConnectorInputMaskRoundTrip(t *testing.T) {
	do := startConnectorAPI(t, "")
	def := inlineEntry("api", map[string]any{
		"setup": map[string]any{"inputs": []map[string]any{{"name": "API_KEY"}}},
		"actions": map[string]any{"mcp": map[string]any{
			"url": "https://mcp.example.com", "headers": map[string]string{"X-Key": "${API_KEY}"},
		}},
	})
	def["inputs"] = map[string]string{"API_KEY": "abc"}
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
	if in := getConnector(t, do, "api")["inputs"].(map[string]any); in["API_KEY"] != "***" {
		t.Fatalf("GET inputs = %v", in)
	}
	if code, body := do("PATCH", "/v1/connectors/api", map[string]any{"inputs": map[string]string{"API_KEY": "***"}}); code != http.StatusOK {
		t.Fatalf("patch = %d %v", code, body)
	}
	if !strings.Contains(stored(), "API_KEY: abc") {
		t.Fatalf("patch lost the input:\n%s", stored())
	}
	def["inputs"] = map[string]string{"API_KEY": "***"}
	if code, body := do("PUT", "/v1/connectors/api", def); code != http.StatusOK {
		t.Fatalf("put = %d %v", code, body)
	}
	if !strings.Contains(stored(), "API_KEY: abc") {
		t.Fatalf("put lost the input:\n%s", stored())
	}
	def["inputs"] = map[string]string{"NEW": "***"}
	if code, body := do("PUT", "/v1/connectors/api", def); code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("mask without value = %d %v", code, body)
	}
}

// TestConnectorPatchKeepsEvents proves a patch restarts the supervisor but
// leaves the cursor row alone.
func TestConnectorPatchKeepsEvents(t *testing.T) {
	do := startConnectorAPI(t, "")

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
	if cur["last_event_id"] != "ev-7" {
		t.Fatalf("patch moved the cursor: %v", cur)
	}
}

// TestConnectorWriteAfterFailedLoad proves a connectors.yaml that does not
// load blocks every write: the file stays as the user left it.
func TestConnectorWriteAfterFailedLoad(t *testing.T) {
	body := "mail:\n  type: stream\n  events: {command: x}\n"
	do := startConnectorAPI(t, body)
	def := inlineEntry("feed", map[string]any{"events": map[string]any{"command": "echo"}})
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
