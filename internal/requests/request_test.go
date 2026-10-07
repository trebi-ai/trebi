package requests

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/runs"
)

// TestDecisionTable covers each cell of plan B.2.
func TestDecisionTable(t *testing.T) {
	t.Parallel()
	all := []Decision{DecisionApprove, DecisionEdit, DecisionReject, DecisionRespond, DecisionChoose, DecisionSubmit}
	for _, typ := range []Type{TypeConfirm, TypeReview, TypeChoose, TypeForm} {
		r := validForm(typ)
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
	r := validForm(TypeConfirm)
	r.Allow = []Decision{DecisionApprove}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := validAnswer(DecisionReject).ValidateFor(r); err == nil {
		t.Error("reject passes outside allow")
	}
	r = validForm(TypeConfirm)
	r.Allow = []Decision{DecisionEdit}
	if err := r.Validate(); err == nil {
		t.Error("allow edit passes on confirm")
	}
}

func TestAnswerRules(t *testing.T) {
	t.Parallel()
	review, choose := validForm(TypeReview), validForm(TypeChoose)
	for _, r := range []*Form{&review, &choose} {
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name  string
		r     Form
		a     FormAnswer
		field string
	}{
		{"edit equal to draft", review, FormAnswer{Decision: DecisionEdit, Text: "draft"}, "text"},
		{"edit empty", review, FormAnswer{Decision: DecisionEdit}, "text"},
		{"respond empty", review, FormAnswer{Decision: DecisionRespond, Message: "  "}, "message"},
		{"choose unknown", choose, FormAnswer{Decision: DecisionChoose, Choices: []string{"z"}}, "choices"},
		{"choose two single", choose, FormAnswer{Decision: DecisionChoose, Choices: []string{"a", "b"}}, "choices"},
		{"expired by hand", review, FormAnswer{Decision: DecisionExpired}, "decision"},
		{"prefix in text", review, FormAnswer{Decision: DecisionEdit, Text: "x " + runs.CompletionPrefix}, "text"},
		{"prefix in message", review, FormAnswer{Decision: DecisionRespond, Message: runs.CompletionPrefix}, "message"},
		{"prefix in note", review, FormAnswer{Decision: DecisionApprove, Note: runs.CompletionPrefix}, "note"},
	}
	for _, c := range cases {
		var fe *FieldError
		if err := c.a.ValidateFor(c.r); !errors.As(err, &fe) || fe.Field != c.field {
			t.Errorf("%s: err %v, want field %s", c.name, err, c.field)
		}
	}
	choose.Payload.Multi = true
	if err := (FormAnswer{Decision: DecisionChoose, Choices: []string{"a", "b"}}).ValidateFor(choose); err != nil {
		t.Errorf("multi choose: %v", err)
	}
}

func TestValidatePayload(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		r     Form
		field string
	}{
		{"bad type", Form{Type: "vote", Title: "x"}, "type"},
		{"no title", Form{Type: TypeConfirm}, "title"},
		{"review no draft", Form{Type: TypeReview, Title: "x"}, "draft"},
		{"draft on confirm", Form{Type: TypeConfirm, Title: "x", Payload: Payload{Draft: &Draft{Text: "a"}}}, "draft"},
		{"bad format", Form{Type: TypeReview, Title: "x", Payload: Payload{Draft: &Draft{Format: "html", Text: "a"}}}, "draft.format"},
		{"choose no options", Form{Type: TypeChoose, Title: "x"}, "options"},
		{"duplicate option", Form{Type: TypeChoose, Title: "x", Payload: Payload{Options: []Option{{ID: "a", Label: "A"}, {ID: "a", Label: "B"}}}}, "options[1].id"},
		{"form no schema", Form{Type: TypeForm, Title: "x"}, "schema"},
	}
	for _, c := range cases {
		var fe *FieldError
		if err := c.r.Validate(); !errors.As(err, &fe) || fe.Field != c.field {
			t.Errorf("%s: err %v, want field %s", c.name, err, c.field)
		}
	}
	r := validForm(TypeConfirm)
	if err := r.Validate(); err != nil || len(r.Allow) != 3 {
		t.Errorf("defaults: %v %v", err, r.Allow)
	}
}

// TestRequestValidate checks the form count and names the bad form.
func TestRequestValidate(t *testing.T) {
	t.Parallel()
	forms := func(n int) []Form {
		out := make([]Form, n)
		for i := range out {
			out[i] = validForm(TypeConfirm)
			out[i].Idx = i
		}
		return out
	}
	bad := forms(2)
	bad[1].Type = "vote"
	cases := []struct {
		name  string
		r     Request
		field string
	}{
		{"no forms", Request{Title: "x"}, "forms"},
		{"too many forms", Request{Title: "x", Forms: forms(maxForms + 1)}, "forms"},
		{"no title", Request{Forms: forms(1)}, "title"},
		{"bad form", Request{Title: "x", Forms: bad}, "forms[1].type"},
	}
	for _, c := range cases {
		var fe *FieldError
		if err := c.r.Validate(); !errors.As(err, &fe) || fe.Field != c.field {
			t.Errorf("%s: err %v, want field %s", c.name, err, c.field)
		}
	}
	r := Request{Title: "x", Forms: forms(maxForms)}
	if err := r.Validate(); err != nil {
		t.Errorf("%d forms: %v", maxForms, err)
	}
}

// TestValidateAnswers needs one valid answer for each form.
func TestValidateAnswers(t *testing.T) {
	t.Parallel()
	r := Request{Title: "x", Forms: []Form{validForm(TypeConfirm), validForm(TypeChoose)}}
	r.Forms[1].Idx = 1
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	approve := FormAnswerIn{Idx: 0, FormAnswer: validAnswer(DecisionApprove)}
	choose := FormAnswerIn{Idx: 1, FormAnswer: validAnswer(DecisionChoose)}
	cases := []struct {
		name string
		in   []FormAnswerIn
		want string
	}{
		{"all", []FormAnswerIn{approve, choose}, ""},
		{"missing", []FormAnswerIn{approve}, "form 1 has no answer"},
		{"twice", []FormAnswerIn{approve, approve, choose}, "form 0 is answered twice"},
		{"unknown", []FormAnswerIn{approve, choose, {Idx: 5, FormAnswer: validAnswer(DecisionApprove)}}, "form 5 does not exist"},
		{"bad", []FormAnswerIn{approve, {Idx: 1, FormAnswer: validAnswer(DecisionApprove)}}, "form 1: decision"},
	}
	for _, c := range cases {
		err := r.ValidateAnswers(c.in)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}

func validForm(t Type) Form {
	r := Form{Type: t, Title: "Do it"}
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
func validAnswer(d Decision) FormAnswer {
	a := FormAnswer{Decision: d}
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
