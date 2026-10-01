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
	// Extracted is the extract output; nil when there is no extract or it
	// returns null.
	Extracted any
	Error     string
	// ExtractErr marks an Error of the extract stage.
	ExtractErr bool
}

// EvalMatch runs extract (optional) then match against event in a Goja VM.
// event is bound as `event` (parsed object or {line: "..."}). An empty
// match matches.
func EvalMatch(ctx context.Context, match, extract string, event any) EvalResult {
	if match == "" && extract == "" {
		return EvalResult{Matched: true}
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
	if err := vm.Set("event", jsValue(event)); err != nil {
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

	var res EvalResult
	if extract != "" {
		v, err := vm.RunString(extract)
		if err != nil {
			return EvalResult{Error: fmt.Sprintf("extract: %v", err), ExtractErr: true}
		}
		if v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
			res.Extracted = v.Export()
			if err := vm.Set("event", res.Extracted); err != nil {
				return EvalResult{Extracted: res.Extracted, Error: fmt.Sprintf("bind extracted event: %v", err), ExtractErr: true}
			}
			if m, ok := res.Extracted.(map[string]any); ok {
				res.Vars = map[string]string{}
				for k, val := range m {
					res.Vars[k] = fmt.Sprint(val)
				}
			}
		}
	}
	if match == "" {
		res.Matched = true
		return res
	}
	matched, err := runMatch(vm, match)
	if err != nil {
		res.Vars = nil
		res.Error = err.Error()
		return res
	}
	res.Matched = matched
	return res
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

// jsValue copies a decoded JSON value with each json.Number as a JS number:
// int64 when the value is an integer, else float64. goja sees a json.Number
// as a string. The copy leaves the caller's value as it is.
func jsValue(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = jsValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = jsValue(val)
		}
		return out
	}
	return v
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
