package dispatch_test

import (
	"context"
	"encoding/json"
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

// TestEvalMatchJSONNumber proves a payload decoded with UseNumber reads as
// JS numbers. JS itself rounds an integer above 2^53.
func TestEvalMatchJSONNumber(t *testing.T) {
	t.Parallel()
	ev := map[string]any{
		"n":     json.Number("7"),
		"ratio": json.Number("0.5"),
		"list":  []any{json.Number("1")},
		"id":    json.Number("1234567890123456789"),
	}
	for _, m := range []string{"event.n === 7", "event.n + 1 === 8", "event.ratio === 0.5", "event.list[0] === 1", "typeof event.id === 'number'"} {
		if r := dispatch.EvalMatch(context.Background(), m, "", ev); r.Error != "" || !r.Matched {
			t.Fatalf("%s: %+v", m, r)
		}
	}
	r := dispatch.EvalMatch(context.Background(), "true", "({n: event.n})", ev)
	if r.Vars["n"] != "7" {
		t.Fatalf("vars = %v", r.Vars)
	}
	if _, ok := ev["n"].(json.Number); !ok {
		t.Fatal("EvalMatch changed the caller's payload")
	}
}
