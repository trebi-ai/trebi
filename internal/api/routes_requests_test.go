package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/entitle"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
)

// recDriver runs the fake driver and keeps every StartRequest.
type recDriver struct {
	agentwire.FakeDriver
	mu   sync.Mutex
	reqs []agentwire.StartRequest
}

func (d *recDriver) Start(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	d.add(req)
	return d.FakeDriver.Start(ctx, req)
}

func (d *recDriver) Resume(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	d.add(req)
	return d.FakeDriver.Resume(ctx, req)
}

func (d *recDriver) add(req agentwire.StartRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reqs = append(d.reqs, req)
}

func (d *recDriver) all() []agentwire.StartRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]agentwire.StartRequest(nil), d.reqs...)
}

// requestStack is one daemon whose cloud plan grants requests, and a job whose
// fake harness writes its run token and its prompts, and ends its
// turn once the test creates the file "go". The harness also ends when
// the test removes its directory or after 60s, so it never outlives the test.
type requestStack struct {
	base string
	dir  string
	drv  *recDriver
	wsID string
	srv  *api.Server
}

func newRequestStack(t *testing.T, features ...string) *requestStack {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if features == nil {
		features = []string{string(entitle.Requests)}
	}
	s, base := startTestServerSeeded(t, api.Options{}, func(st *store.Store) { grantCloud(t, st, features) })
	t.Setenv("TREBI_FAKE_HARNESS", "")    // the CLI driver below runs the job
	t.Setenv("ANTHROPIC_API_KEY", "test") // passes the auth probe; recDriver is not a bare FakeDriver
	st := &requestStack{base: base, dir: t.TempDir(), srv: s}
	script := fmt.Sprintf(`read -r line; printf '%%s\n' "$line" >> %[1]s/prompts; printf '%%s' "$TREBI_RUN_TOKEN" > %[1]s/token.tmp; mv %[1]s/token.tmp %[1]s/token
echo '{"type":"init","session_id":"sess-1"}'
n=0; while [ ! -f %[1]s/go ] && [ -d %[1]s ] && [ $n -lt 1200 ]; do n=$((n+1)); sleep 0.05; done
echo '{"type":"result","subtype":"success","text":"done"}'`, st.dir)
	st.drv = &recDriver{FakeDriver: agentwire.FakeDriver{Script: script}}
	s.SetCLIDriver(st.drv)
	wsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsDir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "demo", "JOB.md"), []byte("---\nharness: claude\n---\n\nDo work.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st.wsID = addWorkspace(t, base, wsDir, "req-ws")
	return st
}

// launch starts the job and returns the run id and its token.
func (s *requestStack) launch(t *testing.T) (runID, token string) {
	t.Helper()
	code, run, _ := doJSON(t, "POST", s.base+"/v1/jobs/demo/run", map[string]any{"workspace": s.wsID})
	if code != 201 {
		t.Fatalf("launch %d %v", code, run)
	}
	runID, _ = run["id"].(string)
	waitForAsst(t, 10*time.Second, "run token", func() bool {
		b, err := os.ReadFile(filepath.Join(s.dir, "token"))
		token = string(b)
		return err == nil && token != ""
	})
	return runID, token
}

func (s *requestStack) endTurn(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (s *requestStack) runStatus(t *testing.T, id string) map[string]any {
	t.Helper()
	_, run, _ := doJSON(t, "GET", s.base+"/v1/runs/"+id, nil)
	return run
}

func (s *requestStack) waitRun(t *testing.T, id, status string) map[string]any {
	t.Helper()
	var run map[string]any
	waitForAsst(t, 15*time.Second, "run "+id+" "+status, func() bool {
		run = s.runStatus(t, id)
		return run["status"] == status
	})
	return run
}

func (s *requestStack) create(t *testing.T, token string, body map[string]any) map[string]any {
	t.Helper()
	code, out, _ := doJSONHeader(t, "POST", s.base+"/v1/requests", body, token)
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	req, _ := out["request"].(map[string]any)
	return req
}

// inboxItem consumes the bus and returns the inbox item of request id.
func (s *requestStack) inboxItem(t *testing.T, id string) alerts.InboxItem {
	t.Helper()
	s.srv.Alerts().Consume(t.Context())
	items, err := s.srv.Alerts().List(t.Context(), alerts.ListOpts{Kind: string(alerts.InboxRequest)})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.RequestID == id {
			return it
		}
	}
	t.Fatalf("no inbox item for request %s in %+v", id, items)
	return alerts.InboxItem{}
}

