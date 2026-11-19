package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/trebi-ai/trebi/internal/request"
)

// agentTestServer answers the workspace, agents, and jobs routes the agent
// CLI touches, and records the requests it saw.
type agentTestServer struct {
	srv       *httptest.Server
	mu        sync.Mutex
	seen      []request.HTTPRequest
	jobsHits  int
	agentsHit map[string]int
	forQuery  string
	query     string
}

func (a *agentTestServer) client() *APIClient {
	return &APIClient{base: a.srv.URL, httpClient: a.srv.Client()}
}

func (a *agentTestServer) last() request.HTTPRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.seen) == 0 {
		return request.HTTPRequest{}
	}
	return a.seen[len(a.seen)-1]
}

// TestAgentListCLI: `agent list` fans out workspaces, honors --workspace,
// and answers an "agents" envelope.
func TestAgentListCLI(t *testing.T) {
	a := newAgentTestServer(t)
	client := a.client()

	out, err := runCLI(t, client, "agent", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if a.jobsHits != 0 {
		t.Fatal("agent list must not hit /v1/jobs")
	}
	var env struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&env); err != nil {
		t.Fatalf("json %v %q", err, out)
	}
	if len(env.Agents) != 1 || env.Agents[0]["name"] != "cmo" || env.Agents[0]["kind"] != "agent" {
		t.Fatalf("agents envelope %q", out)
	}

	out, err = runCLI(t, client, "agent", "list", "--workspace", "two")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "cmo") {
		t.Fatalf("--workspace two leaked ws1 agents: %q", out)
	}
}

// TestAgentListDescription: the table shows the description, and --for goes
// to the route as the for query.
func TestAgentListDescription(t *testing.T) {
	a := newAgentTestServer(t)
	out, err := runCLI(t, a.client(), "agent", "list", "--for", "growth plans")
	if err != nil {
		t.Fatal(err)
	}
	header := strings.Fields(strings.ReplaceAll(strings.Split(out, "\n")[1], "│", " "))
	if strings.Join(header, " ") != "NAME DESCRIPTION WORKSPACE HARNESS ACTIVE JOBS" {
		t.Fatalf("header %q", out)
	}
	if !strings.Contains(out, "Growth plans and campaigns.") {
		t.Fatalf("no description: %q", out)
	}
	if a.forQuery != "growth plans" {
		t.Fatalf("for query = %q", a.forQuery)
	}
}

// TestJobListKindAgent: `job list --kind agent` fans out like the assistant
// kind and never hits /v1/jobs; the envelope stays "jobs".
func TestJobListKindAgent(t *testing.T) {
	a := newAgentTestServer(t)
	client := a.client()

	out, err := runCLI(t, client, "job", "list", "--kind", "agent", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if a.jobsHits != 0 {
		t.Fatal("--kind agent must not hit /v1/jobs")
	}
	if a.agentsHit["/v1/workspaces/ws1/agents"] == 0 {
		t.Fatal("--kind agent should fan out to the agents routes")
	}
	var env struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&env); err != nil {
		t.Fatalf("json %v %q", err, out)
	}
	if len(env.Jobs) != 1 || env.Jobs[0]["name"] != "cmo" {
		t.Fatalf("jobs envelope %q", out)
	}

	if _, err := runCLI(t, client, "job", "list", "--kind", "bogus"); err == nil {
		t.Fatal("bogus kind")
	}
}

// TestAgentCreateCLI: create is a PUT upsert; flags ride the body, and a bare
// create still puts (the daemon seeds AGENT.md).
func TestAgentCreateCLI(t *testing.T) {
	a := newAgentTestServer(t)
	client := a.client()

	if _, err := runCLI(t, client, "agent", "create", "cmo", "--workspace", "ws1", "--prompt", "Run growth."); err != nil {
		t.Fatal(err)
	}
	req := a.last()
	if req.Method != "PUT" || req.Path != "/v1/workspaces/ws1/agents/cmo" {
		t.Fatalf("create request %+v", req)
	}
	body, _ := req.Body.(map[string]any)
	if body["prompt"] != "Run growth." {
		t.Fatalf("create body %v", body)
	}

	if _, err := runCLI(t, client, "agent", "create", "scout", "--workspace", "ws1"); err != nil {
		t.Fatal(err)
	}
	req = a.last()
	if req.Method != "PUT" || req.Path != "/v1/workspaces/ws1/agents/scout" {
		t.Fatalf("bare create request %+v", req)
	}
}

