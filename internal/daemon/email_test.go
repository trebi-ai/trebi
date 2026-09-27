package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// mailWorkerFake serves the trebi-mail contract the poller drives: ingress
// with a live cursor, acks, sends, and health.
type mailWorkerFake struct {
	token     string
	batch     []email.IngressMessage
	sends     []email.SendRequest
	acks      int
	failSends bool
}

func (f *mailWorkerFake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
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
	p.fw.batch = batch
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
	if p.fw.acks != 1 {
		t.Fatalf("acks = %d, want 1", p.fw.acks)
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
	alertMsgID, err := p.cp.Email.SendAlert(ctx, inbox, "[trebi] approval requested", "Reply approve or deny.")
	if err != nil {
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
	pointer, err := p.cp.Conversations.GetPointer(ctx, p.wsID, conversations.ChannelEmail, "<ask@x>")
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
	sendsBefore := len(p.fw.sends)
	if err := p.cp.Email.ReplyOnThread(ctx, email.ReplyOpts{RunID: run.ID, ConversationID: convID}); err != nil {
		t.Fatal(err)
	}
	if len(p.fw.sends) != sendsBefore+1 {
		t.Fatalf("sends = %d, want %d", len(p.fw.sends), sendsBefore+1)
	}
	// A repeat covers the same turn: no second mail for one exchange.
	if err := p.cp.Email.ReplyOnThread(ctx, email.ReplyOpts{RunID: run.ID, ConversationID: convID}); err != nil {
		t.Fatal(err)
	}
	if len(p.fw.sends) != sendsBefore+1 {
		t.Fatalf("second reply sent again: %d sends", len(p.fw.sends))
	}
	last := p.fw.sends[len(p.fw.sends)-1]
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
	if err := cp.Workspaces.RebuildIndex(ctx, added); err != nil {
		t.Fatal(err)
	}
	cp.Email.Configure(&email.Settings{
		Worker: wsSrv.URL, Token: "test-token-0000", Domain: "mail.example.com",
		Operators: []string{"ops@example.com"}, PollInterval: time.Minute,
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

// TestUnsentReplyRowsDoNotCoverTheTurn proves the restart safety of the
// reply path: only a `sent` row covers a turn, so a failed send retries,
// and a row left `sending` by a crash does not suppress the next mail.
func TestUnsentReplyRowsDoNotCoverTheTurn(t *testing.T) {
	p := startMailPlane(t)
	ctx := t.Context()
	const u = "01ARZ3NDEKTSV4RRFFQ69G5FA8"

	// The send fails: the envelope records send_failed and covers nothing.
	p.fw.failSends = true
	if n := p.poll(t, []email.IngressMessage{{
		V: 1, Seq: 1, ID: u, LocalPart: p.local,
		From: "ops@example.com", Dmarc: "pass", MessageID: "<retry@x>",
		Subject: "hello", Text: "Say hi.",
	}}); n != 1 {
		t.Fatalf("pulled %d, want 1", n)
	}
	deadline := time.Now().Add(8 * time.Second)
	var convID, disposition string
	for time.Now().Before(deadline) {
		err := p.st.DB().QueryRowContext(ctx, `
SELECT conversation_id, disposition FROM email_messages
WHERE direction='out' ORDER BY id DESC LIMIT 1`).Scan(&convID, &disposition)
		if err == nil && disposition == "send_failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if disposition != "send_failed" {
		t.Fatalf("disposition = %q, want send_failed", disposition)
	}

	// The transport recovers: the same turn goes out on the next call.
	p.fw.failSends = false
	sends := len(p.fw.sends)
	if err := p.cp.Email.ReplyOnThread(ctx, email.ReplyOpts{ConversationID: convID}); err != nil {
		t.Fatal(err)
	}
	if len(p.fw.sends) != sends+1 {
		t.Fatalf("a send_failed row covered the turn: %d sends", len(p.fw.sends))
	}
	var covered sql.NullInt64
	if err := p.st.DB().QueryRowContext(ctx, `
SELECT disposition, covered_turn FROM email_messages
WHERE direction='out' ORDER BY id DESC LIMIT 1`).Scan(&disposition, &covered); err != nil {
		t.Fatal(err)
	}
	if disposition != "sent" || !covered.Valid {
		t.Fatalf("retry row = %q covered=%v", disposition, covered)
	}

	// A crash between the envelope insert and the sent mark leaves
	// `sending`, which covers nothing: the next call mails the turn again.
	if _, err := p.st.DB().ExecContext(ctx,
		`UPDATE email_messages SET disposition='sending' WHERE direction='out'`); err != nil {
		t.Fatal(err)
	}
	sends = len(p.fw.sends)
	if err := p.cp.Email.ReplyOnThread(ctx, email.ReplyOpts{ConversationID: convID}); err != nil {
		t.Fatal(err)
	}
	if len(p.fw.sends) != sends+1 {
		t.Fatalf("a sending row covered the turn: %d sends", len(p.fw.sends))
	}

	// Now the row is sent: one mail per exchange.
	if err := p.cp.Email.ReplyOnThread(ctx, email.ReplyOpts{ConversationID: convID}); err != nil {
		t.Fatal(err)
	}
	if len(p.fw.sends) != sends+1 {
		t.Fatalf("a second reply went out for one exchange: %d sends", len(p.fw.sends))
	}
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
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && len(p.fw.sends) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if len(p.fw.sends) == 0 {
		t.Fatal("assistant turn sent no reply mail")
	}
	var convID, disposition, runID string
	var covered sql.NullInt64
	if err := p.st.DB().QueryRowContext(ctx, `
SELECT conversation_id, disposition, COALESCE(run_id,''), covered_turn
FROM email_messages WHERE direction='out'`).Scan(&convID, &disposition, &runID, &covered); err != nil {
		t.Fatal(err)
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
	last := p.fw.sends[len(p.fw.sends)-1]
	if last.To[0] != "ops@example.com" || !strings.Contains(last.Text, "--\ntrebi conversation "+convID) {
		t.Fatalf("reply mail = %+v", last)
	}
	// The mail went out once; a repeat of the hook covers the same turn.
	if err := p.cp.Email.ReplyOnThread(ctx, email.ReplyOpts{ConversationID: convID}); err != nil {
		t.Fatal(err)
	}
	if len(p.fw.sends) != 1 {
		t.Fatalf("assistant reply sent %d times; want 1", len(p.fw.sends))
	}
}
