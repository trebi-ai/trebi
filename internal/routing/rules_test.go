package routing_test

import (
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/routing"
)

func TestRuleFits(t *testing.T) {
	t.Parallel()
	m := routing.Message{From: "Ann@Stripe.com", Subject: "Invoice 42", Text: "Please pay", Room: "INBOX"}
	tests := []struct {
		field, value string
		want         bool
	}{
		{routing.RuleSenderIs, "ann@stripe.com", true},
		{routing.RuleSenderIs, "stripe.com", false},
		{routing.RuleSenderEnds, "@STRIPE.com", true},
		{routing.RuleSenderEnds, "@x.io", false},
		{routing.RuleSubjectHas, "invoice", true},
		{routing.RuleSubjectHas, "pay", false},
		{routing.RuleTextHas, "PAY", true},
		{routing.RuleRoomIs, "INBOX", true},
		{routing.RuleRoomIs, "inbox", false},
		{"", "x", false},
	}
	for _, tc := range tests {
		if got := (routing.Rule{Field: tc.field, Value: tc.value}).Fits(m); got != tc.want {
			t.Errorf("%s %q = %v", tc.field, tc.value, got)
		}
	}
}

func TestBatchWithChosenIsInvalid(t *testing.T) {
	t.Parallel()
	f := routing.Parse("gmail", "", []byte("mode: on\nroutes:\n  - agent: a\n    gets: chosen\n    batch: 2m\n"))
	if len(f.Problems) != 1 || f.Problems[0].Code != routing.CodeInvalidRoute || !strings.Contains(f.Problems[0].Message, "chosen") {
		t.Fatalf("problems = %+v", f.Problems)
	}
}

func TestOldIndexPrefixIsInvalid(t *testing.T) {
	t.Parallel()
	for _, src := range []string{
		"mode: on\nroutes:\n  - agent: agents/ops\n    gets: chosen\n",
		"mode: on\nroutes:\n  - job: agents/ops/report\n    gets: chosen\n",
	} {
		f := routing.Parse("gmail", "", []byte(src))
		if len(f.Problems) != 1 || f.Problems[0].Code != routing.CodeInvalidRoute || !strings.Contains(f.Problems[0].Message, "remove the agents/ prefix") {
			t.Errorf("problems = %+v for %q", f.Problems, src)
		}
	}
	f := routing.Parse("gmail", "", []byte("mode: on\nroutes:\n  - job: ops/report\n    gets: chosen\n"))
	if len(f.Problems) != 0 {
		t.Fatalf("new form: %+v", f.Problems)
	}
}
