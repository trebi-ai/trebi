package email

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/store"
)

type fakeChecker struct {
	check ReplyCheck
	err   error
	calls int
}

func (f *fakeChecker) CheckReply(context.Context, RouteDecision, IngressMessage) (ReplyCheck, error) {
	f.calls++
	return f.check, f.err
}

const heldThread = "<out1@mail.example.com>"

func TestDeliverParticipantReplies(t *testing.T) {
	tests := []struct {
		name        string
		checker     *fakeChecker
		noChecker   bool
		from, dmarc string
		disposition string
		inbox       []string
		asks        int
	}{
		{name: "participant delivers", checker: &fakeChecker{check: ReplyCheck{DecisionID: "dec_1"}},
			from: "client@acme.com", dmarc: "pass", disposition: "routed", asks: 1},
		{name: "new sender is held", checker: &fakeChecker{}, from: "newbie@acme.com", dmarc: "pass",
			disposition: "held", inbox: []string{ReasonNewSenderInThread}},
		{name: "check holds", checker: &fakeChecker{check: ReplyCheck{DecisionID: "dec_1", Hold: true, Reasons: []string{"Asks for a secret"}}},
			from: "client@acme.com", dmarc: "pass", disposition: "held", inbox: []string{ReasonDecisionHold}},
		{name: "check fails", checker: &fakeChecker{check: ReplyCheck{DecisionID: "dec_1", Failed: true}},
			from: "client@acme.com", dmarc: "pass", disposition: "held", inbox: []string{ReasonDecisionError}},
		{name: "check errors", checker: &fakeChecker{err: errors.New("boom")},
			from: "client@acme.com", dmarc: "pass", disposition: "held", inbox: []string{ReasonDecisionError}},
		{name: "no checker holds", noChecker: true, from: "client@acme.com", dmarc: "pass", disposition: "held",
			inbox: []string{ReasonDecisionError}},
		{name: "dmarc fail drops", checker: &fakeChecker{}, from: "client@acme.com", dmarc: "fail", disposition: "dropped"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var checker ReplyChecker
			if !tc.noChecker {
				checker = tc.checker
			}
			s, st, acts, addr := heldSetup(t, checker)
			if err := s.Deliver(t.Context(), heldReply(addr, "r1", tc.from, tc.dmarc)); err != nil {
				t.Fatal(err)
			}
			m, err := s.GetMessage(t.Context(), "r1")
			if err != nil {
				t.Fatal(err)
			}
			if m.Disposition != tc.disposition || len(acts.asks) != tc.asks || !slices.Equal(acts.inbox, tc.inbox) {
				t.Fatalf("disposition %s asks %d inbox %v", m.Disposition, len(acts.asks), acts.inbox)
			}
			if tc.checker != nil && tc.checker.check.DecisionID != "" && m.DecisionID != "dec_1" {
				t.Fatalf("decision_id %q", m.DecisionID)
			}
			if tc.disposition == "held" && (m.Content == nil || m.Content.Text != "Thanks.") {
				t.Fatalf("a held mail keeps its text: %+v", m.Content)
			}
			if s.inThread(t.Context(), MailboxRoom("ws_1", "research"), heldThread, "newbie@acme.com") {
				t.Fatal("a held or dropped sender joined the thread")
			}
			var held int
			_ = st.DB().QueryRow(`SELECT count(*) FROM event_log WHERE topic='email.held' AND entity_id='r1'`).Scan(&held)
			if (held == 1) != (tc.disposition == "held") {
				t.Fatalf("email.held rows %d", held)
			}
		})
	}
}

