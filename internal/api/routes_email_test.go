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
	"strings"
	"sync"
	"testing"
	"time"

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

func (a *emailAPIActions) AnswerRequest(context.Context, string, requests.Answer) error { return nil }

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
	if code != 200 || m["disposition"] != "routed" || m["agent"] != "research" || m["conversation_id"] != "conv-1" {
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
	if last := rows[0].(map[string]any); last["agent"] != "research" {
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
	code, wsBody := e.do(t, http.MethodPost, "/v1/workspaces", map[string]string{"path": wsPath, "name": "owned"})
	if code != 201 {
		t.Fatalf("workspace %d %v", code, wsBody)
	}
	wsID, _ := wsBody["id"].(string)
	token := "job-token-00000000000000000000000000"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := e.st.DB().Exec(`INSERT INTO runs
		(id,workspace_id,job,kind,status,trace_id,started_at,updated_at,actor_token_hash)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		"run-job", wsID, "agents/research/digest", "job", "running", "trace", now, now,
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
	if last := m["messages"].([]any)[0].(map[string]any); last["agent"] != "research" {
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
		"html": "<p>The digest is <b>ready</b>.</p>",
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
		"too large":         {"html": strings.Repeat("x", email.MaxSendBytes)},
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
	code, body, _ := doJSON(t, "PUT", e.base+"/v1/workspaces/"+wsID+"/agents/research", nil)
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
