package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/requests"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// emailWorkerFake serves the trebi-mail contract endpoints the routes need:
// pull, ack, send, attachment bytes, and health.
type emailWorkerFake struct {
	token  string
	pulled []email.IngressMessage
	attach map[string]string // "<message-id>/<attachment-id>" -> body
	acks   int
	sends  int
	last   email.SendRequest
}

func (f *emailWorkerFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/ingress":
			writeEmailJSON(w, map[string]any{"v": 1, "messages": f.pulled, "next": len(f.pulled)})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/ingress/ack":
			f.acks++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/send":
			f.sends++
			f.last = email.SendRequest{}
			_ = json.NewDecoder(r.Body).Decode(&f.last) // a bad body fails the caller's asserts
			writeEmailJSON(w, map[string]any{"message_id": "out-1"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/attachments/"):
			body, ok := f.attach[strings.TrimPrefix(r.URL.Path, "/v1/attachments/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/health":
			writeEmailJSON(w, map[string]any{"ok": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// emailAPIActions counts the routing side effects; ids carry the count.
type emailAPIActions struct {
	env       email.RouteEnv
	wakes     int
	asks      int
	inbox     int
	decisions int
	messages  int
	resolves  int
	ambiguous int
}

func (a *emailAPIActions) RouteEnvFor(context.Context, string) (email.RouteEnv, error) {
	return a.env, nil
}

func (a *emailAPIActions) Ask(_ context.Context, _ email.RouteDecision, _ email.IngressMessage, _ string, text string) (string, error) {
	a.asks++
	return fmt.Sprintf("conv-%d", a.asks), nil
}

func (a *emailAPIActions) InboxItem(context.Context, email.RouteDecision, email.IngressMessage) (string, error) {
	a.inbox++
	return fmt.Sprintf("in-%d", a.inbox), nil
}

func (a *emailAPIActions) DecideApproval(context.Context, string, string) (bool, error) {
	a.decisions++
	return true, nil
}

func (a *emailAPIActions) DecideProposal(context.Context, string, bool, string) error { return nil }

func (a *emailAPIActions) Request(context.Context, string) (requests.Request, error) {
	return requests.Request{}, requests.ErrNotFound
}

func (a *emailAPIActions) AnswerRequest(context.Context, string, requests.AnswerIn) error { return nil }

func (a *emailAPIActions) RunMessage(context.Context, string, string) error {
	a.messages++
	return nil
}

func (a *emailAPIActions) ResolveInbox(context.Context, string) error {
	a.resolves++
	return nil
}

func (a *emailAPIActions) AmbiguousReplyItem(context.Context, string, email.RouteDecision, email.IngressMessage) (string, error) {
	a.ambiguous++
	return fmt.Sprintf("amb-%d", a.ambiguous), nil
}

func (a *emailAPIActions) AssistantName(context.Context, string) string { return "" }

// emailAPIServer is one daemon with the email channel live against a fake
// Worker, driven as an HTTP client.
type emailAPIServer struct {
	base string
	dir  string
	api  *api.Server
	fw   *emailWorkerFake
	acts *emailAPIActions
	svc  *email.Service
	st   *store.Store
}

func (e *emailAPIServer) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.base+path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

func (e *emailAPIServer) getRaw(t *testing.T, path string) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.base+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

// TestEmailRoutes drives one poll through the API, then reads the envelope
// log, one envelope, and one attachment, and checks the connectors row.
func TestEmailRoutes(t *testing.T) {
	e := startEmailAPI(t)
	addrRow, err := e.svc.EnsureAddress(t.Context(), "ws_1", "ellis-labs")
	if err != nil {
		t.Fatal(err)
	}
	addr := addrRow.LocalPart
	// The index holds the agent, so its mail rows carry an agent_id.
	if _, err := e.st.DB().Exec(`INSERT INTO job_index (id, workspace_id, name, path, kind, updated_at)
		VALUES ('agt_0000000001','ws_1','agents/research','/x/AGENT.md','agent','2026-09-28T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	// One batch, every verdict class: wake, assistant, drop. ULID ids keep
	// the log order and the cursor path honest.
	const (
		u1 = "01ARZ3NDEKTSV4RRFFQ69G5FA1"
		u2 = "01ARZ3NDEKTSV4RRFFQ69G5FA2"
		u3 = "01ARZ3NDEKTSV4RRFFQ69G5FA3"
	)
	e.fw.pulled = []email.IngressMessage{
		{V: 1, Seq: 1, ID: u1, LocalPart: addr, To: []string{addr + "@in.example.com"}, Tag: "research", From: "ops@example.com",
			Dmarc: "pass", MessageID: "<u1@x>", Subject: "run the digest",
			Attachments: []email.Attachment{{ID: "a1", Name: "invoice.pdf", Size: 4, Type: "application/pdf"}}},
		{V: 1, Seq: 2, ID: u2, LocalPart: addr, To: []string{addr + "@in.example.com"}, From: "ops@example.com", Dmarc: "pass", MessageID: "<u2@x>"},
		{V: 1, Seq: 3, ID: u3, LocalPart: addr, To: []string{addr + "@in.example.com"}, Tag: "billing", From: "stranger@example.net", Dmarc: "pass", MessageID: "<u3@x>"},
	}
	e.fw.attach["<u1@x>/a1"] = "PDF1"

	if code, m := e.do(t, http.MethodPost, "/v1/email/poll", nil); code != 200 || m["pulled"] != float64(3) {
		t.Fatalf("poll %d %v", code, m)
	}
	if e.acts.asks != 2 || e.acts.inbox != 0 {
		t.Fatalf("side effects: wakes %d asks %d inbox %d", e.acts.wakes, e.acts.asks, e.acts.inbox)
	}

	// Log: newest first, page walk by cursor, filters, and the two 400s.
	code, m := e.do(t, http.MethodGet, "/v1/email/messages", nil)
	if code != 200 {
		t.Fatalf("messages %d", code)
	}
	if list := m["messages"].([]any); len(list) != 3 || list[0].(map[string]any)["id"] != u3 {
		t.Fatalf("order: %v", m)
	}
	code, m = e.do(t, http.MethodGet, "/v1/email/messages?limit=2", nil)
	if code != 200 || m["next_cursor"] != u2 {
		t.Fatalf("page one: %d %v", code, m)
	}
	code, m = e.do(t, http.MethodGet, "/v1/email/messages?limit=2&cursor="+u2, nil)
	if code != 200 {
		t.Fatalf("page two: %d", code)
	}
	if m["next_cursor"] != nil || len(m["messages"].([]any)) != 1 {
		t.Fatalf("page two must end: %v", m)
	}
	if code, m := e.do(t, http.MethodGet, "/v1/email/messages?direction=out", nil); code != 200 || len(m["messages"].([]any)) != 0 {
		t.Fatalf("out filter: %d %v", code, m)
	}
	if code, m := e.do(t, http.MethodGet, "/v1/email/messages?direction=sideways", nil); code != 400 {
		t.Fatalf("direction 400: %d %v", code, m)
	}
	if code, m := e.do(t, http.MethodGet, "/v1/email/messages?cursor=zzz", nil); code != 400 {
		t.Fatalf("cursor 400: %d %v", code, m)
	}

	// One envelope with verdicts and links; a miss is a 404.
	code, m = e.do(t, http.MethodGet, "/v1/email/messages/"+u1, nil)
	if code != 200 || m["disposition"] != "routed" || m["agent_id"] != "agt_0000000001" || m["agent_name"] != "research" || m["conversation_id"] != "conv-1" {
		t.Fatalf("show: %d %v", code, m)
	}
	if atts := m["attachments"].([]any); len(atts) != 1 || atts[0].(map[string]any)["id"] != "a1" {
		t.Fatalf("attachments: %v", m)
	}
	if code, _ := e.do(t, http.MethodGet, "/v1/email/messages/01ARZ3NDEKTSV4RRFFQ69G5FA9", nil); code != 404 {
		t.Fatalf("missing envelope: %d", code)
	}

	// Attachment bytes stream through the Worker with mail-supplied name
	// and type on the wire; a wrong id is a 404, never a fetch.
	code, hdr, raw := e.getRaw(t, "/v1/email/messages/"+u1+"/attachments/a1")
	if code != 200 || string(raw) != "PDF1" {
		t.Fatalf("attachment %d %q", code, raw)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("content-type %q", ct)
	}
	if cd := hdr.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "filename=invoice.pdf") {
		t.Fatalf("content-disposition %q", cd)
	}
	if code, hdr, _ := e.getRaw(t, "/v1/email/messages/"+u1+"/attachments/zz"); code != 404 || hdr.Get("Content-Type") == "" {
		t.Fatalf("wrong attachment id: %d", code)
	}

	// The test send writes the outbound side of the log.
	if code, m := e.do(t, http.MethodPost, "/v1/email/test", nil); code != 200 || m["sent"] != true || e.fw.sends != 1 {
		t.Fatalf("test send: %d %v sends=%d", code, m, e.fw.sends)
	}
	code, m = e.do(t, http.MethodGet, "/v1/email/messages?direction=out", nil)
	if code != 200 || len(m["messages"].([]any)) != 1 {
		t.Fatalf("out log: %d %v", code, m)
	}

	// Status answers 200 with the live view.
	if code, m := e.do(t, http.MethodGet, "/v1/email/status", nil); code != 200 || m["configured"] != true || m["domain"] != "mail.example.com" {
		t.Fatalf("status: %d %v", code, m)
	}

	// The synthetic connectors row: present, restart is a poll, and the
	// process verbs have nothing to act on.
	code, m = e.do(t, http.MethodGet, "/v1/connectors", nil)
	if code != 200 {
		t.Fatalf("connectors %d", code)
	}
	var row map[string]any
	for _, c := range m["connectors"].([]any) {
		if c.(map[string]any)["slug"] == "email" {
			row = c.(map[string]any)
		}
	}
	if row == nil || row["provenance"] != "native" || row["managed"] != false {
		t.Fatalf("email row: %v", row)
	}
	if ev, ok := row["events"].(map[string]any); !ok || ev["status"] != "listening" {
		t.Fatalf("email row events: %v", row)
	}
	if code, m := e.do(t, http.MethodGet, "/v1/connectors/email", nil); code != 200 || m["provenance"] != "native" {
		t.Fatalf("connector get: %d %v", code, m)
	}
	if code, m := e.do(t, http.MethodPost, "/v1/connectors/email/restart", nil); code != 200 || m["pulled"] != float64(3) {
		t.Fatalf("restart-as-poll: %d %v", code, m)
	}
	// The process verbs have no process to act on: the row survives them.
	_, _ = e.do(t, http.MethodPost, "/v1/connectors/email/disconnect", nil)
	if code, m := e.do(t, http.MethodGet, "/v1/connectors/email", nil); code != 200 || m["provenance"] != "native" {
		t.Fatalf("row after disconnect: %d %v", code, m)
	}
}

// TestEmailWorkspaceAddress: masked by default, full on reveal, and rotate
// mints a new token in the clear.
func TestEmailWorkspaceAddress(t *testing.T) {
	e := startEmailAPI(t)

	dir := t.TempDir()
	wsPath := filepath.Join(dir, "ws")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "TREBI.md"),
		[]byte("---\nharness: fake\n---\n\nAssistant.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, wsBody := e.do(t, http.MethodPost, "/v1/workspaces", map[string]string{"path": wsPath, "name": "Ellis Labs", "handle": "ellis"})
	if code != 201 || wsBody["handle"] != "ellis" {
		t.Fatalf("workspace %d %v", code, wsBody)
	}
	wsID, _ := wsBody["id"].(string)

	code, m := e.do(t, http.MethodGet, "/v1/workspaces/"+wsID+"/email", nil)
	if code != 200 || m["masked"] != true {
		t.Fatalf("masked: %d %v", code, m)
	}
	if lp, _ := m["local_part"].(string); !strings.Contains(lp, "******") {
		t.Fatalf("local part not masked: %q", lp)
	}
	code, m = e.do(t, http.MethodGet, "/v1/workspaces/"+wsID+"/email?reveal=1", nil)
	if code != 200 || m["masked"] != false {
		t.Fatalf("revealed: %d %v", code, m)
	}
	full, _ := m["address"].(string)
	if !strings.HasSuffix(full, "@mail.example.com") || strings.Contains(full, "******") {
		t.Fatalf("full address %q", full)
	}
	rev := m["revision"]

	// Rotate is confirm-gated: it retires the address and breaks open threads.
	code, m = e.do(t, http.MethodPost, "/v1/workspaces/"+wsID+"/email/rotate", nil)
	if code != 400 {
		t.Fatalf("rotate without confirm: %d %v", code, m)
	}
	code, m = e.do(t, http.MethodPost, "/v1/workspaces/"+wsID+"/email/rotate?confirm=1", nil)
	if code != 200 || m["masked"] != false {
		t.Fatalf("rotate: %d %v", code, m)
	}
	if next, _ := m["address"].(string); next == full || !strings.HasSuffix(next, "@mail.example.com") {
		t.Fatalf("rotate must mint: %q then %q", full, next)
	}
	if m["revision"].(float64) <= rev.(float64) {
		t.Fatalf("revision must advance: %v then %v", rev, m["revision"])
	}

	// A rotate with a handle replaces the workspace handle; a bad one is a 400.
	code, m = e.do(t, http.MethodPost, "/v1/workspaces/"+wsID+"/email/rotate?confirm=1", map[string]string{"handle": "Bad!"})
	if code != 400 {
		t.Fatalf("rotate bad handle: %d %v", code, m)
	}
	code, m = e.do(t, http.MethodPost, "/v1/workspaces/"+wsID+"/email/rotate?confirm=1", map[string]string{"handle": "acme"})
	if lp, _ := m["local_part"].(string); code != 200 || !strings.HasPrefix(lp, "acme-") || len(lp) != len("acme-")+6 {
		t.Fatalf("rotate with handle: %d %v", code, m)
	}
	if code, ws := e.do(t, http.MethodGet, "/v1/workspaces/"+wsID, nil); code != 200 || ws["handle"] != "acme" {
		t.Fatalf("workspace handle after rotate: %d %v", code, ws)
	}
}

// TestEmailSendRoute drives POST /v1/email/send (plan I.3): an operator
// send mints the address on demand and lands in the out log, an agent send
// rides the subaddress, and strangers or missing fields are a 400.
func TestEmailSendRoute(t *testing.T) {
	e := startEmailAPI(t)

	dir := t.TempDir()
	wsPath := filepath.Join(dir, "ws")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "TREBI.md"),
		[]byte("---\nharness: fake\n---\n\nAssistant.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The agent send below needs an indexed agent.
	if err := os.MkdirAll(filepath.Join(wsPath, "agents", "research"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "agents", "research", "AGENT.md"), []byte("---\ndescription: Test agent.\n---\n\nResearch.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, wsBody := e.do(t, http.MethodPost, "/v1/workspaces", map[string]string{"path": wsPath, "name": "ellis"})
	if code != 201 {
		t.Fatalf("workspace %d %v", code, wsBody)
	}
	wsID, _ := wsBody["id"].(string)

	// An operator send: the address is minted on demand, the Worker carries
	// the mail, and the envelope lands in the out log.
	code, m := e.do(t, http.MethodPost, "/v1/email/send", map[string]any{
		"workspace": wsID, "to": []string{"ops@example.com"},
		"subject": "digest ready", "text": "The digest is ready.",
	})
	if code != 200 || m["sent"] != true {
		t.Fatalf("send: %d %v", code, m)
	}
	msgID, _ := m["message_id"].(string)
	if msgID == "" || e.fw.sends != 1 {
		t.Fatalf("send side effects: id %q sends=%d", msgID, e.fw.sends)
	}
	code, m = e.do(t, http.MethodGet, "/v1/email/messages?direction=out", nil)
	if code != 200 || len(m["messages"].([]any)) != 1 {
		t.Fatalf("out log: %d %v", code, m)
	}

	// An agent send rides the subaddress and names the agent in the log.
	code, m = e.do(t, http.MethodPost, "/v1/email/send", map[string]any{
		"workspace": wsID, "agent": "research", "to": []string{"ops@example.com"},
		"subject": "digest ready", "text": "The digest is ready.",
	})
	if code != 200 || e.fw.sends != 2 {
		t.Fatalf("agent send: %d %v sends=%d", code, m, e.fw.sends)
	}
	code, m = e.do(t, http.MethodGet, "/v1/email/messages?direction=out", nil)
	if code != 200 {
		t.Fatalf("out log: %d", code)
	}
	rows := m["messages"].([]any)
	if last := rows[0].(map[string]any); last["agent_name"] != "research" || last["agent_id"] == "" {
		t.Fatalf("agent row: %v", last)
	}

	// A stranger is a 400 validation, with no Worker call.
	if code, m = e.do(t, http.MethodPost, "/v1/email/send", map[string]any{
		"workspace": wsID, "to": []string{"stranger@example.net"},
		"subject": "digest ready", "text": "The digest is ready.",
	}); code != 400 || errCode(m) != "validation" || e.fw.sends != 2 {
		t.Fatalf("stranger: %d %v sends=%d", code, m, e.fw.sends)
	}

	// An unknown workspace is a 404, and a blank field is a 400.
	if code, m = e.do(t, http.MethodPost, "/v1/email/send", map[string]any{
		"workspace": "ws-nope", "to": []string{"ops@example.com"},
		"subject": "digest ready", "text": "The digest is ready.",
	}); code != 404 || errCode(m) != "not_found" {
		t.Fatalf("unknown workspace: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodPost, "/v1/email/send", map[string]any{
		"workspace": wsID, "to": []string{"ops@example.com"}, "text": "The digest is ready.",
	}); code != 400 || errCode(m) != "validation" {
		t.Fatalf("missing subject: %d %v", code, m)
	}
}

// TestEmailSendJobSendsAsOwner: an owned job run sends from its owner agent
// address with no agent field, and cannot name another agent.
func TestEmailSendJobSendsAsOwner(t *testing.T) {
	e := startEmailAPI(t)
	wsPath := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "TREBI.md"),
		[]byte("---\nharness: fake\n---\n\nAssistant.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJobFile(t, wsPath, "agents/research/digest")
	if err := os.WriteFile(filepath.Join(wsPath, "agents", "research", "AGENT.md"), []byte("---\ndescription: Test agent.\n---\n\nResearch.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, wsBody := e.do(t, http.MethodPost, "/v1/workspaces", map[string]string{"path": wsPath, "name": "owned"})
	if code != 201 {
		t.Fatalf("workspace %d %v", code, wsBody)
	}
	wsID, _ := wsBody["id"].(string)
	token := "job-token-00000000000000000000000000"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := e.st.DB().Exec(`INSERT INTO runs
		(id,workspace_id,job_id,kind,status,trace_id,started_at,updated_at,actor_token_hash)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		"run-job", wsID, indexedJobID(t, e.st, wsID, "agents/research/digest"), "job", "running", "trace", now, now,
		runs.HashActorToken(token)); err != nil {
		t.Fatal(err)
	}

	code, m := doScoped(t, http.MethodPost, e.base+"/v1/email/send", token, map[string]any{
		"workspace": wsID, "to": []string{"ops@example.com"},
		"subject": "digest ready", "text": "The digest is ready.",
	})
	if code != 200 {
		t.Fatalf("job send: %d %v", code, m)
	}
	code, m = e.do(t, http.MethodGet, "/v1/email/messages?direction=out", nil)
	if code != 200 {
		t.Fatalf("out log: %d", code)
	}
	if last := m["messages"].([]any)[0].(map[string]any); last["agent_name"] != "research" || last["agent_id"] == "" {
		t.Fatalf("job send must use the owner agent: %v", last)
	}

	code, m = doScoped(t, http.MethodPost, e.base+"/v1/email/send", token, map[string]any{
		"workspace": wsID, "agent": "other", "to": []string{"ops@example.com"},
		"subject": "digest ready", "text": "The digest is ready.",
	})
	if code != 403 {
		t.Fatalf("other agent: %d %v", code, m)
	}
}

// TestEmailSendJobTriggerAllowlist: a job accepts the senders of its own
// event: email trigger, and the addresses its owner agent accepts. An empty
// trigger allowlist opens nothing, and a non-email trigger adds no address.
func TestEmailSendJobTriggerAllowlist(t *testing.T) {
	e := startEmailAPI(t)
	// The owner agents of the jobs below. The inbound gate is the same list
	// (internal/workspaces.AcceptRules), so the fake routing env carries it.
	e.acts.env.Agents["ops"] = email.NewAgentAcceptRule([]job.AcceptEntry{{Address: "c@x.io"}})
	e.acts.env.Agents["marketing"] = email.NewAgentAcceptRule([]job.AcceptEntry{{Domain: "x.io"}})
	e.acts.env.Allowlist = []string{"friend@example.com"}

	wsPath := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(wsPath, "TREBI.md"), "---\nharness: fake\n---\n\nAssistant.\n")
	writeTestFile(t, filepath.Join(wsPath, "agents", "research", "AGENT.md"), "---\ndescription: Test agent.\n---\n\nResearch.\n")
	writeTestFile(t, filepath.Join(wsPath, "agents", "ops", "AGENT.md"), "---\ndescription: Test agent.\n---\n\nOps.\n")
	writeTestFile(t, filepath.Join(wsPath, "agents", "marketing", "AGENT.md"), "---\ndescription: Test agent.\n---\n\nMarketing.\n")

	job := func(name, triggers string) {
		writeTestFile(t, filepath.Join(wsPath, name, "JOB.md"),
			"---\nharness: fake\ntriggers:\n"+triggers+"---\n\nDo work.\n")
	}
	job("agents/research/digest", "  - event: email\n    allowlist:\n      - a@x.io\n")
	job("agents/research/quiet", "  - event: email\n    allowlist: []\n")
	job("agents/research/chatty", "  - event: whatsapp\n    allowlist:\n      - a@x.io\n")
	job("agents/research/plain", "  - cron: \"0 3 * * *\"\n")
	job("agents/ops/digest", "  - event: email\n")
	job("agents/marketing/digest", "  - event: email\n")

	code, wsBody := e.do(t, http.MethodPost, "/v1/workspaces", map[string]string{"path": wsPath, "name": "gated"})
	if code != 201 {
		t.Fatalf("workspace %d %v", code, wsBody)
	}
	wsID, _ := wsBody["id"].(string)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	token := func(run, name string) string {
		tok := "job-token-" + run + "-000000000000000000"
		if _, err := e.st.DB().Exec(`INSERT INTO runs
			(id,workspace_id,job_id,kind,status,trace_id,started_at,updated_at,actor_token_hash)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			run, wsID, indexedJobID(t, e.st, wsID, name), "job", "running", "trace", now, now,
			runs.HashActorToken(tok)); err != nil {
			t.Fatal(err)
		}
		return tok
	}
	send := func(tok, addr string) (int, map[string]any) {
		return doScoped(t, http.MethodPost, e.base+"/v1/email/send", tok, map[string]any{
			"workspace": wsID, "to": []string{addr},
			"subject": "digest ready", "text": "The digest is ready.",
		})
	}

	// The job trigger allowlist admits the address.
	if code, m := send(token("run-trigger", "agents/research/digest"), "a@x.io"); code != 200 {
		t.Fatalf("trigger allowlist: %d %v", code, m)
	}
	// Any other address stays out.
	if code, m := send(token("run-other", "agents/research/digest"), "b@x.io"); code != 400 {
		t.Fatalf("stranger: %d %v", code, m)
	}
	// An empty trigger allowlist means "every sender" for inbound mail. It
	// must not open sends.
	if code, m := send(token("run-empty", "agents/research/quiet"), "b@x.io"); code != 400 {
		t.Fatalf("empty allowlist: %d %v", code, m)
	}
	// Only event: email triggers count: a WhatsApp allowlist holds ids.
	if code, m := send(token("run-whatsapp", "agents/research/chatty"), "a@x.io"); code != 400 {
		t.Fatalf("whatsapp allowlist: %d %v", code, m)
	}
	// Admins and the workspace allowlist still pass for a job with no
	// email trigger, and its next recipient stays out.
	plain := token("run-plain", "agents/research/plain")
	if code, m := send(plain, "ops@example.com"); code != 200 {
		t.Fatalf("admin for a plain job: %d %v", code, m)
	}
	if code, m := send(plain, "friend@example.com"); code != 200 {
		t.Fatalf("workspace allowlist for a plain job: %d %v", code, m)
	}
	if code, m := send(plain, "b@x.io"); code != 400 {
		t.Fatalf("stranger for a plain job: %d %v", code, m)
	}
	// The owner agent email.accept rule adds addresses and domains.
	if code, m := send(token("run-owner", "agents/ops/digest"), "c@x.io"); code != 200 {
		t.Fatalf("owner accept address: %d %v", code, m)
	}
	if code, m := send(token("run-domain", "agents/marketing/digest"), "d@x.io"); code != 200 {
		t.Fatalf("owner accept domain: %d %v", code, m)
	}
}

// TestEmailSendParts: the HTML part, a workspace file, and inline bytes reach
// the Worker. A path outside the workspace and a bad file name are a 400.
func TestEmailSendParts(t *testing.T) {
	e := startEmailAPI(t)
	wsPath := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(filepath.Join(wsPath, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "TREBI.md"), []byte("Assistant.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "reports", "q3.csv"), []byte("week,runs\n39,42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, wsBody := e.do(t, http.MethodPost, "/v1/workspaces", map[string]string{"path": wsPath, "name": "ellis"})
	if code != 201 {
		t.Fatalf("workspace %d %v", code, wsBody)
	}
	wsID, _ := wsBody["id"].(string)
	send := func(extra map[string]any) (int, map[string]any) {
		body := map[string]any{
			"workspace": wsID, "to": []string{"ops@example.com"},
			"subject": "digest ready", "text": "The digest is ready.",
		}
		maps.Copy(body, extra)
		return e.do(t, http.MethodPost, "/v1/email/send", body)
	}

	code, m := send(map[string]any{
		"text": nil, "html": "<p>The digest is <b>ready</b>.</p>",
		"attachments": []any{
			map[string]any{"path": "reports/q3.csv"},
			map[string]any{"name": "note.txt", "content": base64.StdEncoding.EncodeToString([]byte("hi"))},
		},
	})
	if code != 200 || m["sent"] != true {
		t.Fatalf("send: %d %v", code, m)
	}
	got := e.fw.last
	if got.HTML != "<p>The digest is <b>ready</b>.</p>" || got.Text != "The digest is ready." || len(got.Attachments) != 2 {
		t.Fatalf("worker request: %+v", got)
	}
	if a := got.Attachments[0]; a.Name != "q3.csv" || a.Type != "text/csv" || string(a.Content) != "week,runs\n39,42\n" {
		t.Fatalf("workspace file: %+v", a)
	}
	if a := got.Attachments[1]; a.Name != "note.txt" || !strings.HasPrefix(a.Type, "text/plain") || string(a.Content) != "hi" {
		t.Fatalf("inline file: %+v", a)
	}

	for name, extra := range map[string]map[string]any{
		"outside workspace": {"attachments": []any{map[string]any{"path": "../secret.txt"}}},
		"missing file":      {"attachments": []any{map[string]any{"path": "reports/none.csv"}}},
		"bad name":          {"attachments": []any{map[string]any{"name": "a/b.txt", "content": "aGk="}}},
		"bad base64":        {"attachments": []any{map[string]any{"name": "a.txt", "content": "%%"}}},
		"too large":         {"text": nil, "html": strings.Repeat("x", email.MaxSendBytes)},
		"text and html":     {"html": "<p>ready</p>"},
		"no body":           {"text": nil},
	} {
		sends := e.fw.sends
		if code, m := send(extra); code != 400 || errCode(m) != "validation" || e.fw.sends != sends {
			t.Errorf("%s: %d %v", name, code, m)
		}
	}
}

// TestEmailUnconfigured: with no email block the channel answers, the
// synthetic row is absent, and a manual poll is a conflict, not a spin.
func TestEmailUnconfigured(t *testing.T) {
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

	do := func(method, path string) (int, map[string]any) {
		req, _ := http.NewRequest(method, base+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return resp.StatusCode, m
	}
	if code, m := do(http.MethodGet, "/v1/email/status"); code != 200 || m["configured"] != false {
		t.Fatalf("status: %d %v", code, m)
	}
	if code, _ := do(http.MethodGet, "/v1/connectors/email"); code != 404 {
		t.Fatalf("synthetic row must be absent: %d", code)
	}
	if code, m := do(http.MethodPost, "/v1/email/poll"); code != 409 || m["error"] == nil {
		t.Fatalf("poll without ingress: %d %v", code, m)
	}
}

// syncLogBuf is a race-safe sink for the daemon's slog default output.
type syncLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newEmailWorkspace creates one workspace with a live assistant file.
func newEmailWorkspace(t *testing.T, e *emailAPIServer, name string) string {
	t.Helper()
	wsPath := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "TREBI.md"),
		[]byte("---\nharness: fake\n---\n\nAssistant.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body := e.do(t, http.MethodPost, "/v1/workspaces", map[string]string{"path": wsPath, "name": name})
	if code != 201 {
		t.Fatalf("workspace %s: %d %v", name, code, body)
	}
	id, _ := body["id"].(string)
	return id
}

// TestAgentEmailReveal: the agent subaddress serves masked by default and
// full on reveal (plan 2026-09-24 B.3), the reveal rides the same audited
// request log as the workspace reveal, an unknown agent is a 404, and a
// scoped agent run is rejected before the mask moves.
func TestAgentEmailReveal(t *testing.T) {
	sink := &syncLogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(sink, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := startEmailAPI(t)
	wsID := newEmailWorkspace(t, e, "ellis")
	code, body, _ := doJSON(t, "PUT", e.base+"/v1/workspaces/"+wsID+"/agents/research", map[string]any{"description": "Market research."})
	if code != 201 {
		t.Fatalf("agent create: %d %v", code, body)
	}

	// Masked by default: the token is hidden, the tag stays readable.
	code, m := e.do(t, http.MethodGet, "/v1/workspaces/"+wsID+"/agents/research/email", nil)
	if code != 200 || m["masked"] != true {
		t.Fatalf("masked: %d %v", code, m)
	}
	if lp, _ := m["local_part"].(string); !strings.Contains(lp, "******") || !strings.HasSuffix(lp, "+research") {
		t.Fatalf("masked local part %q", lp)
	}
	if addr, _ := m["address"].(string); !strings.HasSuffix(addr, "@mail.example.com") || !strings.Contains(addr, "******") {
		t.Fatalf("masked address %q", addr)
	}

	// Reveal returns the full subaddress and writes the audit entry the
	// workspace reveal writes: one api.request line through the same
	// audited mux.
	code, raw, derr := e.api.Dispatch(t.Context(), request.HTTPRequest{
		Method: http.MethodGet,
		Path:   "/v1/workspaces/" + wsID + "/agents/research/email?reveal=1",
	})
	if derr != nil {
		t.Fatal(derr)
	}
	var revealed map[string]any
	if err := json.Unmarshal(raw, &revealed); err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK || revealed["masked"] != false {
		t.Fatalf("revealed: %d %v", code, revealed)
	}
	if addr, _ := revealed["address"].(string); !strings.HasSuffix(addr, "+research@mail.example.com") || strings.Contains(addr, "******") {
		t.Fatalf("full address %q", addr)
	}
	out := sink.String()
	if !strings.Contains(out, "api.request") || !strings.Contains(out, "/agents/research/email") {
		t.Fatalf("reveal audit entry missing: %s", out)
	}

	// An unknown agent is a 404, not a made-up address.
	if code, m = e.do(t, http.MethodGet, "/v1/workspaces/"+wsID+"/agents/nope/email", nil); code != 404 || errCode(m) != "not_found" {
		t.Fatalf("unknown agent: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodGet, "/v1/workspaces/ws-nope/agents/research/email", nil); code != 404 {
		t.Fatalf("unknown workspace: %d %v", code, m)
	}

	// A scoped agent run is rejected before any address is read.
	st := mustOpenStore(t, filepath.Join(e.dir, "trebi.db"))
	token := "agent-token-0000000000000000000000"
	insertAgentCaller(t, st, wsID, "research", token, "running")
	for _, path := range []string{
		"/v1/workspaces/" + wsID + "/agents/research/email",
		"/v1/workspaces/" + wsID + "/agents/research/email?reveal=1",
	} {
		if code, m := doScoped(t, http.MethodGet, e.base+path, token, nil); code != http.StatusForbidden || errCode(m) != "forbidden" {
			t.Fatalf("scoped agent %s: %d %v", path, code, m)
		}
	}
}

// TestEmailMessagesWorkspaceFilter: the envelope log takes the workspace
// parameter by name or id (plan 2026-09-24 A.3).
func TestEmailMessagesWorkspaceFilter(t *testing.T) {
	e := startEmailAPI(t)
	wsA := newEmailWorkspace(t, e, "ellis")
	wsB := newEmailWorkspace(t, e, "orbit")

	send := func(ws string) {
		t.Helper()
		code, m := e.do(t, http.MethodPost, "/v1/email/send", map[string]any{
			"workspace": ws, "to": []string{"ops@example.com"},
			"subject": "digest", "text": "The digest is ready.",
		})
		if code != 200 || m["sent"] != true {
			t.Fatalf("send %s: %d %v", ws, code, m)
		}
	}
	send(wsA)
	send(wsB)

	code, m := e.do(t, http.MethodGet, "/v1/email/messages", nil)
	if code != 200 || len(m["messages"].([]any)) != 2 {
		t.Fatalf("unfiltered: %d %v", code, m)
	}
	// By name and by id, the filter scopes to one workspace.
	for _, ws := range []string{"ellis", wsA} {
		code, m = e.do(t, http.MethodGet, "/v1/email/messages?workspace="+ws, nil)
		if code != 200 || len(m["messages"].([]any)) != 1 {
			t.Fatalf("filter %s: %d %v", ws, code, m)
		}
		if row := m["messages"].([]any)[0].(map[string]any); row["workspace_id"] != wsA {
			t.Fatalf("filter %s row: %v", ws, row)
		}
	}
	if code, m = e.do(t, http.MethodGet, "/v1/email/messages?workspace=zzz", nil); code != 200 || len(m["messages"].([]any)) != 0 {
		t.Fatalf("unknown workspace filter: %d %v", code, m)
	}
}

func startEmailAPI(t *testing.T) *emailAPIServer {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
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

	fw := &emailWorkerFake{token: "test-token-0000", attach: map[string]string{}}
	ws := httptest.NewServer(fw.handler())
	t.Cleanup(ws.Close)

	acts := &emailAPIActions{env: email.RouteEnv{
		Agents:    map[string]email.AcceptRule{"research": email.NewAcceptRule([]job.AcceptEntry{{Address: "ops@example.com"}})},
		Assistant: email.NewAcceptRule([]job.AcceptEntry{{Address: "ops@example.com"}}),
	}}
	s.Email().Configure(&email.Settings{
		Worker: ws.URL, Token: "test-token-0000", Domain: "mail.example.com",
		Admins: []string{"ops@example.com"}, PollInterval: time.Minute,
		RequireDMARC: true, FromName: "Trebi",
	}, email.NewClient(ws.URL, "test-token-0000"))
	s.Email().AttachIngress(email.NewIngress(s.Email(), connectors.New(st, nil, nil), acts, time.Minute, false))
	return &emailAPIServer{base: "http://" + addr, dir: dir, api: s, fw: fw, acts: acts, svc: s.Email(), st: st}
}

func writeEmailJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// TestEmailHeldRoutes holds two replies from a new sender in a thread of
// research, then releases one by sender and ignores the other.
func TestEmailHeldRoutes(t *testing.T) {
	e := startEmailAPI(t)
	wsID := newEmailWorkspace(t, e, "ellis")
	if code, body, _ := doJSON(t, "PUT", e.base+"/v1/workspaces/"+wsID+"/agents/research", map[string]any{"description": "Market research."}); code/100 != 2 {
		t.Fatalf("agent: %d %v", code, body)
	}
	agentID := indexedJobID(t, e.st, wsID, "agents/research")
	addr, err := e.svc.EnsureAddress(t.Context(), wsID, "ellis")
	if err != nil {
		t.Fatal(err)
	}
	const thread = "<out1@mail.example.com>"
	if _, err := e.st.DB().Exec(`INSERT INTO email_messages (id, direction, workspace_id, agent_id, message_id, thread_root,
		recipients, subject, disposition, created_at, revision) VALUES ('out1','out',?,?,?,?,'client@acme.com','Quote','sent','2026-10-01T00:00:00Z',1)`,
		wsID, agentID, thread, thread); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"r1", "r2"} {
		if err := e.svc.Deliver(t.Context(), email.IngressMessage{V: 1, ID: id, LocalPart: addr.LocalPart, Tag: "research",
			To: []string{addr.Full()}, From: "newbie@acme.com", Dmarc: "pass", Subject: "Re: Quote", MessageID: "<" + id + "@acme.com>",
			InReplyTo: thread, References: []string{thread}, Text: "Thanks."}); err != nil {
			t.Fatal(err)
		}
	}
	code, m := e.do(t, http.MethodGet, "/v1/email/held?workspace=ellis", nil)
	if code != 200 || len(m["items"].([]any)) != 2 {
		t.Fatalf("held: %d %v", code, m)
	}
	// A decision that held r1 needs a confirm before the release.
	if _, err := e.st.DB().Exec(`INSERT INTO decisions (id, workspace_id, kind, kind_version, subject_kind, subject_id, decider, method,
		status, outcome, questions_json, answers_json, took_ms, created_at) VALUES ('dec_1', ?, 'email.reply_risk', 1, 'email', 'r1',
		'fake', 'verbal', 'ok', 'hold', '[]', '{"answers":[],"reasons":["Asks for a secret"]}', 5, '2026-10-01T00:00:00Z')`, wsID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB().Exec(`UPDATE email_messages SET decision_id='dec_1' WHERE id='r1'`); err != nil {
		t.Fatal(err)
	}
	code, m = e.do(t, http.MethodPost, "/v1/email/messages/r1/release", map[string]any{"scope": "sender"})
	if code != 400 || !strings.Contains(fmt.Sprint(m), "Asks for a secret") || !strings.Contains(fmt.Sprint(m), "confirm_required") {
		t.Fatalf("release without confirm: %d %v", code, m)
	}
	if det, _ := m["error"].(map[string]any)["details"].(map[string]any); det["agent"] != "research" || fmt.Sprint(det["reasons"]) != "[Asks for a secret]" {
		t.Fatalf("confirm details: %v", m)
	}
	if code, m = e.do(t, http.MethodPost, "/v1/email/messages/r1/release?scope=all&confirm=1", nil); code != 400 {
		t.Fatalf("bad scope: %d %v", code, m)
	}
	code, m = e.do(t, http.MethodPost, "/v1/email/messages/r1/release?confirm=1", map[string]any{"scope": "sender"})
	if code != 200 || m["disposition"] != "routed" || e.acts.asks != 1 {
		t.Fatalf("release: %d %v asks %d", code, m, e.acts.asks)
	}
	row, err := e.api.Workspaces().JobByName(t.Context(), wsID, "agents/research")
	if err != nil {
		t.Fatal(err)
	}
	if j, err := job.ParseFile(row.Path); err != nil || !slices.Contains(j.EmailAccept, "newbie@acme.com") {
		t.Fatalf("email.accept: %v %v", j, err)
	}
	if code, m = e.do(t, http.MethodGet, "/v1/decisions/dec_1", nil); code != 200 || m["feedback"] != "disagree" {
		t.Fatalf("feedback: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodPost, "/v1/email/messages/r1/release", nil); code != 409 {
		t.Fatalf("second release: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodPost, "/v1/email/messages/r2/ignore", nil); code != 200 || m["disposition"] != "dropped" {
		t.Fatalf("ignore: %d %v", code, m)
	}
	if code, m = e.do(t, http.MethodPost, "/v1/email/messages/nope/ignore", nil); code != 404 {
		t.Fatalf("unknown: %d %v", code, m)
	}
}

// TestEmailRecipientRefused: a refused job send answers 400 with the address
// and the fix, and opens one inbox item per job and address. An operator send
// gets the error only.
func TestEmailRecipientRefused(t *testing.T) {
	e := startEmailAPI(t)
	wsPath := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(wsPath, "TREBI.md"), "---\nharness: fake\n---\n\nAssistant.\n")
	writeTestFile(t, filepath.Join(wsPath, "agents", "research", "AGENT.md"), "---\ndescription: Test agent.\n---\n\nResearch.\n")
	writeTestFile(t, filepath.Join(wsPath, "agents", "research", "digest", "JOB.md"),
		"---\nharness: fake\ntriggers:\n  - event: email\n    allowlist:\n      - a@x.io\n---\n\nDo work.\n")
	code, wsBody := e.do(t, http.MethodPost, "/v1/workspaces", map[string]string{"path": wsPath, "name": "refuse"})
	if code != 201 {
		t.Fatalf("workspace %d %v", code, wsBody)
	}
	wsID, _ := wsBody["id"].(string)
	token := "job-token-refuse-000000000000000000"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := e.st.DB().Exec(`INSERT INTO runs
		(id,workspace_id,job_id,kind,status,trace_id,started_at,updated_at,actor_token_hash)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		"run-refuse", wsID, indexedJobID(t, e.st, wsID, "agents/research/digest"), "job", "running",
		"trace", now, now, runs.HashActorToken(token)); err != nil {
		t.Fatal(err)
	}
	refusals := func() []alerts.InboxItem {
		page, err := e.api.Alerts().List(t.Context(), alerts.ListOpts{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		var out []alerts.InboxItem
		for _, item := range page.Items {
			if item.AlertKind == string(alerts.KindRecipientRefused) {
				out = append(out, item)
			}
		}
		return out
	}

	code, m := doScoped(t, http.MethodPost, e.base+"/v1/email/send", token, map[string]any{
		"workspace": wsID, "to": []string{"emma@example.com"},
		"subject": "digest ready", "text": "The digest is ready.",
	})
	if code != 400 {
		t.Fatalf("refused send: %d %v", code, m)
	}
	details, _ := m["error"].(map[string]any)["details"].(map[string]any)
	if details["recipient"] != "emma@example.com" {
		t.Fatalf("details = %v", details)
	}
	if fix, _ := details["fix"].([]any); len(fix) == 0 {
		t.Fatalf("details has no fix: %v", details)
	}
	items := refusals()
	if len(items) != 1 {
		t.Fatalf("refusals = %d, want one", len(items))
	}
	if items[0].Title != "Email to emma@example.com refused" || items[0].Hint == "" || items[0].JobName != "agents/research/digest" {
		t.Fatalf("item = %+v", items[0])
	}

	// A second refusal from the same job and address updates the one item.
	doScoped(t, http.MethodPost, e.base+"/v1/email/send", token, map[string]any{
		"workspace": wsID, "to": []string{"emma@example.com"},
		"subject": "digest ready", "text": "The digest is ready.",
	})
	if items := refusals(); len(items) != 1 {
		t.Fatalf("refusals after the second send = %d, want one", len(items))
	}

	// An operator send has no actor, so it raises nothing.
	code, m = e.do(t, http.MethodPost, "/v1/email/send", map[string]any{
		"workspace": wsID, "to": []string{"emma@example.com"},
		"subject": "hello", "text": "Hello.",
	})
	if code != 400 {
		t.Fatalf("operator send: %d %v", code, m)
	}
	if items := refusals(); len(items) != 1 {
		t.Fatalf("operator send raised an item: %d", len(items))
	}
}

// writeTestFile writes one test fixture and fails the test on error.
func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
