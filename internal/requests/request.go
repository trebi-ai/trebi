// Package requests owns the decision points that a job run opens for a
// person. The run parks; the answer resumes its session.
package requests

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/runs"
)

type Type string

const (
	TypeConfirm Type = "confirm"
	TypeReview  Type = "review"
	TypeChoose  Type = "choose"
	TypeForm    Type = "form"
)

type Decision string

const (
	DecisionApprove Decision = "approve"
	DecisionEdit    Decision = "edit"
	DecisionReject  Decision = "reject"
	DecisionRespond Decision = "respond"
	DecisionChoose  Decision = "choose"
	DecisionSubmit  Decision = "submit"
	DecisionExpired Decision = "expired"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusAnswered  Status = "answered"
	StatusExpired   Status = "expired"
	StatusDismissed Status = "dismissed"
	StatusCancelled Status = "cancelled"
)

type DeliveryMode string

const (
	DeliveryResume DeliveryMode = "resume"
	DeliveryPoll   DeliveryMode = "poll"
)

type DeliveryState string

const (
	DeliveryNone      DeliveryState = "none"
	DeliveryClaimed   DeliveryState = "claimed"
	DeliveryDelivered DeliveryState = "delivered"
	DeliveryFailed    DeliveryState = "failed"
)

// Who answered, and through which surface.
const (
	ByUser      = "user"
	ByAssistant = "assistant"
	ViaUI       = "ui"
	ViaCLI      = "cli"
	ViaEmail    = "email"
	ViaAsst     = "assistant"
	ViaDaemon   = "daemon"
)

var (
	ErrNotFound = errors.New("request not found")
	ErrClosed   = errors.New("request is closed")
	ErrConflict = errors.New("request revision changed")
	ErrNotRun   = errors.New("only a job run can create a request")
	ErrRunToken = errors.New("a job run cannot answer a request")
	ErrPlan     = errors.New("requests need Trebi Pro")
	ErrInvalid  = errors.New("invalid request")
)

// FieldError is a validation failure on one field. It wraps ErrInvalid.
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Reason }
func (e *FieldError) Unwrap() error { return ErrInvalid }

