package email

import (
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/conversations"
)

// TestDecorateMarksSentTurns: only a sent row with covered_turn marks its
// assistant turn; failed rows, other runs, and rows without a turn never do.
func TestDecorateMarksSentTurns(t *testing.T) {
	t.Parallel()
	s := New(openEmailStore(t))
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
