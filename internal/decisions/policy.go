package decisions

import (
	"context"
	"strconv"

	"github.com/trebi-ai/trebi/internal/pricing"
)

// Outcome is what a policy decides, for example deliver or hold.
type Outcome string

// Policy is one kind of decision: its questions, its rules, and the map
// from answers to an outcome.
type Policy struct {
	Kind      string
	Version   int
	Label     string  // "Email reply check", for History
	Subject   string  // subject kind
	Safe      Outcome // the outcome when the check does not finish
	Rules     []Rule  // deterministic checks that run before the model
	Questions []Question
	Map       func(a Answers, s State) (Outcome, []string)
}

// Rule answers one question with no model. A hit wins over the model.
type Rule struct {
	Question string
	Check    func(s State) (value string, hit bool)
}

// Answers are the answers of one decision by question id.
type Answers map[string]Answer

// Yes reports a true answer with at least min confidence.
func (a Answers) Yes(id string, minConf float64) bool {
	x, ok := a[id]
	return ok && x.Value == "true" && x.Confidence >= minConf
}

// AtLeast reports a score answer at or above level. levels is the option
// order of the question.
func (a Answers) AtLeast(id, level string, levels []Option) bool {
	x, ok := a[id]
	if !ok {
		return false
	}
	want, got := -1, -1
	for i, o := range levels {
		if o.Key == level {
			want = i
		}
		if o.Key == x.Value {
			got = i
		}
	}
	return want >= 0 && got >= want
}

// FactBool reads a "true" or "false" fact.
func (s State) FactBool(key string) bool {
	b, _ := strconv.ParseBool(s.Facts[key])
	return b
}

// Decider answers the questions of one request.
type Decider interface {
	Name() string // "harness:claude", "native:<model>", "fake"
	Decide(ctx context.Context, r Request) (Result, error)
}

// Result is the reply of one decider call.
type Result struct {
	Answers []Answer
	Method  Method
	Raw     []byte // kept as a blob for debugging
	CostUSD float64
	Tokens  pricing.TokenUsage // zero when the decider reports no usage
}