var promptJSON = regexp.MustCompile("(?s)```json\n(.*?)```")

func TestRequestRoundTrip(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer"}, "note": map[string]any{"type": "string", "format": "multiline"}}}
	options := []map[string]any{{"id": "a", "label": "A"}, {"id": "b", "label": "B"}}
	cases := []struct {
		decision string
		create   map[string]any
		answer   map[string]any
		check    func(a map[string]any) bool
	}{
		{"approve", map[string]any{"type": "confirm"}, map[string]any{}, func(map[string]any) bool { return true }},
		{"edit", map[string]any{"type": "review", "draft": map[string]any{"format": "text", "text": "one"}},
			map[string]any{"text": "two `x` \"q\" <b>\n"}, func(a map[string]any) bool { return a["text"] == "two `x` \"q\" <b>\n" }},
		{"reject", map[string]any{"type": "confirm"}, map[string]any{"note": "no"}, func(a map[string]any) bool { return a["note"] == "no" }},
		{"respond", map[string]any{"type": "confirm"}, map[string]any{"message": "why?\n\nexplain"}, func(a map[string]any) bool { return a["message"] == "why?\n\nexplain" }},
		{"choose", map[string]any{"type": "choose", "options": options}, map[string]any{"choices": []string{"b"}},
			func(a map[string]any) bool { c, _ := a["choices"].([]any); return len(c) == 1 && c[0] == "b" }},
		{"submit", map[string]any{"type": "form", "schema": schema}, map[string]any{"values": map[string]any{"n": 3, "note": "a\nb"}},
			func(a map[string]any) bool {
				v, _ := a["values"].(map[string]any)
				return v["n"] == 3.0 && v["note"] == "a\nb"
			}},
	}
	for _, c := range cases {
		t.Run(c.decision, func(t *testing.T) {
			s := newRequestStack(t)
			runID, token := s.launch(t)
			c.create["title"] = "Decide " + c.decision
			req := s.create(t, token, oneForm(c.create))
			id, _ := req["id"].(string)
			s.endTurn(t)
			s.waitRun(t, runID, "parked")
			if it := s.inboxItem(t, id); it.Status != alerts.InboxUnread || it.Title != "Decide "+c.decision {
				t.Fatalf("inbox item %+v", it)
			}

			c.answer["decision"] = c.decision
			code, ans, _ := doJSON(t, "POST", s.base+"/v1/requests/"+id+"/answer", answerAll(c.answer, 0))
			if code != 200 {
				t.Fatalf("answer %d %v", code, ans)
			}
			var got map[string]any
			waitForAsst(t, 15*time.Second, "delivered", func() bool {
				_, got, _ = doJSON(t, "GET", s.base+"/v1/requests/"+id, nil)
				return got["delivery_state"] == "delivered"
			})
			reqs := s.drv.all()
			if len(reqs) != 2 {
				t.Fatalf("driver starts %d", len(reqs))
			}
			if reqs[1].SessionID != "sess-1" || reqs[1].WorkingDir != reqs[0].WorkingDir {
				t.Fatalf("resume session %q dir %q, first dir %q", reqs[1].SessionID, reqs[1].WorkingDir, reqs[0].WorkingDir)
			}
			if next, _ := got["delivered_run_id"].(string); next != runID {
				t.Fatalf("delivered run %q, want the same run %q", next, runID)
			}
			if run := s.waitRun(t, runID, "complete"); run["turn_count"] != float64(2) {
				t.Fatalf("run after answer %v", run)
			}
			_, turns, _ := doJSON(t, "GET", s.base+"/v1/runs/"+runID+"/turns", nil)
			list, _ := turns["turns"].([]any)
			if len(list) != 2 {
				t.Fatalf("turns %v", turns)
			}
			if t2, _ := list[1].(map[string]any); t2["reason"] != "request" || t2["n"] != float64(2) {
				t.Fatalf("turn 2 %v", t2)
			}
			if it := s.inboxItem(t, id); it.Status != alerts.InboxResolved {
				t.Fatalf("inbox item after answer %+v", it)
			}

			lines, err := os.ReadFile(filepath.Join(s.dir, "prompts"))
			if err != nil {
				t.Fatal(err)
			}
			all := strings.Split(strings.TrimSpace(string(lines)), "\n")
			var frame struct{ Text string }
			if err := json.Unmarshal([]byte(all[len(all)-1]), &frame); err != nil {
				t.Fatal(err)
			}
			m := promptJSON.FindStringSubmatch(frame.Text)
			if m == nil {
				t.Fatalf("no answer JSON in prompt:\n%s", frame.Text)
			}
			var a map[string]any
			if err := json.Unmarshal([]byte(m[1]), &a); err != nil {
				t.Fatal(err)
			}
			if a["request_id"] != id || a["decision"] != c.decision || !c.check(a) {
				t.Fatalf("prompt answer %v", a)
			}
		})
	}
}

