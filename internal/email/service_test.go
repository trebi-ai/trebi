package email

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/license"
	"github.com/trebi-ai/trebi/internal/store"
)

// Email is not enforced today, so Has is always true in-process and the
// refusal path is covered by the license package tests.
func TestServiceUnconfigured(t *testing.T) {
	t.Parallel()
	s := New(openEmailStore(t), license.New(nil))

	st := s.Status(t.Context())
	if st.Configured || st.Entitlement != "ok" || st.WorkerHealth != nil {
		t.Errorf("status with no block = %+v", st)
	}
	if _, err := s.Client(); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("client = %v, want ErrNotConfigured", err)
	}
	if err := s.SyncAddresses(t.Context(), []WorkspaceRef{{ID: "ws_1", Name: "One"}}); err != nil {
		t.Errorf("sync with no block must be a no-op: %v", err)
	}
}

func TestEnsureRotateSync(t *testing.T) {
	t.Parallel()
	s := New(openEmailStore(t), nil)
	s.Configure(testSettings("https://worker.example"))

	a, err := s.EnsureAddress(t.Context(), "ws_1", "Ellis Labs")
	if err != nil {
		t.Fatal(err)
	}
	if a.Domain != "mail.example.com" || !strings.HasPrefix(a.LocalPart, "ellis-labs-") {
		t.Fatalf("minted = %+v", a)
	}
	if len(a.LocalPart) != len("ellis-labs-")+tokenLen {
		t.Fatalf("local part %q: wrong length", a.LocalPart)
	}

	// Second call is idempotent.
	again, err := s.EnsureAddress(t.Context(), "ws_1", "Renamed")
	if err != nil || again.LocalPart != a.LocalPart {
		t.Fatalf("re-ensure = %+v,%v; slug must be frozen", again, err)
	}

	// Rotate keeps the slug, changes the token, bumps the revision.
	rot, err := s.Rotate(t.Context(), "ws_1", "Renamed")
	if err != nil {
		t.Fatal(err)
	}
	if rot.Revision != 2 || rot.RotatedAt == "" {
		t.Fatalf("rotated = %+v", rot)
	}
	if !strings.HasPrefix(rot.LocalPart, "ellis-labs-") {
		t.Fatalf("rotate lost the slug: %q", rot.LocalPart)
	}
	if rot.LocalPart == a.LocalPart {
		t.Fatalf("rotate kept the old token: %q", rot.LocalPart)
	}

	// Sync mints for workspaces that lack one, skips those that have one.
	if err := s.SyncAddresses(t.Context(), []WorkspaceRef{
		{ID: "ws_1", Name: "Renamed"},
		{ID: "ws_2", Name: "Two"},
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.AddressCount(t.Context()); n != 2 {
		t.Fatalf("address count = %d, want 2", n)
	}
	known, err := s.KnownLocalParts(t.Context())
	if err != nil || len(known) != 2 {
		t.Fatalf("known = %v,%v", known, err)
	}

	// Masked hides the token; the agent subaddress carries the tag.
	m, err := s.Masked(t.Context(), "ws_1")
	if err != nil {
		t.Fatal(err)
	}
	if m.LocalPart != "ellis-labs-********" {
		t.Errorf("masked = %q", m.LocalPart)
	}
	agent, err := s.AgentAddress(t.Context(), "ws_1", "research")
	if err != nil || agent != "ellis-labs-********+research@mail.example.com" {
		t.Errorf("agent address = %q,%v", agent, err)
	}
	if _, err := s.AgentAddress(t.Context(), "ws_9", "research"); err != nil || agent == "" {
		t.Errorf("agent address on unknown workspace = %q,%v", agent, err)
	}
}

func TestStatusProbesWorker(t *testing.T) {
	t.Parallel()
	fw := &fakeWorker{token: "test-token-0000"}
	fw.health = Health{Version: "1.0.0", SpoolDepth: 0, KnownCount: 1}
	srv := httptest.NewServer(fw.handler())
	t.Cleanup(srv.Close)

	s := New(openEmailStore(t), nil)
	s.Configure(testSettings(srv.URL))
	s.SetPollState(PollState{Position: 41, LastPoll: "2026-09-19T09:00:00Z"})

	st := s.Status(t.Context())
	if !st.Configured || st.Worker != srv.URL || st.Entitlement != "ok" {
		t.Errorf("status = %+v", st)
	}
	if st.WorkerHealth == nil || st.WorkerHealth.Version != "1.0.0" {
		t.Errorf("worker health = %+v err=%q", st.WorkerHealth, st.WorkerError)
	}
	if st.Position != 41 || st.PollIntervalMS != 30000 || !st.RequireDMARC {
		t.Errorf("loop facts missing: %+v", st)
	}

	// A dead Worker is reported, not returned.
	s.Configure(testSettings("http://127.0.0.1:1"))
	st = s.Status(t.Context())
	if st.WorkerError == "" || st.WorkerHealth != nil {
		t.Errorf("worker error = %q, health = %+v", st.WorkerError, st.WorkerHealth)
	}
}

func TestTestSendsAndLogs(t *testing.T) {
	t.Parallel()
	fw := &fakeWorker{token: "test-token-0000", sentID: "<t1@mail.example.com>"}
	srv := httptest.NewServer(fw.handler())
	t.Cleanup(srv.Close)

	st := openEmailStore(t)
	s := New(st, nil)
	s.Configure(testSettings(srv.URL))

	msgID, err := s.Test(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msgID, "<") || !strings.HasSuffix(msgID, "@mail.example.com>") {
		t.Errorf("message id = %q", msgID)
	}

	var sent SendRequest
	if err := json.Unmarshal(fw.lastSend, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.From != "trebi@mail.example.com" || len(sent.To) != 1 || sent.To[0] != "ops@example.com" {
		t.Errorf("test send body = %+v", sent)
	}
	if sent.MessageID != msgID {
		t.Errorf("send message id %q != returned %q", sent.MessageID, msgID)
	}

	var direction, disposition, recipients string
	err = st.DB().QueryRowContext(t.Context(),
		`SELECT direction, disposition, recipients FROM email_messages WHERE direction = 'out'`).
		Scan(&direction, &disposition, &recipients)
	if err != nil {
		t.Fatalf("envelope row: %v", err)
	}
	if direction != "out" || disposition != "sent" || recipients != "ops@example.com" {
		t.Errorf("envelope = %s/%s/%s", direction, disposition, recipients)
	}

	// A failed send still logs the envelope, with the failure named.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(dead.Close)
	s.Configure(testSettings(dead.URL))
	if _, err := s.Test(t.Context()); err == nil {
		t.Error("send to a dead worker must fail")
	}
	var d2 string
	if err := st.DB().QueryRowContext(t.Context(),
		`SELECT disposition FROM email_messages WHERE disposition = 'send_failed'`).Scan(&d2); err != nil {
		t.Fatalf("send_failed envelope: %v", err)
	}
}

func TestSendAlertThreadsAndRecovers(t *testing.T) {
	t.Parallel()
	fw := &fakeWorker{token: "test-token-0000"}
	srv := httptest.NewServer(fw.handler())
	t.Cleanup(srv.Close)

	st := openEmailStore(t)
	s := New(st, nil)
	s.Configure(testSettings(srv.URL))

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB().ExecContext(t.Context(),
		`INSERT INTO inbox_items (id, kind, title, revision, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		"inbox-1", "failure", "Run failed", 1, now, now); err != nil {
		t.Fatal(err)
	}
	const subject = "[trebi] Run failed: job/ws-1/deploy"

	// First mail on the thread: no In-Reply-To, its own id is the root.
	first, err := s.SendAlert(t.Context(), "inbox-1", subject, "kind: run_failed")
	if err != nil {
		t.Fatal(err)
	}
	var sent SendRequest
	if err := json.Unmarshal(fw.lastSend, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.MessageID != first || sent.InReplyTo != "" || len(sent.References) != 0 {
		t.Errorf("first send = %+v", sent)
	}
	if inReplyTo, root, disp := envelope(t, st, first); inReplyTo != "" || root != first || disp != "sent" {
		t.Errorf("first envelope = %q/%q/%s", inReplyTo, root, disp)
	}
	var link string
	if err := st.DB().QueryRowContext(t.Context(),
		`SELECT email_message_id FROM inbox_items WHERE id='inbox-1'`).Scan(&link); err != nil || link != first {
		t.Errorf("inbox link = %q,%v; want the minted id", link, err)
	}
	if n := busCount(t, st, "email.sent"); n != 1 {
		t.Errorf("email.sent rows = %d, want 1", n)
	}

	// A repeat of the same fault threads under the first root.
	second, err := s.SendAlert(t.Context(), "inbox-1", subject, "kind: run_failed")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fw.lastSend, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.InReplyTo != first || len(sent.References) != 1 || sent.References[0] != first {
		t.Errorf("second send = %+v", sent)
	}
	if inReplyTo, root, _ := envelope(t, st, second); inReplyTo != first || root != first {
		t.Errorf("second envelope = %q root %q", inReplyTo, root)
	}

	// A 429 is retried, not failed.
	fw.sendScript = []int{http.StatusTooManyRequests}
	third, err := s.SendAlert(t.Context(), "inbox-1", subject, "kind: run_failed")
	if err != nil {
		t.Fatalf("send after one 429: %v", err)
	}

	// A permanent failure marks the row send_failed and surfaces in Status.
	fw.sendScript = []int{http.StatusInternalServerError}
	failed, err := s.SendAlert(t.Context(), "inbox-1", subject, "kind: run_failed")
	if err == nil {
		t.Fatal("permanent send failure must error")
	}
	if _, root, disp := envelope(t, st, failed); root != first || disp != "send_failed" {
		t.Errorf("failed envelope root=%q disposition=%s", root, disp)
	}
	if se := s.Status(t.Context()).SendError; se == "" {
		t.Error("send failure missing from Status")
	}

	// The next success clears the failure and threads on the last sent mail.
	fw.sendScript = nil
	if _, err := s.SendAlert(t.Context(), "inbox-1", subject, "kind: run_failed"); err != nil {
		t.Fatal(err)
	}
	if se := s.Status(t.Context()).SendError; se != "" {
		t.Errorf("send error not cleared: %q", se)
	}
	if err := json.Unmarshal(fw.lastSend, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.InReplyTo != third {
		t.Errorf("recovery send = %+v; a send_failed row must not thread", sent)
	}
}

// TestSendRecipientMatrix drives the explicit send (plan I.3): operators
// pass, the workspace allowlist passes through a wired ingress, and anyone
// else is refused before an envelope row or a Worker call exists.
func TestSendRecipientMatrix(t *testing.T) {
	t.Parallel()
	fw := &fakeWorker{token: "test-token-0000"}
	srv := httptest.NewServer(fw.handler())
	t.Cleanup(srv.Close)

	st := openEmailStore(t)
	s := New(st, nil)
	s.Configure(testSettings(srv.URL))
	if _, err := s.EnsureAddress(t.Context(), "ws_1", "Ellis Labs"); err != nil {
		t.Fatal(err)
	}
	// The allowlist rides the routing environment; only a wired ingress has it.
	s.AttachIngress(NewIngress(s, connectors.New(st, nil, nil, nil),
		&fakeActions{env: RouteEnv{Allowlist: []string{"Friend@Example.com"}}}, time.Minute, false))

	// An operator passes; the mail carries the plain workspace address.
	first, err := s.Send(t.Context(), SendOpts{
		WorkspaceID: "ws_1", To: []string{"ops@example.com"},
		Subject: "digest", Text: "ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sent SendRequest
	if err := json.Unmarshal(fw.lastSend, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.From == "" || strings.Contains(sent.From, "+") || !strings.HasSuffix(sent.From, "@mail.example.com") {
		t.Errorf("from = %q; want the plain workspace address", sent.From)
	}
	if sent.MessageID != first {
		t.Errorf("message id %q != returned %q", sent.MessageID, first)
	}
	if inReplyTo, root, disp := envelope(t, st, first); inReplyTo != "" || root != first || disp != "sent" {
		t.Errorf("envelope = %q/%q/%s", inReplyTo, root, disp)
	}

	// The allowlist folds case and trims, like the operator list.
	if _, err := s.Send(t.Context(), SendOpts{
		WorkspaceID: "ws_1", To: []string{" friend@example.com "},
		Subject: "digest", Text: "ready",
	}); err != nil {
		t.Fatalf("allowlisted recipient: %v", err)
	}

	// An agent send rides the agent subaddress.
	if _, err := s.Send(t.Context(), SendOpts{
		WorkspaceID: "ws_1", Agent: "research", To: []string{"ops@example.com"},
		Subject: "digest", Text: "ready",
	}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fw.lastSend, &sent); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(sent.From, "+research@mail.example.com") {
		t.Errorf("agent from = %q", sent.From)
	}

	// A reply threads onto the root of the message it answers.
	threaded, err := s.Send(t.Context(), SendOpts{
		WorkspaceID: "ws_1", To: []string{"ops@example.com"},
		Subject: "digest", Text: "ready", InReplyTo: first,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fw.lastSend, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.InReplyTo != first || len(sent.References) == 0 || sent.References[len(sent.References)-1] != first {
		t.Errorf("threaded send = %+v", sent)
	}
	if inReplyTo, root, _ := envelope(t, st, threaded); inReplyTo != first || root != first {
		t.Errorf("threaded envelope = %q/%q", inReplyTo, root)
	}

	// A stranger mixed into the recipient list refuses the whole send.
	if _, err := s.Send(t.Context(), SendOpts{
		WorkspaceID: "ws_1", To: []string{"ops@example.com", "stranger@example.net"},
		Subject: "digest", Text: "ready",
	}); !errors.Is(err, ErrRecipient) {
		t.Fatalf("stranger = %v, want ErrRecipient", err)
	}
	var rows int
	if err := st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM email_messages WHERE recipients LIKE '%stranger%'`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("refused send wrote %d envelope rows, err %v", rows, err)
	}
	if err := st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM email_messages WHERE direction='out'`).Scan(&rows); err != nil || rows != 4 {
		t.Errorf("out rows = %d err %v, want the 4 accepted sends", rows, err)
	}
}

// TestSendAlertFromWorkspaceAddress: an alert for a workspace leaves from
// that workspace's address, so a reply routes back through the Worker. A
// sender of trebi@<domain> has no <slug>-<token> shape, so the Worker drops
// the reply before the known-set check (plan A.10, Phase 5).
func TestSendAlertFromWorkspaceAddress(t *testing.T) {
	t.Parallel()
	fw := &fakeWorker{token: "test-token-0000"}
	srv := httptest.NewServer(fw.handler())
	t.Cleanup(srv.Close)

	st := openEmailStore(t)
	s := New(st, nil)
	s.Configure(testSettings(srv.URL))
	addr, err := s.EnsureAddress(t.Context(), "ws_1", "Ellis Labs")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for _, tc := range []struct{ id, workspace, want string }{
		{"inbox-ws", "ws_1", addr.Full()},
		{"inbox-none", "", "trebi@mail.example.com"},
	} {
		if _, err := st.DB().ExecContext(t.Context(),
			`INSERT INTO inbox_items (id, kind, title, workspace_id, revision, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?)`,
			tc.id, "failure", "Run failed", store.NullStr(tc.workspace), 1, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SendAlert(t.Context(), tc.id, "[trebi] Run failed: "+tc.id, "kind: run_failed"); err != nil {
			t.Fatal(err)
		}
		var sent SendRequest
		if err := json.Unmarshal(fw.lastSend, &sent); err != nil {
			t.Fatal(err)
		}
		if sent.From != tc.want {
			t.Errorf("%s: from = %q, want %q", tc.id, sent.From, tc.want)
		}
	}

	// The workspace address carries the <slug>-<token> shape the Worker
	// needs to match a reply back to the workspace.
	if !strings.Contains(addr.LocalPart, "-") {
		t.Errorf("local part %q has no separator; a reply cannot route", addr.LocalPart)
	}
}

func envelope(t *testing.T, st *store.Store, messageID string) (inReplyTo, root, disposition string) {
	t.Helper()
	err := st.DB().QueryRowContext(t.Context(),
		`SELECT COALESCE(in_reply_to,''), thread_root, disposition FROM email_messages WHERE message_id=?`,
		messageID).Scan(&inReplyTo, &root, &disposition)
	if err != nil {
		t.Fatalf("envelope %s: %v", messageID, err)
	}
	return inReplyTo, root, disposition
}

func busCount(t *testing.T, st *store.Store, topic string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM event_log WHERE topic=?`, topic).Scan(&n); err != nil {
		t.Fatalf("bus count %s: %v", topic, err)
	}
	return n
}

func openEmailStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func testSettings(worker string) *Settings {
	return &Settings{
		Worker:       worker,
		Token:        "test-token-0000",
		Domain:       "mail.example.com",
		Operators:    []string{"ops@example.com"},
		PollInterval: 30 * time.Second,
		RequireDMARC: true,
		FromName:     "Trebi",
	}
}