// TestJobCreateAgentFlag: --agent rides the POST /v1/jobs body.
func TestJobCreateAgentFlag(t *testing.T) {
	a := newAgentTestServer(t)
	client := a.client()

	if _, err := runCLI(t, client, "job", "create", "weekly-digest", "--workspace", "ws1",
		"--prompt", "Write it.", "--agent", "cmo"); err != nil {
		t.Fatal(err)
	}
	req := a.last()
	if req.Method != "POST" || req.Path != "/v1/jobs" {
		t.Fatalf("job create request %+v", req)
	}
	body, _ := req.Body.(map[string]any)
	if body["agent"] != "cmo" {
		t.Fatalf("agent field missing: %v", body)
	}

	// A job always belongs to an agent: no --agent and a bare name fails
	// before a request.
	n := len(a.seen)
	if _, err := runCLI(t, client, "job", "create", "solo", "--workspace", "ws1", "--prompt", "x"); err == nil || !strings.Contains(err.Error(), "--agent") {
		t.Fatalf("create without --agent: err=%v", err)
	}
	if len(a.seen) != n {
		t.Fatalf("create without --agent sent a request: %+v", a.last())
	}

	// A full agents/<name>/ path needs no --agent.
	if _, err := runCLI(t, client, "job", "create", "cmo/solo", "--workspace", "ws1", "--prompt", "x"); err != nil {
		t.Fatal(err)
	}
	if req := a.last(); req.Method != "POST" || req.Path != "/v1/jobs" {
		t.Fatalf("create with a full name: %+v", req)
	}
}

// TestJobMoveNeedsAgent: an empty --agent and a move with no target fail
// before a request.
func TestJobMoveNeedsAgent(t *testing.T) {
	a := newAgentTestServer(t)
	client := a.client()
	for _, args := range [][]string{
		{"job", "move", "cmo/x", "--workspace", "ws1", "--agent", ""},
		{"job", "move", "cmo/x", "--workspace", "ws1"},
	} {
		if _, err := runCLI(t, client, args...); err == nil {
			t.Fatalf("%v: want an error", args)
		}
	}
	if len(a.seen) != 0 {
		t.Fatalf("a refused move sent a request: %+v", a.seen)
	}
	if _, err := runCLI(t, client, "job", "move", "cmo/x", "--workspace", "ws1", "--agent", "ops"); err != nil {
		t.Fatal(err)
	}
	body, _ := a.last().Body.(map[string]any)
	if body["agent"] != "ops" {
		t.Fatalf("move body: %v", body)
	}
}

// TestAgentDeleteMoveJobsTo: --move-jobs-to rides the query, and it does
// not go with --purge.
func TestAgentDeleteMoveJobsTo(t *testing.T) {
	a := newAgentTestServer(t)
	client := a.client()
	if _, err := runCLI(t, client, "agent", "delete", "cmo", "--workspace", "ws1", "--force", "--move-jobs-to", "ops"); err != nil {
		t.Fatal(err)
	}
	req := a.last()
	q, _ := url.ParseQuery(a.query)
	if req.Method != "DELETE" || req.Path != "/v1/workspaces/ws1/agents/cmo" || q.Get("move_jobs_to") != "ops" || q.Get("purge") != "" {
		t.Fatalf("delete request %+v query %q", req, a.query)
	}
	n := len(a.seen)
	if _, err := runCLI(t, client, "agent", "delete", "cmo", "--workspace", "ws1", "--force", "--move-jobs-to", "ops", "--purge"); err == nil {
		t.Fatal("--move-jobs-to with --purge: want an error")
	}
	if len(a.seen) != n {
		t.Fatalf("a refused delete sent a request: %+v", a.last())
	}
}