func TestRequestRunTokenCannotAnswer(t *testing.T) {
	s := newRequestStack(t)
	_, token := s.launch(t)
	req := s.create(t, token, oneForm(map[string]any{"type": "confirm", "title": "x"}))
	id, _ := req["id"].(string)
	for _, verb := range []string{"answer", "dismiss"} {
		code, body, _ := doJSONHeader(t, "POST", s.base+"/v1/requests/"+id+"/"+verb, answerAll(map[string]any{"decision": "approve"}, 0), token)
		if code != 403 {
			t.Fatalf("%s with run token: %d %v", verb, code, body)
		}
	}
	// A run token reads its own job's requests.
	if code, _, _ := doJSONHeader(t, "GET", s.base+"/v1/requests/"+id, nil, token); code != 200 {
		t.Fatalf("own get %d", code)
	}
	s.endTurn(t)
}

func TestRequestAssistantCanAnswer(t *testing.T) {
	s := newRequestStack(t)
	_, token := s.launch(t)
	defer s.endTurn(t)
	req := s.create(t, token, oneForm(map[string]any{"type": "confirm", "title": "x"}))
	id, _ := req["id"].(string)

	code, conv, _ := doJSON(t, "POST", s.base+"/v1/conversations", map[string]any{"workspace": s.wsID, "origin_channel": "cli", "harness": "fake"})
	if code != 201 {
		t.Fatalf("conversation %d %v", code, conv)
	}
	convID, _ := conv["id"].(string)
	asstToken, hash, err := assistant.MintSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.srv.AssistantStore().Update(t.Context(), convID, func(r *assistant.Record) {
		r.ActorTokenHash = hash
		r.Status = assistant.StatusRunning
	}); err != nil {
		t.Fatal(err)
	}
	code, got, _ := doJSONHeader(t, "POST", s.base+"/v1/requests/"+id+"/answer", answerAll(map[string]any{"decision": "approve"}, 0), asstToken)
	if code != 200 {
		t.Fatalf("assistant answer %d %v", code, got)
	}
	ans, _ := got["answered"].(map[string]any)
	if ans["by"] != "assistant" || ans["via"] != "assistant" {
		t.Fatalf("answer %v", ans)
	}
}

