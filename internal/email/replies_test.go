package email

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/requests"
)

// TestDecorateMarksSentTurns: only a sent row with covered_turn marks its
// assistant turn; failed rows, other runs, and rows without a turn never do.
func TestDecorateMarksSentTurns(t *testing.T) {
	t.Parallel()
	s := testService(openEmailStore(t))
	configureTest(s, "https://worker.example")

	turn := 2
	other := 3
	msgs := []conversations.Message{
		{ConversationID: "conv-1", Role: conversations.RoleUser, RunID: "run-1", TurnIndex: &turn},
		{ConversationID: "conv-1", Role: conversations.RoleAssistant, RunID: "run-1", TurnIndex: &turn},
		{ConversationID: "conv-1", Role: conversations.RoleAssistant, RunID: "run-1"},                    // no turn index
		{ConversationID: "conv-1", Role: conversations.RoleAssistant, RunID: "run-2", TurnIndex: &other}, // no row
	}

	outEnvelope(t, s, "e1", "conv-1", "run-1", &turn, "<out-1@x>", "sent")
	outEnvelope(t, s, "e2", "conv-1", "run-1", &turn, "<out-2@x>", "send_failed") // failed sends never mark
	outEnvelope(t, s, "e3", "conv-1", "run-1", nil, "<out-3@x>", "sent")          // no covered turn
	outEnvelope(t, s, "e4", "conv-2", "run-1", &turn, "<out-4@x>", "sent")        // another conversation

	s.Decorate(t.Context(), msgs)
	if d := msgs[0].Delivery; d != nil {
		t.Errorf("user turn decorated: %+v", d)
	}
	d := msgs[1].Delivery
	if d == nil {
		t.Fatal("assistant turn not decorated")
	}
	if d.Channel != "email" || d.MessageID != "<out-1@x>" || d.SentAt == "" {
		t.Errorf("delivery = %+v", d)
	}
	for i, m := range msgs[2:] {
		if m.Delivery != nil {
			t.Errorf("message %d decorated: %+v", i+2, m.Delivery)
		}
	}
}

