package requests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// maxPrompt is the size above which each answer JSON is cut to its id.
const maxPrompt = 32 << 10

// answerJSON is the fenced block that the model reads. The field order is
// part of the prompt contract.
type answerJSON struct {
	RequestID   string          `json:"request_id"`
	Type        Type            `json:"type,omitempty"`
	Title       string          `json:"title,omitempty"`
	Decision    Decision        `json:"decision"`
	Text        string          `json:"text,omitempty"`
	Message     string          `json:"message,omitempty"`
	Choices     []string        `json:"choices,omitempty"`
	Values      json.RawMessage `json:"values,omitempty"`
	Note        string          `json:"note,omitempty"`
	AnsweredBy  string          `json:"answered_by,omitempty"`
	AnsweredVia string          `json:"answered_via,omitempty"`
	AnsweredAt  string          `json:"answered_at,omitempty"`
	Truncated   bool            `json:"truncated,omitempty"`
}

// askedJSON is the original request in the fresh-run capsule.
type askedJSON struct {
	RequestID string          `json:"request_id"`
	Type      Type            `json:"type"`
	Title     string          `json:"title"`
	Summary   string          `json:"summary,omitempty"`
	Target    string          `json:"target,omitempty"`
	Draft     *Draft          `json:"draft,omitempty"`
	Options   []Option        `json:"options,omitempty"`
	Multi     bool            `json:"multi,omitempty"`
	Schema    json.RawMessage `json:"schema,omitempty"`
}

// Prompt is the first prompt of the resumed session: one section for each
// answer, in creation order.
func (d Delivery) Prompt() string {
	p := d.prompt(false)
	if len(p) > maxPrompt {
		p = d.prompt(true)
	}
	return p
}

// CapsulePrompt is the first prompt of a fresh run when the session cannot
// resume. It restates what the earlier run asked.
func (d Delivery) CapsulePrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[trebi] The earlier session is gone. You asked this in run %s. Do the step that follows this answer.\n\n", d.Run.ID)
	for _, r := range d.Requests {
		fmt.Fprintf(&b, "You asked this in request %s:\n\n", r.ID)
		writeJSONBlock(&b, askedJSON{
			RequestID: r.ID, Type: r.Type, Title: r.Title, Summary: r.Summary, Target: r.Target,
			Draft: r.Payload.Draft, Options: r.Payload.Options, Multi: r.Payload.Multi, Schema: compact(r.Payload.Schema),
		})
		b.WriteString("\n")
	}
	b.WriteString(d.Prompt())
	return b.String()
}

func (d Delivery) prompt(truncate bool) string {
	var b strings.Builder
	n := len(d.Requests)
	if n == 1 {
		b.WriteString("[trebi] The person answered 1 request. This session continues with the answer.\n\n")
	} else {
		fmt.Fprintf(&b, "[trebi] The person answered %d requests. This session continues with the answers.\n\n", n)
	}
	for _, r := range d.Requests {
		a := Answer{Decision: DecisionExpired}
		if r.Answer != nil {
			a = *r.Answer
		}
		fmt.Fprintf(&b, "Request %s  %q (%s)\n", r.ID, r.Title, r.Type)
		fmt.Fprintf(&b, "Decision: %s. %s\n", a.Decision, instruction(r.Type, a.Decision))
		b.WriteString(answeredLine(a))
		if truncate {
			fmt.Fprintf(&b, "The answer is too long for this prompt. Run `trebi request get %s` for the full answer.\n", r.ID)
		}
		b.WriteString("\n")
		j := answerJSON{RequestID: r.ID, Decision: a.Decision, Truncated: truncate}
		if !truncate {
			j = answerJSON{
				RequestID: r.ID, Type: r.Type, Title: r.Title, Decision: a.Decision,
				Text: a.Text, Message: a.Message, Choices: a.Choices, Values: compact(a.Values), Note: a.Note,
				AnsweredBy: a.By, AnsweredVia: a.Via,
			}
			if !a.At.IsZero() {
				j.AnsweredAt = a.At.UTC().Format(time.RFC3339)
			}
		}
		writeJSONBlock(&b, j)
		b.WriteString("\n")
	}
	if n == 1 {
		b.WriteString("Continue the task with this answer.\n")
	} else {
		b.WriteString("Continue the task with these answers.\n")
	}
	return b.String()
}

// instruction is the fixed line for one decision (plan B.2).
func instruction(t Type, d Decision) string {
	switch d {
	case DecisionApprove:
		if t == TypeReview {
			return "Do the action as proposed. Use your original draft."
		}
		return "Do the action as proposed."
	case DecisionEdit:
		return "The person changed your draft. Use the text in the JSON below exactly. Do not use your original draft."
	case DecisionReject:
		return "Do not do the action. Read the note. Finish, or make a new request."
	case DecisionRespond:
		return "Nothing is approved. Read the message and continue."
	case DecisionChoose:
		return "Continue with the chosen options."
	case DecisionSubmit:
		return "Continue with the values."
	case DecisionExpired:
		return "Nothing is approved. The person did not answer in time."
	}
	return ""
}

func answeredLine(a Answer) string {
	at := a.At.UTC().Format(time.RFC3339)
	if a.Decision == DecisionExpired {
		return fmt.Sprintf("Closed by Trebi at %s.\n", at)
	}
	who := "the user"
	if a.By == ByAssistant {
		who = "the assistant"
	}
	where := map[string]string{ViaUI: "in the inbox", ViaCLI: "with the CLI", ViaEmail: "by email", ViaAsst: "through the assistant"}[a.Via]
	if where == "" {
		where = "in Trebi"
	}
	return fmt.Sprintf("Answered by %s %s at %s.\n", who, where, at)
}

func writeJSONBlock(b *strings.Builder, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v) // the value types always encode
	b.WriteString("```json\n")
	b.Write(buf.Bytes())
	b.WriteString("```\n")
}

// compact drops the whitespace of raw JSON so the indent is the encoder's own.
func compact(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}