func TestRequestAnswerErrors(t *testing.T) {
	s := newRequestStack(t)
	_, token := s.launch(t)
	defer s.endTurn(t)
	req := s.create(t, token, oneForm(map[string]any{"type": "confirm", "title": "x"}))
	id, _ := req["id"].(string)
	url := s.base + "/v1/requests/" + id + "/answer"
	code, body, _ := doJSON(t, "POST", url, answerAll(map[string]any{"decision": "edit"}, 0))
	if code != 400 || errCode(body) != "validation" {
		t.Fatalf("edit on confirm %d %v", code, body)
	}
	code, body, _ = doJSONHeaders(t, "POST", url, answerAll(map[string]any{"decision": "approve"}, 0), map[string]string{"If-Match": `"9"`})
	if code != 412 {
		t.Fatalf("stale if-match %d %v", code, body)
	}
	approve := answerAll(map[string]any{"decision": "approve"}, 0)
	approve["via"] = "cli"
	if code, body, _ = doJSON(t, "POST", url, approve); code != 200 {
		t.Fatalf("answer %d %v", code, body)
	}
	if a, _ := body["answered"].(map[string]any); a["via"] != "cli" || a["by"] != "user" {
		t.Fatalf("answered %v", body["answered"])
	}
	code, body, _ = doJSON(t, "POST", url, answerAll(map[string]any{"decision": "reject"}, 0))
	errBody, _ := body["error"].(map[string]any)
	details, _ := errBody["details"].(map[string]any)
	if code != 409 || errCode(body) != "request_closed" || details["request"] == nil {
		t.Fatalf("second answer %d %v", code, body)
	}
	if code, _, _ = doJSON(t, "GET", s.base+"/v1/requests/NOPE", nil); code != 404 {
		t.Fatalf("missing %d", code)
	}
}

// TestRequestBatch: the create calls of one turn join one request, and the
// answer covers each form.
func TestRequestBatch(t *testing.T) {
	s := newRequestStack(t)
	_, token := s.launch(t)
	defer s.endTurn(t)
	first := s.create(t, token, map[string]any{"title": "Ship the release", "forms": []any{map[string]any{"type": "confirm", "title": "Tag it"}}})
	second := s.create(t, token, oneForm(map[string]any{"type": "confirm", "title": "Mail the team"}))
	id, _ := first["id"].(string)
	forms, _ := second["forms"].([]any)
	if second["id"] != id || second["title"] != "Ship the release" || len(forms) != 2 {
		t.Fatalf("second create %v", second)
	}
	url := s.base + "/v1/requests/" + id + "/answer"
	stale := fmt.Sprintf(`"%v"`, first["revision"])
	if code, body, _ := doJSONHeaders(t, "POST", url, answerAll(map[string]any{"decision": "approve"}, 0, 1), map[string]string{"If-Match": stale}); code != 412 {
		t.Fatalf("answer after a form joined %d %v", code, body)
	}
	code, body, _ := doJSON(t, "POST", url, answerAll(map[string]any{"decision": "approve"}, 0))
	if errBody, _ := body["error"].(map[string]any); code != 400 || !strings.Contains(fmt.Sprint(errBody["message"]), "form 1 has no answer") {
		t.Fatalf("missing form %d %v", code, body)
	}
	if code, body, _ = doJSON(t, "POST", url, answerAll(map[string]any{"decision": "approve"}, 0, 1)); code != 200 || body["status"] != "answered" {
		t.Fatalf("answer %d %v", code, body)
	}
	if code, body, _ = doJSONHeader(t, "POST", s.base+"/v1/requests", map[string]any{"title": "x"}, token); code != 400 || errCode(body) != "validation" {
		t.Fatalf("create with no forms %d %v", code, body)
	}
}

