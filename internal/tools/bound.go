package tools

import "encoding/json"

// defaultMaxBytes caps a bounded body when the caller names no max_bytes. A
// whole log file in the model context is never the useful answer.
const defaultMaxBytes = 64 << 10

// BoundBody caps one response at max_bytes. A cut body is no longer valid
// JSON, so it comes back wrapped and marked.
func BoundBody(in Input, body []byte, tail bool) ([]byte, error) {
	cut, truncated, err := in.clampBody(body, tail)
	if err != nil {
		return nil, err
	}
	if !truncated {
		return body, nil
	}
	return json.Marshal(map[string]any{"content": string(cut), "truncated": true})
}

// clampBody cuts body to max_bytes and reports whether it cut anything.
func (in Input) clampBody(body []byte, tail bool) ([]byte, bool, error) {
	max, ok, err := in.num("max_bytes")
	if err != nil {
		return nil, false, err
	}
	if !ok || max <= 0 {
		max = defaultMaxBytes
	}
	if len(body) <= max {
		return body, false, nil
	}
	if tail {
		return body[len(body)-max:], true, nil
	}
	return body[:max], true, nil
}
