package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/tools"
)

// bearerTransport adds the daemon token to every MCP request.
type bearerTransport struct{ token string }

func (tr bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	return http.DefaultTransport.RoundTrip(req)
}

func TestMCPToolsList(t *testing.T) {
	_, base := startTestServer(t)
	cs := mcpSession(t, base)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := []string{"agent", "artifact", "assistant", "chain", "config", "connector", "conversation", "email", "event", "file",
		"harness", "inbox", "job", "mobile", "notify", "outbox", "queue", "request", "run", "secret",
		"skill", "state", "system", "workspace"}
	got := make([]string, 0, len(res.Tools))
	total := 0
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
		b, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		total += len(b)
		schema, _ := tool.InputSchema.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		action, _ := props["action"].(map[string]any)
		enum, _ := action["enum"].([]any)
		hasDocs := false
		for _, v := range enum {
			if str, ok := v.(string); ok && str == "docs" {
				hasDocs = true
			}
		}
		if len(enum) < 2 || !hasDocs {
			t.Errorf("tool %q: action enum %v lacks actions or docs", tool.Name, enum)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("tools = %v; want %v", got, want)
	}
	if total > 20*1024 {
		t.Fatalf("serialized tool definitions are %d bytes; budget is %d", total, 20*1024)
	}
}

func TestMCPJobRoundTrip(t *testing.T) {
	_, base := startTestServer(t)
	cs := mcpSession(t, base)

	res, text := mcpCall(t, cs, "workspace", "create", map[string]any{
		"path": t.TempDir(), "name": "mcp-jobs", "create": true,
	})
	wsID := fmt.Sprint(mcpOK(t, "workspace.create", res, text)["id"])

	res, text = mcpCall(t, cs, "job", "create", map[string]any{
		"workspace": wsID, "name": "mcp-roundtrip",
		"content": "---\nharness: fake\n---\n\nRound trip over MCP.\n",
	})
	mcpOK(t, "job.create", res, text)
	res, text = mcpCall(t, cs, "job", "show", map[string]any{"workspace": wsID, "name": "mcp-roundtrip"})
	job := mcpOK(t, "job.show", res, text)
	if job["name"] != "mcp-roundtrip" {
		t.Fatalf("job.show name = %v", job["name"])
	}

	// Plain HTTP sees the same job: one writer, two clients.
	code, list, _ := doJSON(t, "GET", base+"/v1/jobs", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/jobs: %d", code)
	}
	found := false
	items, _ := list["jobs"].([]any)
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && m["name"] == "mcp-roundtrip" {
			found = true
		}
	}
	if !found {
		t.Fatalf("GET /v1/jobs does not list mcp-roundtrip: %v", list)
	}

	// The tool call rides the same bus as the HTTP path.
	frames := collectBus(t, base, "job.created", busWait)
	if countTopic(frames, "job.created") < 1 {
		t.Fatalf("job.created not on the bus: %v", frames)
	}
}

func TestMCPErrorMapping(t *testing.T) {
	_, base := startTestServer(t)
	cs := mcpSession(t, base)

	res, text := mcpCall(t, cs, "run", "show", map[string]any{"id": "01MISSING"})
	if !res.IsError {
		t.Fatalf("run.show unknown id: want IsError, got %s", text)
	}
	if code := errCode(mcpJSON(t, "run.show", text)); code != "not_found" {
		t.Fatalf("run.show code = %q; want not_found (%s)", code, text)
	}

	res, text = mcpCall(t, cs, "job", "create", map[string]any{"name": "no-content"})
	if !res.IsError || errCode(mcpJSON(t, "job.create", text)) != "validation" {
		t.Fatalf("job.create without content: want validation, got %s", text)
	}

	res, text = mcpCall(t, cs, "job", "teleport", nil)
	if !res.IsError || errCode(mcpJSON(t, "job.teleport", text)) != "validation" {
		t.Fatalf("unknown action: want validation, got %s", text)
	}

	res, text = mcpCall(t, cs, "job", "create", "not-an-object")
	if !res.IsError || errCode(mcpJSON(t, "job.create", text)) != "validation" {
		t.Fatalf("input of wrong type: want validation, got %s", text)
	}
}

