// Package decisions asks a model fixed questions about a piece of content
// and maps the answers to an outcome through a policy. It has no email
// code; email is the first policy.
package decisions

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
)

// QuestionType is boolean, choice, or score.
type QuestionType string

const (
	TypeBoolean QuestionType = "boolean"
	TypeChoice  QuestionType = "choice"
	TypeScore   QuestionType = "score"
)

// Question is one fixed question of a policy.
type Question struct {
	ID       string       `json:"id"`
	Type     QuestionType `json:"type"`
	Text     string       `json:"text"`
	Label    string       `json:"label"` // UI text for a yes answer, e.g. "Asks for data"
	Criteria *Criteria    `json:"criteria,omitempty"`
	Options  []Option     `json:"options,omitempty"` // choice: any order; score: lowest first
	// Shares asks for a share of 0 to 1 for each option. It needs a choice.
	Shares bool `json:"shares,omitempty"`
}

// Criteria explain a yes and a no answer of a boolean question.
type Criteria struct {
	True  string `json:"true"`
	False string `json:"false"`
}

// Option is one value of a choice or score question.
type Option struct {
	Key         string `json:"key"`
	Description string `json:"description"`
	// Ref is the id behind the key, for a caller that maps a key back. The
	// prompt never shows it.
	Ref string `json:"ref,omitempty"`
}

// State is the content to decide on.
type State struct {
	Text      string            `json:"text"`                // capped at maxStateText characters
	Untrusted bool              `json:"untrusted,omitempty"` // fenced as outside data
	Facts     map[string]string `json:"facts,omitempty"`     // trusted context
	Choices   []Option          `json:"choices,omitempty"`   // trusted options for a policy with Build
}

// Subject is the object that a decision is about.
type Subject struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Scope string `json:"scope,omitempty"` // for example the connection of an event
}

// Request is one call to a decider.
type Request struct {
	Workspace string
	Kind      string
	Subject   Subject
	TraceID   string
	State     State
	Questions []Question
	Repair    string // the validation error of the first call; empty on the first call
}

// Answer is the answer to one question.
type Answer struct {
	ID           string             `json:"id"`
	Value        string             `json:"value"`
	Distribution map[string]float64 `json:"distribution,omitempty"`
	Confidence   float64            `json:"confidence"`
	Margin       float64            `json:"margin,omitempty"`
	Level        int                `json:"level,omitempty"`
	Reason       string             `json:"reason,omitempty"`
	Method       Method             `json:"method,omitempty"`
}

// Method is how an answer was made.
type Method string

const (
	MethodLogprob Method = "logprob"
	MethodVote    Method = "vote"
	MethodVerbal  Method = "verbal"
	MethodRule    Method = "rule"
)

const (
	maxStateText = 20000
	maxReason    = 200
)

var (
	questionIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	optionKeyRE  = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
)

// Validate checks the shape of a question.
func (q Question) Validate() error {
	if !questionIDRE.MatchString(q.ID) {
		return fmt.Errorf("question id %q must match %s", q.ID, questionIDRE)
	}
	if q.Text == "" {
		return fmt.Errorf("question %s has no text", q.ID)
	}
	switch q.Type {
	case TypeBoolean:
		if len(q.Options) > 0 {
			return fmt.Errorf("boolean question %s has options", q.ID)
		}
	case TypeChoice, TypeScore:
		if len(q.Options) < 2 {
			return fmt.Errorf("question %s needs at least two options", q.ID)
		}
		seen := map[string]bool{}
		for _, o := range q.Options {
			if !optionKeyRE.MatchString(o.Key) || seen[o.Key] {
				return fmt.Errorf("question %s: option %q must match %s and be unique", q.ID, o.Key, optionKeyRE)
			}
			seen[o.Key] = true
		}
	default:
		return fmt.Errorf("question %s: type %q is not boolean, choice, or score", q.ID, q.Type)
	}
	if q.Shares && q.Type != TypeChoice {
		return fmt.Errorf("question %s: shares need a choice question", q.ID)
	}
	return nil
}

// Values is the set of answer values of q.
func (q Question) Values() []string {
	if q.Type == TypeBoolean {
		return []string{"true", "false"}
	}
	out := make([]string, len(q.Options))
	for i, o := range q.Options {
		out[i] = o.Key
	}
	return out
}

// Check validates one answer against q.
func (q Question) Check(a Answer) error {
	vals := q.Values()
	if (!q.Shares || a.Value != "") && !slices.Contains(vals, a.Value) {
		return fmt.Errorf("%s: value %q is not one of %v", q.ID, a.Value, vals)
	}
	if a.Confidence < 0 || a.Confidence > 1 || math.IsNaN(a.Confidence) {
		return fmt.Errorf("%s: confidence %v is not from 0 to 1", q.ID, a.Confidence)
	}
	if len(a.Distribution) == 0 {
		if q.Shares {
			return fmt.Errorf("%s: give a distribution with a share for each option", q.ID)
		}
		return nil
	}
	var sum float64
	for k, p := range a.Distribution {
		if !slices.Contains(vals, k) {
			return fmt.Errorf("%s: distribution key %q is not a value", q.ID, k)
		}
		if q.Shares && (p < 0 || p > 1 || math.IsNaN(p)) {
			return fmt.Errorf("%s: share %v of %q is not from 0 to 1", q.ID, p, k)
		}
		sum += p
	}
	if math.Abs(sum-1) > 0.01 {
		return fmt.Errorf("%s: distribution sums to %v, not 1", q.ID, sum)
	}
	return nil
}

// Normalize completes an answer to a shares question that passed Check: a
// missing option gets 0, the value is the largest share (the first option on
// a tie), and the confidence and margin follow from the shares.
func (q Question) Normalize(a Answer) Answer {
	dist := make(map[string]float64, len(q.Options))
	for _, o := range q.Options {
		dist[o.Key] = a.Distribution[o.Key]
	}
	top, second := -1.0, 0.0
	for _, o := range q.Options {
		switch p := dist[o.Key]; {
		case p > top:
			a.Value, second, top = o.Key, max(top, 0), p
		case p > second:
			second = p
		}
	}
	a.Distribution, a.Confidence, a.Margin = dist, top, top-second
	return a
}

// unsafe is the value that a low-confidence answer counts as: yes for a
// boolean, the highest level for a score. A choice has none.
func (q Question) unsafe() string {
	switch q.Type {
	case TypeBoolean:
		return "true"
	case TypeScore:
		return q.Options[len(q.Options)-1].Key
	}
	return ""
}

// checkAnswers validates one answer for each question.
func checkAnswers(qs []Question, as []Answer) error {
	by := map[string]Answer{}
	for _, a := range as {
		by[a.ID] = a
	}
	var errs []error
	for _, q := range qs {
		a, ok := by[q.ID]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: no answer", q.ID))
			continue
		}
		if err := q.Check(a); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
