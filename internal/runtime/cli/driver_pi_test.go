package cli

import (
	"encoding/json"
	"testing"
)

// piFixtureLines mirror a real `pi --mode rpc` capture (pi 0.84.1,
// 2026-09-16): response ack, agent/turn/message lifecycle, agent_end.
var piFixtureLines = []string{
	`{"type":"response","command":"prompt","success":true}`,
	`{"type":"agent_start"}`,
	`{"type":"turn_start"}`,
	`{"type":"message_start","message":{"role":"user","content":[{"type":"text","text":"Reply with exactly PONG."}],"timestamp":1}}`,
	`{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"Reply with exactly PONG."}],"timestamp":1}}`,
	`{"type":"message_start","message":{"role":"assistant","content":[],"provider":"xai","model":"grok-4.5","usage":{"input":1,"output":2,"cacheRead":3,"cacheWrite":0,"totalTokens":6,"cost":{"total":0.001}},"stopReason":null,"timestamp":2}}`,
	`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"PONG"}],"provider":"xai","model":"grok-4.5","usage":{"input":1,"output":2,"cacheRead":3,"cacheWrite":0,"totalTokens":6,"cost":{"total":0.001}},"stopReason":"stop","timestamp":2}}`,
	`{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"PONG"}]},"toolResults":[]}`,
	`{"type":"agent_end","messages":[],"willRetry":false}`,
	`{"type":"agent_settled"}`,
}

func TestPiProtocolMapping(t *testing.T) {
	t.Parallel()
	proto := &piProtocol{waiters: map[string]chan map[string]any{}}
	var evs []Event
	for _, l := range piFixtureLines {
		evs = append(evs, proto.Parse([]byte(l))...)
	}
	if countType(evs, EventAssistant) != 1 || countType(evs, EventResult) != 1 {
		t.Fatalf("events: %+v", evs)
	}
	for _, ev := range evs {
		if ev.Type == EventAssistant && ev.Text != "PONG" {
			t.Fatalf("assistant text: %q", ev.Text)
		}
		if ev.Type == EventResult && (ev.Result == nil || ev.Result.IsError) {
			t.Fatalf("result: %+v", ev.Result)
		}
	}
	if countType(evs, EventUsage) != 1 {
		t.Fatalf("usage: %+v", evs)
	}
	frame, err := proto.EncodePrompt(Prompt{Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(frame, &m)
	if m["type"] != "prompt" || m["message"] != "hi" {
		t.Fatalf("prompt frame: %+v", m)
	}
	dec, _ := proto.EncodeDecision("perm_1", Decision{Allow: false})
	_ = json.Unmarshal(dec, &m)
	if m["approved"] != false {
		t.Fatalf("decision: %+v", m)
	}
}

// TestPiProtocolProviderError pins the real failure shape: a provider OAuth
// failure arrives on message_end and must surface as a failed result, not a
// silent success.
func TestPiProtocolProviderError(t *testing.T) {
	t.Parallel()
	proto := &piProtocol{waiters: map[string]chan map[string]any{}}
	lines := []string{
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"OAuth refresh failed for xai: invalid_grant"}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"OAuth refresh failed for xai: invalid_grant"}}`,
		`{"type":"turn_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"OAuth refresh failed for xai: invalid_grant"},"toolResults":[]}`,
		`{"type":"agent_end","messages":[],"willRetry":false}`,
	}
	var evs []Event
	for _, l := range lines {
		evs = append(evs, proto.Parse([]byte(l))...)
	}
	var result *Result
	for i := range evs {
		if evs[i].Type == EventResult {
			result = evs[i].Result
		}
	}
	if result == nil || !result.IsError {
		t.Fatalf("result: %+v", result)
	}
	if result.EndReason != string(FailAuth) {
		t.Fatalf("class: %q (%s)", result.EndReason, result.Text)
	}
}

// TestPiProtocolPermissions pins the permission request/answer shapes.
func TestPiProtocolPermissions(t *testing.T) {
	t.Parallel()
	proto := &piProtocol{waiters: map[string]chan map[string]any{}}
	evs := proto.Parse([]byte(`{"type":"permission_request","id":"perm_1","tool":"bash","message":"Allow bash?"}`))
	if countType(evs, EventPermission) != 1 {
		t.Fatalf("permission: %+v", evs)
	}
}