func TestRequestPlan402(t *testing.T) {
	s := newRequestStack(t, "backup")
	_, token := s.launch(t)
	defer s.endTurn(t)
	code, body, _ := doJSONHeader(t, "POST", s.base+"/v1/requests", oneForm(map[string]any{"type": "confirm", "title": "x"}), token)
	errBody, _ := body["error"].(map[string]any)
	details, _ := errBody["details"].(map[string]any)
	if code != 402 || errCode(body) != "plan_required" || details["feature"] != "requests" {
		t.Fatalf("402: %d %v", code, body)
	}
	// Without a run token, create is 403 before the plan check.
	if code, _, _ = doJSON(t, "POST", s.base+"/v1/requests", oneForm(map[string]any{"type": "confirm", "title": "x"})); code != 403 {
		t.Fatalf("human create %d", code)
	}
}

func TestRequestReportCSP(t *testing.T) {
	s := newRequestStack(t)
	runID, token := s.launch(t)
	defer s.endTurn(t)
	wd, _ := s.runStatus(t, runID)["working_dir"].(string)
	for name, body := range map[string]string{"r.html": "<h1>x</h1>", "r.md": "# x\n"} {
		if err := os.WriteFile(filepath.Join(wd, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"r.html", "r.md"}
	ctypes := []string{"text/html; charset=utf-8", "text/markdown; charset=utf-8"}
	req := s.create(t, token, map[string]any{"forms": []any{
		map[string]any{"type": "confirm", "title": "x", "report": names[0]},
		map[string]any{"type": "confirm", "title": "y", "report": names[1]},
	}})
	for idx, name := range names {
		ctype := ctypes[idx]
		resp, err := http.Get(fmt.Sprintf("%s/v1/requests/%s/forms/%d/report", s.base, req["id"], idx))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != ctype || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: %d %v", name, resp.StatusCode, resp.Header)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		want := ""
		if name == "r.html" {
			want = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline' https://cdn.jsdelivr.net https://cdnjs.cloudflare.com; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; img-src data: https:; connect-src 'none'; form-action 'none'; frame-ancestors 'self'"
		}
		if csp != want {
			t.Fatalf("%s CSP %q", name, csp)
		}
	}
}

// tokenTransport sends the run token on each MCP call.
type tokenTransport string

func (t tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(tools.HeaderRunToken, string(t))
	return http.DefaultTransport.RoundTrip(r)
}

func TestRequestToolPlanMessage(t *testing.T) {
	s := newRequestStack(t, "backup")
	_, token := s.launch(t)
	defer s.endTurn(t)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "req-test", Version: "1"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: s.base + "/mcp", DisableStandaloneSSE: true, HTTPClient: &http.Client{Transport: tokenTransport(token)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, text := mcpCall(t, cs, "request", "create", oneForm(map[string]any{"type": "confirm", "title": "x"}))
	if !res.IsError || !strings.Contains(text, "plan_required") || !strings.Contains(text, tools.RequestPlanMessage) {
		t.Fatalf("mcp 402: %s", text)
	}
}

// grantCloud writes a linked cloud row whose plan grants features.
func grantCloud(t *testing.T, st *store.Store, features []string) {
	t.Helper()
	raw, _ := json.Marshal(features)
	if _, err := st.DB().Exec(`INSERT INTO cloud_link
		(id, device_id, device_name, api_url, account_email, plan, features, linked_at, refreshed_at)
		VALUES (1, 'dev', 'mbp', 'http://127.0.0.1:1', '', 'pro_cloud', ?, '2026-10-05T00:00:00Z', '2026-10-05T00:00:00Z')`, string(raw)); err != nil {
		t.Fatal(err)
	}
}

// oneForm is a create body with the one form f.
func oneForm(f map[string]any) map[string]any { return map[string]any{"forms": []any{f}} }

// answerAll is an answer body with one answer a for each idx.
func answerAll(a map[string]any, idx ...int) map[string]any {
	forms := make([]any, len(idx))
	for i, n := range idx {
		f := map[string]any{"idx": n}
		for k, v := range a {
			f[k] = v
		}
		forms[i] = f
	}
	return map[string]any{"forms": forms}
}
