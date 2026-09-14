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
	vm := goja.New()
	type result struct {
		r EvalResult
	}
	ch := make(chan result, 1)
	go func() {
		ch <- result{r: evalMatchOn(vm, match, extract, event)}
	}()
	timer := time.NewTimer(jsStageTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		vm.Interrupt("cancelled")
		return EvalResult{Error: "js cancelled"}
	case <-timer.C:
		vm.Interrupt("timeout")
		return EvalResult{Error: "js stage timeout"}
	case res := <-ch:
		return res.r
	}
}

func evalMatchOn(vm *goja.Runtime, match, extract string, event any) EvalResult {
	if err := vm.Set("event", event); err != nil {
		return EvalResult{Error: fmt.Sprintf("bind event: %v", err)}
	}
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
		if v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
			exported := v.Export()
			if err := vm.Set("event", exported); err != nil {
				return EvalResult{Error: fmt.Sprintf("bind extracted event: %v", err)}
			}
			if m, ok := exported.(map[string]any); ok {
				vars := map[string]string{}
				for k, val := range m {
					vars[k] = fmt.Sprint(val)
				}
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