func TestDeliverNotOurThreadStaysDropped(t *testing.T) {
	s, _, acts, addr := heldSetup(t, &fakeChecker{})
	msg := heldReply(addr, "r1", "client@acme.com", "pass")
	msg.InReplyTo, msg.References = "<other@acme.com>", []string{"<other@acme.com>"}
	if err := s.Deliver(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	m, _ := s.GetMessage(t.Context(), "r1")
	if m.Disposition != "dropped" || len(acts.asks)+len(acts.inbox) != 0 {
		t.Fatalf("disposition %s asks %v inbox %v", m.Disposition, acts.asks, acts.inbox)
	}
}

func TestReleaseAndIgnore(t *testing.T) {
	s, st, acts, addr := heldSetup(t, &fakeChecker{})
	for _, id := range []string{"r1", "r2", "r3"} {
		if err := s.Deliver(t.Context(), heldReply(addr, id, "newbie@acme.com", "pass")); err != nil {
			t.Fatal(err)
		}
	}
	if list, err := s.Held(t.Context(), "ws_1"); err != nil || len(list) != 3 {
		t.Fatalf("held %d, %v", len(list), err)
	}
	if _, err := s.Release(t.Context(), "r1", "always"); err == nil {
		t.Fatal("bad scope accepted")
	}
	m, err := s.Release(t.Context(), "r1", ReleaseOnce)
	if err != nil || m.Disposition != "routed" || len(acts.asks) != 1 {
		t.Fatalf("release once: %+v %v asks %d", m.Disposition, err, len(acts.asks))
	}
	room := MailboxRoom("ws_1", "research")
	if s.inThread(t.Context(), room, heldThread, "newbie@acme.com") {
		t.Fatal("release once added the sender")
	}
	if _, err := s.Release(t.Context(), "r1", ReleaseOnce); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("second release: %v", err)
	}
	if _, err := s.Release(t.Context(), "r2", ReleaseThread); err != nil || !s.inThread(t.Context(), room, heldThread, "newbie@acme.com") {
		t.Fatalf("release thread: %v", err)
	}
	if m, err := s.Ignore(t.Context(), "r3"); err != nil || m.Disposition != "dropped" {
		t.Fatalf("ignore: %+v %v", m.Disposition, err)
	}
	var received int
	_ = st.DB().QueryRow(`SELECT count(*) FROM event_log WHERE topic='email.received' AND entity_id IN ('r1','r2','r3')`).Scan(&received)
	if received != 2 {
		t.Fatalf("email.received rows %d, want 2", received)
	}
}

func TestAcceptEntry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ scope, want string }{
		{ReleaseSender, "a@acme.com"}, {ReleaseDomain, "@acme.com"}, {ReleaseOnce, ""}, {ReleaseThread, ""},
	} {
		if got, err := AcceptEntry(tc.scope, " A@Acme.com "); err != nil || got != tc.want {
			t.Fatalf("%s: %q %v", tc.scope, got, err)
		}
	}
	if _, err := AcceptEntry("all", "a@acme.com"); err == nil {
		t.Fatal("bad scope accepted")
	}
	if title, body := (RouteDecision{Reason: ReasonDecisionHold, HoldReasons: []string{"Asks for data"}}).HeldItem(
		IngressMessage{From: "a@acme.com", Subject: "Re: Quote", Text: "secret body"}, "Research"); title != "A reply from a@acme.com waits for you" ||
		body != "The safety check of the reply to 'Quote' found: Asks for data. Research does not read the mail until you release it." {
		t.Fatalf("%q %q", title, body)
	}
}

// heldSetup has an agent "research" that sent heldThread to client@acme.com.
// research accepts only @example.net, so acme.com mail is a drop by rule.
func heldSetup(t *testing.T, checker ReplyChecker) (*Service, *store.Store, *fakeActions, Address) {
	t.Helper()
	isolateHome(t)
	st := openEmailStore(t)
	s := testService(st)
	if checker != nil {
		s.WithReplyChecker(checker)
	}
	configureTest(s, "https://worker.example")
	addr, err := s.EnsureAddress(t.Context(), "ws_1", "ellis-labs")
	if err != nil {
		t.Fatal(err)
	}
	indexAgent(t, st, "ws_1", "research")
	if _, err := st.DB().Exec(`INSERT INTO email_messages (id, direction, workspace_id, agent_id, message_id, thread_root,
		recipients, subject, disposition, created_at, revision) VALUES ('out1','out','ws_1','agt_research',?,?,
		'client@acme.com','Quote','sent','2026-10-01T00:00:00Z',1)`, heldThread, heldThread); err != nil {
		t.Fatal(err)
	}
	s.touchThread(t.Context(), MailboxRoom("ws_1", "research"), heldThread, "Quote", []string{"client@acme.com"})
	acts := &fakeActions{env: testRouteEnv()}
	s.AttachIngress(NewIngress(s, connectors.New(st, nil, nil), acts, time.Minute, false))
	return s, st, acts, addr
}

func heldReply(addr Address, id, from, dmarc string) IngressMessage {
	return IngressMessage{V: 1, ID: id, LocalPart: addr.LocalPart, Tag: "research", To: []string{addr.Full()},
		From: from, Dmarc: dmarc, Subject: "Re: Quote", MessageID: "<" + id + "@acme.com>", InReplyTo: heldThread,
		References: []string{heldThread}, Text: "Thanks."}
}
