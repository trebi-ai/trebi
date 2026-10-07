package chains

import (
	"fmt"
	"strings"
)

// Text limits of Part I.
const (
	maxText  = 8 << 10
	maxReply = 16 << 10
)

// askMessage is the wake message of the target of an ask or a hand-off.
func askMessage(kind Kind, hopID, from, fromRun string, fromTurn int, text, context string) string {
	var b strings.Builder
	origin := fmt.Sprintf("hop %s", hopID)
	if fromRun != "" {
		origin += fmt.Sprintf(", from run %s turn %d", fromRun, fromTurn)
	}
	who := "agents/" + from
	if from == "" {
		who, from = "A person", "the person"
	}
	if kind == KindHandoff {
		fmt.Fprintf(&b, "%s hands this work to you (%s). %s does not wait for a reply. You own the result.\n\n", who, origin, from)
	} else {
		fmt.Fprintf(&b, "%s asks you (%s):\n\n", who, origin)
	}
	b.WriteString(strings.TrimSpace(text))
	if c := strings.TrimSpace(context); c != "" {
		fmt.Fprintf(&b, "\n\nContext from %s:\n%s", from, c)
	}
	if kind == KindAsk {
		fmt.Fprintf(&b, "\n\nYour last message in this turn is your reply. The daemon sends it to %s. Keep it short and complete. Do not ask %s questions. If you need a person, use a request.", from, from)
	}
	return b.String()
}

// replyPrompt is the part of a resume prompt that carries one reply.
func replyPrompt(from, hopID, runID string, turn int, text string) string {
	return fmt.Sprintf("Reply from agents/%s (hop %s, run %s turn %d):\n\n%s", from, hopID, runID, turn, cutReply(text, hopID))
}

// cutReply bounds a reply in a prompt.
func cutReply(text, hopID string) string {
	text = strings.TrimSpace(text)
	if len(text) <= maxReply {
		return text
	}
	cut := text[:maxReply]
	for len(cut) > 0 && !validEnd(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "\n\n(Cut. Read all of it with: trebi chain hop " + hopID + ")"
}

// validEnd reports whether s does not end inside a UTF-8 sequence.
func validEnd(s string) bool {
	return strings.ToValidUTF8(s, "\x00") == s
}

// askedMessage is the tool result for an agent that asks.
func askedMessage(kind Kind, to, hopID string) string {
	if kind == KindHandoff {
		return fmt.Sprintf("Handed off to %s (hop %s). %s owns the work now. Do not wait for a reply.", to, hopID, to)
	}
	return fmt.Sprintf("Asked %s (hop %s). End your turn now. The reply from %s starts your next turn.", to, hopID, to)
}

// noReplyText is the reply of a turn that wrote no text.
func noReplyText(agent string) string {
	return fmt.Sprintf("(agents/%s ended the turn with no reply.)", agent)
}

// failedReplyText is the reply of a turn that failed.
func failedReplyText(agent, reason string) string {
	return fmt.Sprintf("(agents/%s did not answer: %s.)", agent, reason)
}

// stoppedReplyText is the reply that each waiting agent gets when a person
// stops the chain.
const stoppedReplyText = "(The chain was stopped by a person. Finish with what you have. Do not ask other agents.)"

// limitText is the refusal of a hop at a chain limit with no request.
const limitText = "The chain reached its limit. Finish with what you have and reply."
