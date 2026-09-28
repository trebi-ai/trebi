package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/outbox"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// mailWorkerFake serves the trebi-mail contract the poller drives: ingress
// with a live cursor, acks, sends, and health.
type mailWorkerFake struct {
	token string

	mu        sync.Mutex // guards the fields below; the handler runs on server goroutines
	batch     []email.IngressMessage
	sends     []email.SendRequest
	acks      int
	failSends bool
}

func (f *mailWorkerFake) setBatch(b []email.IngressMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batch = b
}

func (f *mailWorkerFake) setFailSends(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSends = v
}

func (f *mailWorkerFake) ackCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acks
}

// sent returns a copy of the send requests so far.
func (f *mailWorkerFake) sent() []email.SendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]email.SendRequest(nil), f.sends...)
}

func (f *mailWorkerFake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/ingress":
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		next := after
		var out []email.IngressMessage
		for _, m := range f.batch {
			if m.Seq > after {
				out = append(out, m)
				if m.Seq > next {
					next = m.Seq
				}
			}
		}
		writeMailJSON(w, map[string]any{"v": 1, "messages": out, "next": next})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/ingress/ack":
		f.acks++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/send":
		var req email.SendRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.sends = append(f.sends, req)
		if f.failSends {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeMailJSON(w, map[string]any{"message_id": req.MessageID})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/health":
		writeMailJSON(w, map[string]any{"ok": true})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// mailPlane is a live control plane with the email channel pointed at the
// fake Worker. Shadow keeps every ticker quiet; polls run by hand.
type mailPlane struct {
	cp    *daemon.ControlPlane
	fw    *mailWorkerFake
	st    *store.Store
	wsID  string
	local string
	full  string
}

// poll swaps the batch and runs one poll to completion.
func (p *mailPlane) poll(t *testing.T, batch []email.IngressMessage) int {
	t.Helper()
	p.fw.setBatch(batch)
	n, err := p.cp.Email.PollNow(t.Context())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	return n
}

// TestEmailPollerWakeApproveReply drives the full poller through routing
// (plan Part N): a tagged mail wakes an agent, an operator reply decides an
// approval, and a plain mail opens a conversation the daemon answers on the
// thread.
func TestEmailPollerWakeApproveReply(t *testing.T) {
	p := startMailPlane(t)
	// Leg 3 injects the answer by hand, so the assistant session stays silent.
	t.Setenv(agentwire.FakeScriptEnv, "cat >/dev/null")
	ctx := t.Context()

	// Leg 1: tagged mail wakes the research agent. The run row exists when
	// the poll returns; the envelope records the route; the batch is acked.
	const (
		u1 = "01ARZ3NDEKTSV4RRFFQ69G5FA1"
		u2 = "01ARZ3NDEKTSV4RRFFQ69G5FA2"
		u3 = "01ARZ3NDEKTSV4RRFFQ69G5FA3"
	)
	if n := p.poll(t, []email.IngressMessage{{
		V: 1, Seq: 1, ID: u1, LocalPart: p.local, Tag: "research",
		From: "alice@example.com", Dmarc: "pass", MessageID: "<wake@x>",
		Subject: "dig the news", Text: "Run the research pass.",
	}}); n != 1 {
		t.Fatalf("pulled %d, want 1", n)
	}
	var agentRuns []*runs.Run
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		agentRuns, err = p.cp.Runs.List(ctx, runs.ListOpts{Workspace: p.wsID, Kind: string(runs.KindAgent)})
		if err != nil {
			t.Fatal(err)
		}
		if len(agentRuns) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(agentRuns) != 1 || agentRuns[0].Job != "agents/research" {
		t.Fatalf("agent runs = %+v", agentRuns)
	}
	run := agentRuns[0]
	env1, err := p.cp.Email.GetMessage(ctx, u1)
	if err != nil {
		t.Fatal(err)
	}
	if env1.Disposition != "routed" || env1.Agent != "research" || env1.ConversationID == "" {
		t.Fatalf("wake envelope = %+v", env1)
	}
	agentConv, err := p.cp.Conversations.Get(ctx, env1.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if agentConv.Operator != "agents/research" || agentConv.OriginChannel != conversations.ChannelEmail {
		t.Fatalf("agent conversation = %+v", agentConv)
	}
	if p.fw.ackCount() != 1 {
		t.Fatalf("acks = %d, want 1", p.fw.ackCount())
	}

	// Leg 2: the run asks for approval; the alert mail goes out; the
	// operator replies "Approve" on that mail and the decision lands.
	if _, err := p.cp.Runs.OpenApproval(ctx, run.ID, "Deploy to production?"); err != nil {
		t.Fatal(err)
	}
	inbox, err := p.cp.Alerts.Raise(ctx, alerts.RaiseOpts{
		Kind: alerts.KindWaiting, RunID: run.ID, WorkspaceID: p.wsID,
		Title: "Agent waiting", Body: "Deploy to production?",
		Hint: "Reply approve or deny by mail",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Shadow skips route delivery, so the test mails the alert itself.
	row, alertMsgID, err := p.cp.Email.SendAlert(ctx, email.AlertMail{
		InboxID: inbox, Subject: "[trebi] approval requested", Text: "Reply approve or deny.",
		To: []string{"ops@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.cp.Outbox.SendNow(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		var linked string
		if err := p.st.DB().QueryRowContext(ctx, `SELECT email_message_id FROM inbox_items WHERE id=?`, inbox).Scan(&linked); err != nil {
			t.Fatal(err)
		}
		if linked == alertMsgID {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := p.poll(t, []email.IngressMessage{{
		V: 1, Seq: 2, ID: u2, LocalPart: p.local,
		From: "ops@example.com", Dmarc: "pass", MessageID: "<decide@x>",
		InReplyTo: alertMsgID, Subject: "Re: [trebi] approval requested",
		Text: "Approve. Ship it.",
	}}); n != 1 {
		t.Fatalf("pulled %d, want 1", n)
	}
	if pa, err := p.cp.Runs.PendingApproval(ctx, run.ID); err != nil || pa != nil {
		t.Fatalf("approval still pending: %+v err=%v", pa, err)
	}
	item, err := p.cp.Alerts.Get(ctx, inbox)
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != alerts.InboxResolved {
		t.Fatalf("inbox status = %q, want resolved", item.Status)
	}
	env2, err := p.cp.Email.GetMessage(ctx, u2)
	if err != nil {
		t.Fatal(err)
	}
	if env2.RunID != run.ID || env2.InboxID != inbox {
		t.Fatalf("reply envelope run=%q inbox=%q", env2.RunID, env2.InboxID)
	}

	// Leg 3: plain operator mail opens a conversation; the run answers; the
	// answer goes back on the thread as one mail.
	if n := p.poll(t, []email.IngressMessage{{
		V: 1, Seq: 3, ID: u3, LocalPart: p.local,
		From: "ops@example.com", Dmarc: "pass", MessageID: "<ask@x>",
		Subject: "weekly digest", Text: "Send the digest.",
	}}); n != 1 {
		t.Fatalf("pulled %d, want 1", n)
	}
	pointer, err := p.cp.Conversations.GetPointer(ctx, p.wsID, conversations.Address{
		Connector: conversations.ChannelEmail, Room: p.wsID, Thread: "<ask@x>",
	})
	if err != nil {
		t.Fatal(err)
	}
	convID := pointer.ConversationID
	if convID == "" {
		t.Fatal("email conversation pointer missing")
	}
	const turnText = "The digest is ready. Three items need review."
	if _, err := p.cp.Conversations.IngestAssistantTurns(ctx, run.ID, convID, []conversations.IncomingTurn{{
		Role: conversations.RoleAssistant, Text: turnText, At: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	sendsBefore := len(p.fw.sent())
	if err := p.cp.Email.ReplyOnThread(ctx, email.ReplyOpts{RunID: run.ID, ConversationID: convID}); err != nil {
		t.Fatal(err)
	}
	p.waitSends(t, sendsBefore+1)
	// A repeat covers the same turn: no second mail for one exchange.
	if err := p.cp.Email.ReplyOnThread(ctx, email.ReplyOpts{RunID: run.ID, ConversationID: convID}); err != nil {
		t.Fatal(err)
	}
	if len(p.fw.sent()) != sendsBefore+1 {
		t.Fatalf("second reply sent again: %d sends", len(p.fw.sent()))
	}
	sent := p.fw.sent()
	last := sent[len(sent)-1]
	if len(last.To) != 1 || last.To[0] != "ops@example.com" {
		t.Fatalf("reply to = %v", last.To)
	}
	if last.From != p.full {
		t.Fatalf("reply from = %q, want %q", last.From, p.full)
	}
	if !strings.HasPrefix(last.Subject, "Re: ") || !strings.Contains(last.Subject, "weekly digest") {
		t.Fatalf("reply subject = %q", last.Subject)
	}
	if last.InReplyTo != "<ask@x>" {
		t.Fatalf("reply in-reply-to = %q", last.InReplyTo)
	}
	if len(last.References) == 0 || last.References[0] != "<ask@x>" {
		t.Fatalf("reply references = %v", last.References)
	}
	if !strings.Contains(last.Text, turnText) || !strings.Contains(last.Text, convID) {
		t.Fatalf("reply text = %q", last.Text)
	}

	// The outbound envelope row carries the conversation, the run, and
	// covered_turn 0 (the one turn of this exchange).
	out, err := p.cp.Email.ListMessages(ctx, email.MessageListOpts{Direction: "out", Workspace: p.wsID})
	if err != nil {
		t.Fatal(err)
	}
	var envelope email.Message
	for _, m := range out {
		if m.ConversationID == convID {
			envelope = m
		}
	}
	if envelope.ID == "" {
		t.Fatalf("no outbound envelope for conversation %s", convID)
	}
	if envelope.RunID != run.ID || envelope.Disposition != "sent" {
		t.Fatalf("reply envelope = %+v", envelope)
	}
	var covered sql.NullInt64
	if err := p.st.DB().QueryRowContext(ctx,
		`SELECT covered_turn FROM email_messages WHERE id=?`, envelope.ID).Scan(&covered); err != nil {
		t.Fatal(err)
	}
	if !covered.Valid || covered.Int64 != 0 {
		t.Fatalf("covered_turn = %+v, want 0", covered)
	}

	// The delivery field: the messages route decorates the emailed turn.
	apiSrv := api.New(p.st, api.Options{})
	apiAddr, err := apiSrv.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = apiSrv.Shutdown(ctx) })
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+apiAddr+"/v1/conversations/"+convID+"/messages", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("messages route = %d: %s", resp.StatusCode, raw)
	}
	var body struct {
		Messages []conversations.Message `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	var sawUser, sawDelivery bool
	for _, m := range body.Messages {
		if m.Role == conversations.RoleUser {
			sawUser = true
			if m.Delivery != nil {
				t.Fatalf("user turn decorated: %+v", m.Delivery)
			}
			continue
		}
		if m.RunID == run.ID && m.Delivery != nil {
			sawDelivery = true
			if m.Delivery.Channel != "email" || m.Delivery.MessageID != envelope.MessageID || m.Delivery.SentAt == "" {
				t.Fatalf("delivery = %+v", m.Delivery)
			}
		}
	}
	if !sawUser || !sawDelivery {
		t.Fatalf("messages: user=%v delivery=%v", sawUser, sawDelivery)
	}
}

func startMailPlane(t *testing.T) *mailPlane {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	// The stock fake exits after its result, so one turn fires the turn-end
	// hook twice and races the tests. This script stays alive like a real CLI.
	t.Setenv(agentwire.FakeScriptEnv, `while IFS= read -r line; do
echo '{"type":"assistant","text":"working"}'
echo '{"type":"result","subtype":"success","text":"done"}'
done`)
	t.Cleanup(func() { home.SetBase("") })

	// One workspace with a workspace doc and one agent under agents/.
	wsPath := filepath.Join(dir, "ws")
	agentPath := filepath.Join(wsPath, "agents", "research")
	if err := os.MkdirAll(agentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "TREBI.md"),
		[]byte("---\nharness: fake\n---\n\nAssistant.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentPath, "AGENT.md"),
		[]byte("---\nharness: fake\nemail:\n  accept:\n    - alice@example.com\n---\n\nResearch things.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	fw := &mailWorkerFake{token: "test-token-0000"}
	wsSrv := httptest.NewServer(http.HandlerFunc(fw.serve))
	t.Cleanup(wsSrv.Close)

	// Shadow keeps every producer quiet; a minute keeps every ticker slow.
	// The test drives the poller by hand.
	cp := daemon.New(st, daemon.Options{
		Shadow:            false,
		ScheduleInterval:  time.Minute,
		QueueInterval:     50 * time.Millisecond,
		AlertsInterval:    time.Minute,
		TurnWatchInterval: time.Minute,
		EmailInterval:     time.Minute,
	})
	ctx := context.Background()
	added, err := cp.Workspaces.Add(ctx, wsPath, "ellis")
	if err != nil {
		t.Fatal(err)
	}
	// Pin the assistant harness, so auto does not probe the CLIs on PATH.
	// Append keeps the id that Add wrote.
	yml, err := os.OpenFile(filepath.Join(wsPath, "trebi.yaml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := yml.WriteString("assistant:\n  harness: fake\n"); err != nil {
		t.Fatal(err)
	}
	if err := yml.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cp.Workspaces.RebuildIndex(ctx, added); err != nil {
		t.Fatal(err)
	}
	cp.Email.Configure(&email.Settings{
		Worker: wsSrv.URL, Token: "test-token-0000", Domain: "mail.example.com",
		Admins: []string{"ops@example.com"}, PollInterval: time.Minute,
		RequireDMARC: true, FromName: "Trebi",
	}, email.NewClient(wsSrv.URL, "test-token-0000"))
	addr, err := cp.Email.EnsureAddress(ctx, added.ID, "ellis")
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	cp.Start(runCtx)
	t.Cleanup(func() { cancel(); cp.Stop() })

	return &mailPlane{
		cp: cp, fw: fw, st: st,
		wsID: added.ID, local: addr.LocalPart, full: addr.Full(),
	}
}

func writeMailJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// TestAssistantTurnMailsReplyOnThread proves the phase-0 fix end to end:
// a plain operator mail runs one assistant session turn, and the turn-end
// hook mails the reply on the thread. The outbound envelope carries
// run_id NULL (a session is not a run) and covered_turn, and it reaches
// sent only after the transport accepted the mail.
func TestAssistantTurnMailsReplyOnThread(t *testing.T) {
	p := startMailPlane(t)
	ctx := t.Context()
	const u = "01ARZ3NDEKTSV4RRFFQ69G5FA9"
	if n := p.poll(t, []email.IngressMessage{{
		V: 1, Seq: 1, ID: u, LocalPart: p.local,
		From: "ops@example.com", Dmarc: "pass", MessageID: "<plain@x>",
		Subject: "hello", Text: "Say hi.",
	}}); n != 1 {
		t.Fatalf("pulled %d, want 1", n)
	}
	// The sent mark follows the transport answer, so wait for the final
	// disposition, not only for the send.
	var convID, disposition, runID string
	var covered sql.NullInt64
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		err := p.st.DB().QueryRowContext(ctx, `
SELECT conversation_id, disposition, COALESCE(run_id,''), covered_turn
FROM email_messages WHERE direction='out'`).Scan(&convID, &disposition, &runID, &covered)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		if disposition == "sent" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(p.fw.sent()) == 0 {
		t.Fatal("assistant turn sent no reply mail")
	}
	if disposition != "sent" {
		t.Fatalf("disposition = %q, want sent", disposition)
	}
	if runID != "" {
		t.Fatalf("run_id = %q, want empty for the assistant session", runID)
	}
	if !covered.Valid {
		t.Fatal("covered_turn must be set once the reply is sent")
	}
	sent := p.fw.sent()
	last := sent[len(sent)-1]
	if last.To[0] != "ops@example.com" || !strings.Contains(last.Text, "--\ntrebi conversation "+convID) {
		t.Fatalf("reply mail = %+v", last)
	}
	// The mail went out once; a repeat of the hook covers the same turn.
	if err := p.cp.Email.ReplyOnThread(ctx, email.ReplyOpts{ConversationID: convID}); err != nil {
		t.Fatal(err)
	}
	if len(p.fw.sent()) != 1 {
		t.Fatalf("assistant reply sent %d times; want 1", len(p.fw.sent()))
	}
}

// TestOutboxRetriesAssistantReply: a failed assistant reply stays one
// envelope while the outbox retries it, fails after five attempts, and sends
// the same Message-ID on a manual retry. A stale claim goes out after 2m.
func TestOutboxRetriesAssistantReply(t *testing.T) {
	p := startMailPlane(t)
	ctx := t.Context()
	ob := p.cp.Outbox

	// Thread one: the turn-end send fails.
	p.fw.setFailSends(true)
	p.poll(t, []email.IngressMessage{{
		V: 1, Seq: 1, ID: "01ARZ3NDEKTSV4RRFFQ69G5FB1", LocalPart: p.local,
		From: "ops@example.com", Dmarc: "pass", MessageID: "<r1@x>",
		Subject: "one", Text: "Say hi.",
	}})
	convA := p.waitOut(t, "<r1@x>", "queued")
	rowID := p.outboxOf(t, convA)
	p.waitAttempts(t, rowID, 1)
	for i := 1; i <= 6; i++ {
		if err := ob.SendDue(ctx, time.Now().Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	row, err := ob.Get(ctx, rowID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != outbox.StatusFailed || row.Attempts != 5 {
		t.Fatalf("row = %s after %d attempts, want failed after 5", row.Status, row.Attempts)
	}
	if n := p.outRows(t, convA); n != 1 {
		t.Fatalf("envelopes = %d, want 1", n)
	}
	p.waitOut(t, "<r1@x>", "send_failed")

	p.fw.setFailSends(false)
	if _, err := ob.Retry(ctx, rowID); err != nil {
		t.Fatal(err)
	}
	if err := ob.SendDue(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	p.waitOut(t, "<r1@x>", "sent")
	sent := p.fw.sent()
	var msgID string
	if err := p.st.DB().QueryRowContext(ctx,
		`SELECT message_id FROM email_messages WHERE outbox_id=?`, rowID).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	if last := sent[len(sent)-1]; last.MessageID != msgID {
		t.Fatalf("retry message id = %q, want %q", last.MessageID, msgID)
	}

	// Thread two: a crash leaves the row claimed. It waits 2m, then it goes out.
	p.fw.setFailSends(true)
	p.poll(t, []email.IngressMessage{{
		V: 1, Seq: 2, ID: "01ARZ3NDEKTSV4RRFFQ69G5FB2", LocalPart: p.local,
		From: "ops@example.com", Dmarc: "pass", MessageID: "<r2@x>",
		Subject: "two", Text: "Say hi again.",
	}})
	convB := p.waitOut(t, "<r2@x>", "queued")
	rowB := p.outboxOf(t, convB)
	p.waitAttempts(t, rowB, 1)
	p.fw.setFailSends(false)
	if _, err := p.st.DB().ExecContext(ctx, `UPDATE outbox SET status='sending', claimed_at=? WHERE id=?`,
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"), rowB); err != nil {
		t.Fatal(err)
	}
	sends := len(p.fw.sent())
	if err := ob.SendDue(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(p.fw.sent()) != sends {
		t.Fatal("a fresh claim was sent again")
	}
	if err := ob.SendDue(ctx, time.Now().Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(p.fw.sent()) != sends+1 {
		t.Fatalf("stale claim: %d sends, want %d", len(p.fw.sent()), sends+1)
	}
	p.waitOut(t, "<r2@x>", "sent")
}

// waitSends waits until the worker holds n sends.
func (p *mailPlane) waitSends(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && len(p.fw.sent()) < n {
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(p.fw.sent()); got != n {
		t.Fatalf("sends = %d, want %d", got, n)
	}
}

// outboxOf returns the outbox row of the one outbound envelope of convID.
func (p *mailPlane) outboxOf(t *testing.T, convID string) string {
	t.Helper()
	var id string
	if err := p.st.DB().QueryRowContext(t.Context(),
		`SELECT outbox_id FROM email_messages WHERE direction='out' AND conversation_id=?`, convID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// waitAttempts waits until the worker made n attempts on the row.
func (p *mailPlane) waitAttempts(t *testing.T, rowID string, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		row, err := p.cp.Outbox.Get(t.Context(), rowID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Attempts >= n && row.Status == outbox.StatusPending {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("row %s never reached %d attempts", rowID, n)
}

// waitOut waits for the outbound reply on the thread rooted at root to
// reach disposition and returns its conversation id.
func (p *mailPlane) waitOut(t *testing.T, root, disposition string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var convID string
		err := p.st.DB().QueryRowContext(t.Context(), `
SELECT conversation_id FROM email_messages
WHERE direction='out' AND thread_root=? AND disposition=? LIMIT 1`, root, disposition).Scan(&convID)
		if err == nil {
			return convID
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s reply on thread %s", disposition, root)
	return ""
}

func (p *mailPlane) outRows(t *testing.T, convID string) int {
	t.Helper()
	var n int
	if err := p.st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM email_messages WHERE direction='out' AND conversation_id=?`, convID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
