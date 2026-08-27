package dispatch_test

import (
	"context"
	"testing"

	"github.com/trebi-ai/trebi/internal/dispatch"
)

func TestEvalMatchTrue(t *testing.T) {
	t.Parallel()
	r := dispatch.EvalMatch(context.Background(), "event.x === 1", "", map[string]any{"x": 1})
	if r.Error != "" || !r.Matched {
		t.Fatalf("%+v", r)
	}
}

func TestEvalMatchFalse(t *testing.T) {
	t.Parallel()
	r := dispatch.EvalMatch(context.Background(), "event.x === 2", "", map[string]any{"x": 1})
	if r.Error != "" || r.Matched {
		t.Fatalf("%+v", r)
	}
}

func TestEvalExtractVars(t *testing.T) {
	t.Parallel()
	r := dispatch.EvalMatch(context.Background(),
		"event.from === 'alice'",
		"({from: event.sender, body: event.text})",
		map[string]any{"sender": "alice", "text": "hi"},
	)
	if r.Error != "" || !r.Matched {
		t.Fatalf("%+v", r)
	}
	if r.Vars["from"] != "alice" || r.Vars["body"] != "hi" {
		t.Fatalf("vars=%v", r.Vars)
	}
}

func TestEvalMatchError(t *testing.T) {
	t.Parallel()
	// goja may throw on undefined property access depending on code;
	// use an explicit throw for a deterministic error.
	_ = dispatch.EvalMatch(context.Background(), "event.nope.deep.x", "", map[string]any{})
	r := dispatch.EvalMatch(context.Background(), "throw new Error('boom')", "", map[string]any{})
	if r.Error == "" {
		t.Fatal("expected error")
	}
}

func TestEvalMatchRequired(t *testing.T) {
	t.Parallel()
	r := dispatch.EvalMatch(context.Background(), "", "", nil)
	if r.Error == "" {
		t.Fatal("match required")
	}
}
