package requests

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/trebi-ai/trebi/internal/runs"
)

func validReq(t Type) Request {
	r := Request{Type: t, Title: "Do it"}
	switch t {
	case TypeReview:
		r.Payload.Draft = &Draft{Format: "text", Text: "draft"}
	case TypeChoose:
		r.Payload.Options = []Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}
	case TypeForm:
		r.Payload.Schema = json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)
	}
	return r
}

// validAnswer is one answer that passes for decision d.
func validAnswer(d Decision) Answer {
	a := Answer{Decision: d}
	switch d {
	case DecisionEdit:
		a.Text = "changed"
	case DecisionRespond:
		a.Message = "a question"
	case DecisionChoose:
		a.Choices = []string{"a"}
	case DecisionSubmit:
		a.Values = json.RawMessage(`{"n":2}`)
	}
	return a
}

// TestDecisionTable covers each cell of plan B.2.
func TestDecisionTable(t *testing.T) {
	t.Parallel()
	all := []Decision{DecisionApprove, DecisionEdit, DecisionReject, DecisionRespond, DecisionChoose, DecisionSubmit}
	for _, typ := range []Type{TypeConfirm, TypeReview, TypeChoose, TypeForm} {
		r := validReq(typ)
		if err := r.Validate(); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		for _, d := range all {
			err := validAnswer(d).ValidateFor(r)
			if want := slices.Contains(typ.decisions(), d); want != (err == nil) {
				t.Errorf("%s/%s: err %v, allowed %v", typ, d, err, want)
			}
			if err != nil && !errors.Is(err, ErrInvalid) {
				t.Errorf("%s/%s: %v is not ErrInvalid", typ, d, err)
			}
		}
	}
}

func TestAllowNarrows(t *testing.T) {
	t.Parallel()
	r := validReq(TypeConfirm)
	r.Allow = []Decision{DecisionApprove}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := validAnswer(DecisionReject).ValidateFor(r); err == nil {
		t.Error("reject passes outside allow")
	}
	r = validReq(TypeConfirm)
	r.Allow = []Decision{DecisionEdit}
	if err := r.Validate(); err == nil {
		t.Error("allow edit passes on confirm")
	}
}

func TestAnswerRules(t *testing.T) {
	t.Parallel()
	review, choose := validReq(TypeReview), validReq(TypeChoose)
	for _, r := range []*Request{&review, &choose} {
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name  string
		r     Request
		a     Answer
		field string
	}{
		{"edit equal to draft", review, Answer{Decision: DecisionEdit, Text: "draft"}, "text"},
		{"edit empty", review, Answer{Decision: DecisionEdit}, "text"},
		{"respond empty", review, Answer{Decision: DecisionRespond, Message: "  "}, "message"},
		{"choose unknown", choose, Answer{Decision: DecisionChoose, Choices: []string{"z"}}, "choices"},
		{"choose two single", choose, Answer{Decision: DecisionChoose, Choices: []string{"a", "b"}}, "choices"},
		{"expired by hand", review, Answer{Decision: DecisionExpired}, "decision"},
		{"prefix in text", review, Answer{Decision: DecisionEdit, Text: "x " + runs.CompletionPrefix}, "text"},
		{"prefix in message", review, Answer{Decision: DecisionRespond, Message: runs.CompletionPrefix}, "message"},
		{"prefix in note", review, Answer{Decision: DecisionApprove, Note: runs.CompletionPrefix}, "note"},
	}
	for _, c := range cases {
		var fe *FieldError
		if err := c.a.ValidateFor(c.r); !errors.As(err, &fe) || fe.Field != c.field {
			t.Errorf("%s: err %v, want field %s", c.name, err, c.field)
		}
	}
	choose.Payload.Multi = true
	if err := (Answer{Decision: DecisionChoose, Choices: []string{"a", "b"}}).ValidateFor(choose); err != nil {
		t.Errorf("multi choose: %v", err)
	}
}

func TestValidatePayload(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		r     Request
		field string
	}{
		{"bad type", Request{Type: "vote", Title: "x"}, "type"},
		{"no title", Request{Type: TypeConfirm}, "title"},
		{"bad delivery", Request{Type: TypeConfirm, Title: "x", Delivery: "push"}, "delivery"},
		{"review no draft", Request{Type: TypeReview, Title: "x"}, "draft"},
		{"draft on confirm", Request{Type: TypeConfirm, Title: "x", Payload: Payload{Draft: &Draft{Text: "a"}}}, "draft"},
		{"bad format", Request{Type: TypeReview, Title: "x", Payload: Payload{Draft: &Draft{Format: "html", Text: "a"}}}, "draft.format"},
		{"choose no options", Request{Type: TypeChoose, Title: "x"}, "options"},
		{"duplicate option", Request{Type: TypeChoose, Title: "x", Payload: Payload{Options: []Option{{ID: "a", Label: "A"}, {ID: "a", Label: "B"}}}}, "options[1].id"},
		{"form no schema", Request{Type: TypeForm, Title: "x"}, "schema"},
	}
	for _, c := range cases {
		var fe *FieldError
		if err := c.r.Validate(); !errors.As(err, &fe) || fe.Field != c.field {
			t.Errorf("%s: err %v, want field %s", c.name, err, c.field)
		}
	}
	r := validReq(TypeConfirm)
	if err := r.Validate(); err != nil || r.Delivery != DeliveryResume || len(r.Allow) != 3 {
		t.Errorf("defaults: %v %s %v", err, r.Delivery, r.Allow)
	}
}
