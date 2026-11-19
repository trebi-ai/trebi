package routing

import "strings"

// Message is the part of a message that a rule reads.
type Message struct {
	From, Subject, Text, Room string
}

// Fits reports whether the message meets the condition of the rule. Text
// compares without case; a room compares exactly.
func (r Rule) Fits(m Message) bool {
	switch r.Field {
	case RuleSenderIs:
		return strings.EqualFold(strings.TrimSpace(m.From), r.Value)
	case RuleSenderEnds:
		return strings.HasSuffix(strings.ToLower(strings.TrimSpace(m.From)), strings.ToLower(r.Value))
	case RuleSubjectHas:
		return containsFold(m.Subject, r.Value)
	case RuleTextHas:
		return containsFold(m.Text, r.Value)
	case RuleRoomIs:
		return m.Room == r.Value
	}
	return false
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