func invalid(field, format string, args ...any) error {
	return &FieldError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

type Draft struct {
	Format string `json:"format"` // text | markdown
	Text   string `json:"text"`
}

type Option struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

type Payload struct {
	Draft   *Draft          `json:"draft,omitempty"`
	Options []Option        `json:"options,omitempty"`
	Multi   bool            `json:"multi,omitempty"`
	Schema  json.RawMessage `json:"schema,omitempty"`
}

type Answer struct {
	Decision Decision        `json:"decision"`
	Text     string          `json:"text,omitempty"`
	Message  string          `json:"message,omitempty"`
	Choices  []string        `json:"choices,omitempty"`
	Values   json.RawMessage `json:"values,omitempty"`
	Note     string          `json:"note,omitempty"`
	By       string          `json:"by"`
	Via      string          `json:"via"`
	At       time.Time       `json:"at"`
}

type Request struct {
	ID              string        `json:"id"`
	RunID           string        `json:"run_id"`
	WorkspaceID     string        `json:"workspace"`
	Job             string        `json:"job"`
	Kind            string        `json:"kind"`
	SessionID       string        `json:"session_id,omitempty"`
	Harness         string        `json:"harness,omitempty"`
	Runtime         string        `json:"runtime,omitempty"`
	WorkingDir      string        `json:"working_dir,omitempty"`
	ConversationKey string        `json:"conversation_key,omitempty"`
	ConversationID  string        `json:"conversation_id,omitempty"`
	Type            Type          `json:"type"`
	Title           string        `json:"title"`
	Summary         string        `json:"summary,omitempty"`
	Target          string        `json:"target,omitempty"`
	Payload         Payload       `json:"payload"`
	Allow           []Decision    `json:"allow"`
	ReportKind      string        `json:"report_kind,omitempty"`
	ReportSHA256    string        `json:"report_sha256,omitempty"`
	Delivery        DeliveryMode  `json:"delivery"`
	Status          Status        `json:"status"`
	Answer          *Answer       `json:"answer,omitempty"`
	ExpiresAt       *time.Time    `json:"expires_at,omitempty"`
	DeliveryState   DeliveryState `json:"delivery_state"`
	ClaimedAt       *time.Time    `json:"-"`
	DeliveryPath    string        `json:"delivery_path,omitempty"`
	DeliveredRunID  string        `json:"delivered_run_id,omitempty"`
	DeliveryError   string        `json:"delivery_error,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	Revision        int           `json:"revision"`
}

const (
	maxTitle   = 200
	maxText    = 256 << 10
	maxOptions = 50
)

// decisions lists what each type supports, in display order.
func (t Type) decisions() []Decision {
	switch t {
	case TypeConfirm:
		return []Decision{DecisionApprove, DecisionReject, DecisionRespond}
	case TypeReview:
		return []Decision{DecisionApprove, DecisionEdit, DecisionReject, DecisionRespond}
	case TypeChoose:
		return []Decision{DecisionChoose, DecisionReject, DecisionRespond}
	case TypeForm:
		return []Decision{DecisionSubmit, DecisionReject, DecisionRespond}
	}
	return nil
}

// Open reports whether the request still waits for a person.
func (r Request) Open() bool { return r.Status == StatusPending }

// Validate checks the type, the payload, and the allowed decisions. It fills
// Allow with the type default when it is empty.
func (r *Request) Validate() error {
	supported := r.Type.decisions()
	if supported == nil {
		return invalid("type", "must be confirm, review, choose, or form")
	}
	if strings.TrimSpace(r.Title) == "" {
		return invalid("title", "is required")
	}
	if len(r.Title) > maxTitle {
		return invalid("title", "is longer than %d bytes", maxTitle)
	}
	switch r.Delivery {
	case "":
		r.Delivery = DeliveryResume
	case DeliveryResume, DeliveryPoll:
	default:
		return invalid("delivery", "must be resume or poll")
	}
	if err := r.Payload.validate(r.Type); err != nil {
		return err
	}
	if len(r.Allow) == 0 {
		r.Allow = supported
		return nil
	}
	for _, d := range r.Allow {
		if !slices.Contains(supported, d) {
			return invalid("allow", "%s is not a decision of a %s request", d, r.Type)
		}
	}
	return nil
}

func (p Payload) validate(t Type) error {
	if p.Draft != nil && t != TypeReview {
		return invalid("draft", "is only for a review request")
	}
	if len(p.Options) > 0 && t != TypeChoose {
		return invalid("options", "are only for a choose request")
	}
	if len(p.Schema) > 0 && t != TypeForm {
		return invalid("schema", "is only for a form request")
	}
	switch t {
	case TypeReview:
		return p.Draft.validate()
	case TypeChoose:
		return validateOptions(p.Options)
	case TypeForm:
		_, err := ParseSchema(p.Schema)
		return err
	}
	return nil
}

// validate checks a review draft and sets the default format.
func (d *Draft) validate() error {
	if d == nil {
		return invalid("draft", "is required for a review request")
	}
	if d.Format == "" {
		d.Format = "text"
	}
	if d.Format != "text" && d.Format != "markdown" {
		return invalid("draft.format", "must be text or markdown")
	}
	if len(d.Text) > maxText {
		return invalid("draft.text", "is longer than %d bytes", maxText)
	}
	return nil
}

func validateOptions(opts []Option) error {
	if len(opts) == 0 {
		return invalid("options", "are required for a choose request")
	}
	if len(opts) > maxOptions {
		return invalid("options", "has more than %d entries", maxOptions)
	}
	seen := map[string]bool{}
	for i, o := range opts {
		if o.ID == "" || o.Label == "" {
			return invalid(fmt.Sprintf("options[%d]", i), "needs an id and a label")
		}
		if seen[o.ID] {
			return invalid(fmt.Sprintf("options[%d].id", i), "%q is used twice", o.ID)
		}
		seen[o.ID] = true
	}
	return nil
}

// ValidateFor checks an answer against the B.2 table of the plan and the
// allowed decisions of r.
func (a Answer) ValidateFor(r Request) error {
	if a.Decision == DecisionExpired {
		return invalid("decision", "expired is set by the daemon")
	}
	if !slices.Contains(r.Allow, a.Decision) {
		return invalid("decision", "%s is not allowed for this request", a.Decision)
	}
	if err := a.validateText(); err != nil {
		return err
	}
	switch a.Decision {
	case DecisionEdit:
		if a.Text == "" {
			return invalid("text", "is required for edit")
		}
		if r.Payload.Draft != nil && a.Text == r.Payload.Draft.Text {
			return invalid("text", "is equal to the draft; use approve")
		}
	case DecisionRespond:
		if strings.TrimSpace(a.Message) == "" {
			return invalid("message", "is required for respond")
		}
	case DecisionChoose:
		return a.validateChoices(r.Payload)
	case DecisionSubmit:
		s, err := ParseSchema(r.Payload.Schema)
		if err != nil {
			return err
		}
		return s.ValidateValues(a.Values)
	}
	return nil
}

// validateText refuses the completion line and oversize text in the free
// text fields, so an answer cannot end the resumed turn.
func (a Answer) validateText() error {
	for _, f := range []struct{ name, v string }{{"text", a.Text}, {"message", a.Message}, {"note", a.Note}} {
		if strings.Contains(f.v, runs.CompletionPrefix) {
			return invalid(f.name, "must not contain %s", runs.CompletionPrefix)
		}
		if len(f.v) > maxText {
			return invalid(f.name, "is longer than %d bytes", maxText)
		}
	}
	return nil
}

func (a Answer) validateChoices(p Payload) error {
	if len(a.Choices) == 0 {
		return invalid("choices", "needs at least one option id")
	}
	if len(a.Choices) > 1 && !p.Multi {
		return invalid("choices", "only one option is allowed")
	}
	for _, c := range a.Choices {
		if !slices.ContainsFunc(p.Options, func(o Option) bool { return o.ID == c }) {
			return invalid("choices", "unknown option %q", c)
		}
	}
	return nil
}