func TestNativeConfirmGate(t *testing.T) {
	s, base := startTestServer(t)
	code, ws, _ := doJSON(t, "POST", base+"/v1/workspaces", map[string]any{
		"path": t.TempDir(), "name": "confirm-ws", "create": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("workspace %d %v", code, ws)
	}
	wsID := fmt.Sprint(ws["id"])
	code, created, _ := doJSON(t, "POST", base+"/v1/jobs", map[string]any{
		"workspace": wsID, "name": "keep-me",
		"content": "---\nharness: fake\n---\n\nStay.\n",
	})
	if code != http.StatusCreated {
		t.Fatalf("job create %d %v", code, created)
	}
	res := tools.NewNativeRegistry(s).Call(context.Background(), "job", "delete", tools.Input{"name": "keep-me", "workspace": wsID})
	if res.Status != http.StatusBadRequest {
		t.Fatalf("status %d body %s", res.Status, res.Body)
	}
	if !strings.Contains(string(res.Body), "confirm_required") {
		t.Fatalf("want confirm_required, got %s", res.Body)
	}
	code, body, _ := doJSON(t, "GET", base+"/v1/jobs/keep-me?workspace="+wsID, nil)
	if code != http.StatusOK {
		t.Fatalf("job deleted without confirm: %d %v", code, body)
	}
}

// TestMCPOpenWithoutCredentials pins the open local endpoint: a plain POST
// initializes a session with no credential at all.
func TestMCPOpenWithoutCredentials(t *testing.T) {
	_, base := startTestServer(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	req, _ := http.NewRequest("POST", base+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /mcp without credentials: status %d; want 200", resp.StatusCode)
	}
	var out struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if out.Result.ServerInfo.Name != "trebi" {
		t.Fatalf("initialize result: %+v", out.Result)
	}
}

func TestMCPFilePlane(t *testing.T) {
	_, base := startTestServer(t)
	cs := mcpSession(t, base)

	res, text := mcpCall(t, cs, "workspace", "create", map[string]any{
		"path": t.TempDir(), "name": "mcp-files", "create": true,
	})
	ws := mcpOK(t, "workspace.create", res, text)
	wsID := fmt.Sprint(ws["id"])

	put := map[string]any{
		"owner": "workspace", "workspace": wsID, "path": "notes/a.md", "content": "v1\n",
	}
	res, text = mcpCall(t, cs, "file", "put", put)
	mcpOK(t, "file.put", res, text)

	stale := map[string]any{
		"owner": "workspace", "workspace": wsID, "path": "notes/a.md",
		"content": "v2\n", "if_match": "stale",
	}
	res, text = mcpCall(t, cs, "file", "put", stale)
	if !res.IsError || errCode(mcpJSON(t, "file.put", text)) != "precondition_failed" {
		t.Fatalf("stale if_match: want precondition_failed, got %s", text)
	}

	res, text = mcpCall(t, cs, "file", "get", map[string]any{
		"owner": "workspace", "workspace": wsID, "path": "notes/a.md",
	})
	if res.IsError {
		t.Fatalf("file.get: %s", text)
	}
	if !strings.Contains(text, "v1") || strings.Contains(text, "v2") {
		t.Fatalf("stale put leaked through; file content: %q", text)
	}
}

func TestMCPBusPoll(t *testing.T) {
	_, base := startTestServer(t)
	cs := mcpSession(t, base)

	res, text := mcpCall(t, cs, "workspace", "create", map[string]any{
		"path": t.TempDir(), "name": "mcp-poll", "create": true,
	})
	wsID := fmt.Sprint(mcpOK(t, "workspace.create", res, text)["id"])
	mustCreate := func(name string) {
		res, text := mcpCall(t, cs, "job", "create", map[string]any{
			"workspace": wsID, "name": name,
			"content": "---\nharness: fake\n---\n\n" + name + "\n",
		})
		mcpOK(t, "job.create "+name, res, text)
	}
	mustCreate("poll-one")

	res, text = mcpCall(t, cs, "system", "bus_poll", map[string]any{"topics": "job.created"})
	m := mcpOK(t, "bus_poll", res, text)
	events, _ := m["events"].([]any)
	cursor, _ := m["next_cursor"].(float64)
	if len(events) == 0 {
		t.Fatalf("bus_poll returned no job.created events: %s", text)
	}
	if cursor <= 0 {
		t.Fatalf("bus_poll next_cursor = %v; want > 0", m["next_cursor"])
	}

	// The cursor advances: a later poll returns only newer frames.
	mustCreate("poll-two")
	res, text = mcpCall(t, cs, "system", "bus_poll", map[string]any{
		"topics": "job.created", "cursor": cursor,
	})
	m = mcpOK(t, "bus_poll with cursor", res, text)
	events, _ = m["events"].([]any)
	if len(events) == 0 {
		t.Fatalf("poll after cursor returned no events: %s", text)
	}
	for _, ev := range events {
		frame, ok := ev.(map[string]any)
		if !ok {
			t.Fatalf("event is not an object: %v", ev)
		}
		if seq, _ := frame["seq"].(float64); seq <= cursor {
			t.Fatalf("event seq %v is not after cursor %v: %v", seq, cursor, frame)
		}
		if frame["topic"] != "job.created" {
			t.Fatalf("topic filter leaked: %v", frame)
		}
	}
}

func TestMCPDrainUndrain(t *testing.T) {
	_, base := startTestServer(t)
	cs := mcpSession(t, base)

	drainState := func() bool {
		res, text := mcpCall(t, cs, "system", "health", nil)
		m := mcpOK(t, "system.health", res, text)
		ds, _ := m["drain_state"].(map[string]any)
		return ds["draining"] == true
	}

	res, text := mcpCall(t, cs, "system", "drain", nil)
	mcpOK(t, "system.drain", res, text)
	if !drainState() {
		t.Fatal("health does not report draining after system.drain")
	}

	res, text = mcpCall(t, cs, "system", "undrain", nil)
	mcpOK(t, "system.undrain", res, text)
	if drainState() {
		t.Fatal("health still reports draining after system.undrain")
	}
}

func TestGuidesHTTP(t *testing.T) {
	_, base := startTestServer(t)

	code, body, _ := doJSON(t, "GET", base+"/v1/system/guides", nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d %v", code, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 14 {
		t.Fatalf("list items %d; want 14", len(items))
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/system/guides/getting-started", nil)
	if code != http.StatusOK {
		t.Fatalf("get: %d %v", code, body)
	}
	if body["topic"] != "getting-started" || body["body"] == nil || body["body"] == "" {
		t.Fatalf("get body: %v", body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/system/guides/no-such-topic", nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown: %d", code)
	}
	if errCode(body) != "not_found" {
		t.Fatalf("unknown code %v", body)
	}
}

func TestMCPGuides(t *testing.T) {
	_, base := startTestServer(t)
	cs := mcpSession(t, base)

	res, text := mcpCall(t, cs, "system", "guides", nil)
	m := mcpOK(t, "system.guides", res, text)
	items, _ := m["items"].([]any)
	if len(items) != 14 {
		t.Fatalf("guides items %d", len(items))
	}

	res, text = mcpCall(t, cs, "system", "guide", map[string]any{"topic": "getting-started"})
	got := mcpOK(t, "system.guide", res, text)
	if got["topic"] != "getting-started" || fmt.Sprint(got["body"]) == "" {
		t.Fatalf("guide: %v", got)
	}

	res, text = mcpCall(t, cs, "system", "guide", map[string]any{"topic": "nope"})
	if !res.IsError {
		t.Fatalf("bogus topic: want IsError, got %s", text)
	}
	if code := errCode(mcpJSON(t, "system.guide", text)); code != "not_found" {
		t.Fatalf("bogus topic code = %q (%s)", code, text)
	}

	ir := cs.InitializeResult()
	if ir == nil || !strings.Contains(ir.Instructions, "mcp-basics") {
		t.Fatalf("initialize instructions = %v", ir)
	}
}

func TestMCPDocsAction(t *testing.T) {
	_, base := startTestServer(t)
	cs := mcpSession(t, base)
	tools := []string{"assistant", "config", "conversation", "event", "file",
		"harness", "inbox", "connector", "job", "queue", "run", "secret",
		"skill", "state", "system", "workspace"}
	for _, tool := range tools {
		res, text := mcpCall(t, cs, tool, "docs", nil)
		if res.IsError {
			t.Errorf("%s.docs: %s", tool, text)
			continue
		}
		if !strings.Contains(text, "## Actions") {
			t.Errorf("%s.docs: no action reference in %q", tool, text)
		}
	}
}

// TestMCPTraversalIsError pins that a file.put whose path escapes the jail
// surfaces as an error. It used to answer 307 with an empty body, which the
// MCP layer reported as a successful write that never happened.
func TestMCPTraversalIsError(t *testing.T) {
	_, base := startTestServer(t)
	cs := mcpSession(t, base)

	res, text := mcpCall(t, cs, "workspace", "create", map[string]any{
		"path": t.TempDir(), "name": "mcp-traversal", "create": true,
	})
	ws := mcpOK(t, "workspace.create", res, text)
	wsID := fmt.Sprint(ws["id"])

	for _, p := range []string{"../notes.txt", "a/../../notes.txt", "./notes.txt", "/tmp/notes.txt"} {
		res, text = mcpCall(t, cs, "file", "put", map[string]any{
			"owner": "workspace", "workspace": wsID, "path": p, "content": "SHOULD NOT LAND\n",
		})
		if !res.IsError {
			t.Errorf("file.put %q: want IsError, got %s", p, text)
		}
		if strings.TrimSpace(text) == "" {
			t.Errorf("file.put %q: empty result body", p)
		}
	}
}

// TestMCPWorkspacePathlessAdd pins that `create` + `name` mints a workspace
// without a path, matching `trebi workspace add --create --name`.
func TestMCPWorkspacePathlessCreate(t *testing.T) {
	root := t.TempDir()
	_, base := startTestServerOpts(t, api.Options{WorkspacesRoot: root})
	cs := mcpSession(t, base)

	res, text := mcpCall(t, cs, "workspace", "create", map[string]any{
		"name": "ops", "mkdir": true,
	})
	ws := mcpOK(t, "workspace.create", res, text)
	if fmt.Sprint(ws["name"]) != "ops" {
		t.Fatalf("name = %v, want ops", ws["name"])
	}
	if fmt.Sprint(ws["path"]) == "" {
		t.Fatal("a pathless create returned no minted path")
	}

	// Still an error with neither path nor a name to mint from.
	res, text = mcpCall(t, cs, "workspace", "create", map[string]any{"mkdir": true})
	if !res.IsError {
		t.Fatalf("nameless pathless create: want error, got %s", text)
	}
}

// mcpSession connects one SDK client to the test server's /mcp endpoint.
func mcpSession(t *testing.T, base string) *mcp.ClientSession {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcp-test", Version: "1"}, nil).Connect(
		context.Background(),
		&mcp.StreamableClientTransport{
			Endpoint:             base + "/mcp",
			DisableStandaloneSSE: true,
		}, nil)
	if err != nil {
		t.Fatalf("connect /mcp: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// mcpCall runs one tool action and returns the result plus its text content.
func mcpCall(t *testing.T, cs *mcp.ClientSession, tool, action string, input any) (*mcp.CallToolResult, string) {
	t.Helper()
	args := map[string]any{"action": action}
	if input != nil {
		args["input"] = input
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s.%s: call: %v", tool, action, err)
	}
	return res, mcpResultText(res)
}

func mcpResultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// mcpJSON decodes one JSON tool result.
func mcpJSON(t *testing.T, what, text string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("%s: decode result %q: %v", what, text, err)
	}
	return m
}

// mcpOK fails unless the result is a success envelope, returning its JSON.
func mcpOK(t *testing.T, what string, res *mcp.CallToolResult, text string) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("%s: unexpected error: %s", what, text)
	}
	return mcpJSON(t, what, text)
}
