// Package dispatch evaluates match/extract JS (Goja) and owns queue/policy.
package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dop251/goja"
)

const jsStageTimeout = 2 * time.Second

// EvalResult is the fire/skip decision from match/extract.
type EvalResult struct {
	Matched bool
	Vars    map[string]string
	Error   string
}

// EvalMatch runs extract (optional) then match against event in a Goja VM.
// event is bound as `event` (parsed object or {line: "..."}).
func EvalMatch(ctx context.Context, match, extract string, event any) EvalResult {
	if match == "" {
		return EvalResult{Error: "match is required"}
	}
	type result struct {
		r EvalResult
	}
	ch := make(chan result, 1)
	// The evaluator ends when evalMatchSync returns; the buffered channel lets it
	// finish safely even when this caller returns on cancellation or timeout.
	go func() {
		ch <- result{r: evalMatchSync(match, extract, event)}
	}()
	select {
	case <-ctx.Done():
		return EvalResult{Error: "js cancelled"}
	case <-time.After(jsStageTimeout):
		return EvalResult{Error: "js stage timeout"}
	case res := <-ch:
		return res.r
	}
}

func evalMatchSync(match, extract string, event any) EvalResult {
	vm := goja.New()
	if err := vm.Set("event", event); err != nil {
		return EvalResult{Error: fmt.Sprintf("bind event: %v", err)}
	}
	// helpers
	if err := vm.Set("json", func(s string) any {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return nil
		}
		return v
	}); err != nil {
		return EvalResult{Error: fmt.Sprintf("bind json helper: %v", err)}
	}

	if extract != "" {
		v, err := vm.RunString(extract)
		if err != nil {
			return EvalResult{Error: fmt.Sprintf("extract: %v", err)}
		}
		// Re-bind event to extract result when object.
		if v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
			exported := v.Export()
			if err := vm.Set("event", exported); err != nil {
				return EvalResult{Error: fmt.Sprintf("bind extracted event: %v", err)}
			}
			// If object, collect string vars
			if m, ok := exported.(map[string]any); ok {
				vars := map[string]string{}
				for k, val := range m {
					vars[k] = fmt.Sprint(val)
				}
				// continue with match; vars returned if match fires
				matched, err := runMatch(vm, match)
				if err != nil {
					return EvalResult{Error: err.Error()}
				}
				return EvalResult{Matched: matched, Vars: vars}
			}
		}
	}
	matched, err := runMatch(vm, match)
	if err != nil {
		return EvalResult{Error: err.Error()}
	}
	return EvalResult{Matched: matched}
}

func runMatch(vm *goja.Runtime, match string) (bool, error) {
	v, err := vm.RunString(match)
	if err != nil {
		return false, fmt.Errorf("match: %w", err)
	}
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return false, nil
	}
	return truthy(v.Export()), nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x != "" && x != "false" && x != "0"
	case int64:
		return x != 0
	case float64:
		return x != 0
	case nil:
		return false
	default:
		return true
	}
}