// TestAgentWakeCLI: wake posts to the agent sugar route.
func TestAgentWakeCLI(t *testing.T) {
	a := newAgentTestServer(t)
	client := a.client()

	out, err := runCLI(t, client, "agent", "wake", "cmo", "--workspace", "ws1", "--test")
	if err != nil {
		t.Fatal(err)
	}
	req := a.last()
	if req.Method != "POST" || req.Path != "/v1/workspaces/ws1/agents/cmo/wake" {
		t.Fatalf("wake request %+v", req)
	}
	if !strings.Contains(out, "01ABC") {
		t.Fatalf("dry-run output %q", out)
	}
}

func newAgentTestServer(t *testing.T) *agentTestServer {
	t.Helper()
	// An empty TREBI_HOME keeps the machine's sticky/linked workspaces out of
	// workspace resolution, so --workspace names map only against the fake.
	t.Setenv("TREBI_HOME", t.TempDir())
	a := &agentTestServer{agentsHit: map[string]int{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		a.mu.Lock()
		a.seen = append(a.seen, request.HTTPRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		a.query = r.URL.RawQuery
		a.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/jobs" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "cmo/weekly-digest"})
		case r.URL.Path == "/v1/jobs":
			a.mu.Lock()
			a.jobsHits++
			a.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []any{}})
		case r.URL.Path == "/v1/workspaces":
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": []any{
				map[string]any{"id": "ws1", "name": "one"},
				map[string]any{"id": "ws2", "name": "two"},
			}})
		case strings.HasSuffix(r.URL.Path, "/agents"):
			a.mu.Lock()
			a.agentsHit[r.URL.Path]++
			a.forQuery = r.URL.Query().Get("for")
			a.mu.Unlock()
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/workspaces/"), "/agents")
			if id == "ws2" {
				_ = json.NewEncoder(w).Encode(map[string]any{"agents": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []any{map[string]any{
				"name": "cmo", "index_name": "cmo", "workspace_id": "ws1",
				"harness": "claude", "active": true, "memory": true, "job_count": 2,
				"description": "Growth plans and campaigns.",
			}}})
		case strings.HasSuffix(r.URL.Path, "/wake"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "01ABC", "status": "running", "kind": "agent"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func TestAgentArchiveCommand(t *testing.T) {
	var method, path, query, ifMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/workspaces" {
			_, _ = io.WriteString(w, `{"workspaces":[{"id":"ws1","name":"ws1"}]}`)
			return
		}
		method, path, query, ifMatch = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("If-Match")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"agents":[{"name":"old","workspace_id":"ws1","archived_at":"2026-11-01T10:00:00Z"}],"counts":{"current":0,"archived":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"name":"old","archived_at":"2026-11-01T10:00:00Z","jobs":2}`)
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "agent", "archive", "old", "-w", "ws1", "--if-match", "abc")
	if err != nil || method != "POST" || path != "/v1/workspaces/ws1/agents/old/archive" || ifMatch != "abc" {
		t.Fatalf("archive: %v %s %s %q\n%s", err, method, path, ifMatch, out)
	}
	if !strings.Contains(out, "archived old") {
		t.Fatalf("out=%q", out)
	}
	out, err = runCLI(t, client, "agent", "unarchive", "old", "-w", "ws1")
	if err != nil || path != "/v1/workspaces/ws1/agents/old/unarchive" || !strings.Contains(out, "unarchived old") {
		t.Fatalf("unarchive: %v %s\n%s", err, path, out)
	}
	if _, err = runCLI(t, client, "agent", "list", "-w", "ws1", "--archived"); err != nil || !strings.Contains(query, "archived=true") {
		t.Fatalf("list --archived: %v %s", err, query)
	}
	if _, err = runCLI(t, client, "agent", "list", "-w", "ws1", "--archived", "--include-archived"); err == nil {
		t.Fatal("both flags: want usage error")
	}
}