// outEnvelope inserts one outbound envelope row with the facts Decorate reads.
func outEnvelope(t *testing.T, s *Service, id, conversation, runID string, coveredTurn *int, messageID, disposition string) {
	t.Helper()
	var covered any
	if coveredTurn != nil {
		covered = *coveredTurn
	}
	if _, err := s.st.DB().ExecContext(t.Context(), `
INSERT INTO email_messages
  (id, direction, workspace_id, message_id, thread_root, recipients, disposition,
   conversation_id, run_id, covered_turn, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		id, "out", "ws_1", messageID, messageID, "ops@example.com", disposition,
		conversation, runID, covered, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
}

func TestFormAnswer(t *testing.T) {
	t.Parallel()
	review := requests.Form{Type: requests.TypeReview, Allow: []requests.Decision{requests.DecisionApprove, requests.DecisionEdit, requests.DecisionReject, requests.DecisionRespond}}
	choose := requests.Form{Type: requests.TypeChoose, Payload: requests.Payload{Options: []requests.Option{{ID: "a", Label: "Plan A"}, {ID: "b", Label: "Plan B"}}}}
	form := func(schema string) requests.Form {
		return requests.Form{Type: requests.TypeForm, Payload: requests.Payload{Schema: json.RawMessage(schema)}}
	}
	cases := []struct {
		name     string
		r        requests.Form
		text     string
		decision requests.Decision
		extra    string
	}{
		{"approve", review, "Approve.\n\n> quoted", requests.DecisionApprove, ""},
		{"yes", review, "yes", requests.DecisionApprove, ""},
		{"reject", review, "Reject", requests.DecisionReject, ""},
		{"no", review, "no thanks", requests.DecisionReject, ""},
		{"other text", review, "Make it shorter.\n> Draft", requests.DecisionRespond, "Make it shorter."},
		{"both words", review, "yes or no", requests.DecisionRespond, "yes or no"},
		{"option label", choose, "plan b\nthanks", requests.DecisionChoose, "b"},
		{"option id", choose, "a", requests.DecisionChoose, "a"},
		{"choose approve", choose, "approve", requests.DecisionRespond, "approve"},
		{"form no required", form(`{"type":"object","properties":{"n":{"type":"string"}}}`), "yes", requests.DecisionSubmit, ""},
		{"form required", form(`{"type":"object","properties":{"n":{"type":"string"}},"required":["n"]}`), "yes", requests.DecisionRespond, "yes"},
	}
	for _, c := range cases {
		a := formAnswer(c.r, c.text)
		if a.Decision != c.decision {
			t.Errorf("%s: decision %s, want %s", c.name, a.Decision, c.decision)
			continue
		}
		switch c.decision {
		case requests.DecisionRespond:
			if a.Message != c.extra {
				t.Errorf("%s: message %q", c.name, a.Message)
			}
		case requests.DecisionChoose:
			if len(a.Choices) != 1 || a.Choices[0] != c.extra {
				t.Errorf("%s: choices %v", c.name, a.Choices)
			}
		}
	}
}

func TestRequestReplyAnswers(t *testing.T) {
	t.Parallel()
	s := testService(openEmailStore(t))
	confirm := requests.Form{Type: requests.TypeConfirm, Allow: []requests.Decision{requests.DecisionApprove, requests.DecisionReject, requests.DecisionRespond}}
	acts := &fakeActions{env: testRouteEnv(), request: requests.Request{Forms: []requests.Form{confirm}}}
	g := NewIngress(s, connectors.New(s.st, nil, nil), acts, time.Minute, false)
	msg := IngressMessage{ID: "m1", From: "ops@example.com", Text: "approve"}
	g.applyItemReply(t.Context(), msg, RouteDecision{}, &replyTarget{inboxID: "in1", alertKind: "request", requestID: "R1", isItem: true})
	if len(acts.answers) != 1 || len(acts.answers[0].Forms) != 1 || acts.answers[0].Forms[0].Decision != requests.DecisionApprove || acts.answers[0].By != "email:ops@example.com" {
		t.Fatalf("answers %+v", acts.answers)
	}
	if len(acts.asks) != 0 || len(acts.messages) != 0 {
		t.Fatalf("asks %v messages %v", acts.asks, acts.messages)
	}
}

// TestRequestReplyManyForms refuses a mail answer to a request with more
// than one form.
func TestRequestReplyManyForms(t *testing.T) {
	t.Parallel()
	s := testService(openEmailStore(t))
	confirm := requests.Form{Type: requests.TypeConfirm, Allow: []requests.Decision{requests.DecisionApprove, requests.DecisionReject}}
	acts := &fakeActions{env: testRouteEnv(), request: requests.Request{Forms: []requests.Form{confirm, confirm}}}
	g := NewIngress(s, connectors.New(s.st, nil, nil), acts, time.Minute, false)
	msg := IngressMessage{ID: "m1", From: "ops@example.com", Text: "approve"}
	g.applyItemReply(t.Context(), msg, RouteDecision{}, &replyTarget{inboxID: "in1", alertKind: "request", requestID: "R1", isItem: true})
	if len(acts.answers) != 0 {
		t.Fatalf("answers %+v", acts.answers)
	}
	if len(acts.asks) != 1 || !strings.Contains(acts.asks[0], "This request has 2 forms. Open the link to answer.") {
		t.Fatalf("asks %v", acts.asks)
	}
}

// TestConversationReplyGoesToOperator sends a reply on an agent thread to
// that agent; a refused reply raises an inbox item.
func TestConversationReplyGoesToOperator(t *testing.T) {
	t.Parallel()
	s := testService(openEmailStore(t))
	acts := &fakeActions{env: testRouteEnv()}
	g := NewIngress(s, connectors.New(s.st, nil, nil), acts, time.Minute, false)
	msg := IngressMessage{ID: "m1", From: "ops@example.com", Text: "Make it shorter."}
	g.applyReply(t.Context(), msg, RouteDecision{}, &replyTarget{conversationID: "c1", operator: "ellis"})
	if len(acts.askOperators) != 1 || acts.askOperators[0] != "ellis" {
		t.Fatalf("ask operators %q", acts.askOperators)
	}

	acts = &fakeActions{env: testRouteEnv(), askErr: errors.New("refused")}
	g = NewIngress(s, connectors.New(s.st, nil, nil), acts, time.Minute, false)
	g.applyReply(t.Context(), msg, RouteDecision{}, &replyTarget{conversationID: "c1", operator: "ellis"})
	if len(acts.inbox) != 1 || acts.inbox[0] != ReasonUndelivered {
		t.Fatalf("inbox %q", acts.inbox)
	}
}
