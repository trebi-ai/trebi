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

// launchHint tells a job run how to ask a person.
const launchHint = "Requests: ask the person before an action that needs approval, or for a fact that only a person has. Do not ask for a thing that you can decide alone. Ask with:\n" +
	"  trebi request create --type <confirm|review|choose|form> --title \"<what the person decides>\"\n" +
	"confirm is one action. review is a draft that the person can change (--draft-file <file>). choose is a list (--option <id>=<label>, repeat; --multi for more than one). form is a few fields (--schema-file <file>). " +
	"Add --summary \"<Markdown context>\", --report <file.html or .md> in the working dir, and --expires-in <duration> as necessary.\n" +
	"Each create call adds one form. To ask more than one thing, call create once for each form, or once with --forms-file <file.json>. The calls of one turn join one request, and the person answers all its forms together. " +
	"After the last create, end your turn. Do not wait and do not poll. Trebi continues this session with a prompt that starts with \"[trebi]\". Its JSON blocks are the source of truth. " +
	"Run `trebi guide requests` for the decisions and their meaning."

// answerJSON is the fenced block of one form that the model reads. The
// field order is part of the prompt contract.
type answerJSON struct {
	RequestID string          `json:"request_id"`
	Idx       int             `json:"idx"`
	Type      Type            `json:"type,omitempty"`
	Title     string          `json:"title,omitempty"`
	Decision  Decision        `json:"decision"`
	Text      string          `json:"text,omitempty"`
	Message   string          `json:"message,omitempty"`
	Choices   []string        `json:"choices,omitempty"`
	Values    json.RawMessage `json:"values,omitempty"`
	Note      string          `json:"note,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

// askedJSON is one original form in the fresh-run capsule.
type askedJSON struct {
	RequestID string          `json:"request_id"`
	Idx       int             `json:"idx"`
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
// request, in creation order, and one block for each form.
func (d Delivery) Prompt() string {
	p := d.prompt(false)
	if len(p) > maxPrompt {
		p = d.prompt(true)
	}
	return p
}

// CapsulePrompt is the first prompt of a fresh run when the session cannot
// resume. It restates each form that the earlier run asked.
func (d Delivery) CapsulePrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[trebi] The earlier session is gone. You asked this in run %s. Do the step that follows these answers.\n\n", d.Run.ID)
	for _, r := range d.Requests {
		fmt.Fprintf(&b, "You asked this in request %s %q:\n\n", r.ID, r.Title)
		for _, f := range r.Forms {
			writeJSONBlock(&b, askedJSON{
				RequestID: r.ID, Idx: f.Idx, Type: f.Type, Title: f.Title, Summary: f.Summary, Target: f.Target,
				Draft: f.Payload.Draft, Options: f.Payload.Options, Multi: f.Payload.Multi, Schema: compact(f.Payload.Schema),
			})
		}
		b.WriteString("\n")
	}
	b.WriteString(d.Prompt())
	return b.String()
}

func (d Delivery) prompt(truncate bool) string {
	var b strings.Builder
	for _, r := range d.Requests {
		n := fmt.Sprintf("%d forms", len(r.Forms))
		if len(r.Forms) == 1 {
			n = "1 form"
		}
		if r.Status == StatusExpired {
			fmt.Fprintf(&b, "[trebi] Request %s %q (%s) closed with no answer.\n", r.ID, r.Title, n)
		} else {
			fmt.Fprintf(&b, "[trebi] The person answered request %s %q (%s).\n", r.ID, r.Title, n)
		}
		b.WriteString(answeredLine(r.Status, r.Answered))
		if truncate {
			fmt.Fprintf(&b, "The answers are too long for this prompt. Run `trebi request get %s` for the full answers.\n", r.ID)
		}
		b.WriteString("\n")
		for _, f := range r.Forms {
			a := FormAnswer{Decision: DecisionExpired}
			if f.Answer != nil && r.Status == StatusAnswered {
				a = *f.Answer
			}
			fmt.Fprintf(&b, "Form %d %q (%s)\n", f.Idx+1, f.Title, f.Type)
			fmt.Fprintf(&b, "Decision: %s. %s\n", a.Decision, instruction(f.Type, a.Decision))
			j := answerJSON{RequestID: r.ID, Idx: f.Idx, Decision: a.Decision, Truncated: truncate}
			if !truncate {
				j = answerJSON{
					RequestID: r.ID, Idx: f.Idx, Type: f.Type, Title: f.Title, Decision: a.Decision,
					Text: a.Text, Message: a.Message, Choices: a.Choices, Values: compact(a.Values), Note: a.Note,
				}
			}
			writeJSONBlock(&b, j)
			b.WriteString("\n")
		}
	}
	b.WriteString("Continue the task with these answers.\n")
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

func answeredLine(st Status, a *Answered) string {
	if a == nil {
		a = &Answered{}
	}
	at := a.At.UTC().Format(time.RFC3339)
	if st == StatusExpired {
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
