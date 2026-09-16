package cli

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// TestACPParseUpdate pins session/update mapping without a peer.
func TestACPParseUpdate(t *testing.T) {
	t.Parallel()
	proto := &acpProtocol{kind: acpCursor, waiters: map[int]chan map[string]any{}, approvals: map[string][]acpOption{}}
	proto.sessionID = "ses_1"
	lines := []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"pong"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_1","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Edit file","status":"in_progress"}}}`,
	}
	var evs []Event
	for _, l := range lines {
		evs = append(evs, proto.Parse([]byte(l))...)
	}
	if countType(evs, EventAssistant) != 1 || countType(evs, EventTool) != 1 {
		t.Fatalf("events: %+v", evs)
	}
	// A prompt response is a terminal result.
	promptFrame, err := proto.EncodePrompt(Prompt{Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	_ = json.Unmarshal(promptFrame, &sent)
	id := int(sent["id"].(float64))
	evs = proto.Parse([]byte(`{"jsonrpc":"2.0","id":` + itoa(id) + `,"result":{"stopReason":"end_turn"}}`))
	if countType(evs, EventResult) != 1 {
		t.Fatalf("prompt response: %+v", evs)
	}
}

// TestACPPermissionDecision pins allow/deny option selection.
func TestACPPermissionDecision(t *testing.T) {
	t.Parallel()
	proto := &acpProtocol{kind: acpCopilot, waiters: map[int]chan map[string]any{}, approvals: map[string][]acpOption{}}
	line := `{"jsonrpc":"2.0","id":4,"method":"session/request_permission","params":{"sessionId":"s","toolCall":{"title":"Run tests"},"options":[{"optionId":"a1","kind":"allow_once"},{"optionId":"r1","kind":"reject_once"}]}}`
	evs := proto.Parse([]byte(line))
	if countType(evs, EventPermission) != 1 {
		t.Fatalf("permission: %+v", evs)
	}
	allow, _ := proto.EncodeDecision("4", Decision{Allow: true})
	// Re-register the request so the deny path has options too.
	proto.Parse([]byte(line))
	deny, _ := proto.EncodeDecision("4", Decision{Allow: false})
	if !contains(string(allow), `"optionId":"a1"`) {
		t.Fatalf("allow frame: %s", allow)
	}
	if !contains(string(deny), `"optionId":"r1"`) {
		t.Fatalf("deny frame: %s", deny)
	}
}

// TestACPUnhandledRequestGetsError proves unknown client methods are answered
// so the agent never hangs.
func TestACPUnhandledRequestGetsError(t *testing.T) {
	t.Parallel()
	proto := &acpProtocol{kind: acpCopilot, waiters: map[int]chan map[string]any{}, approvals: map[string][]acpOption{}}
	inR, inW := io.Pipe()
	proto.setWriter(&wireWriter{s: &wireSession{stdin: inW}})
	line := `{"jsonrpc":"2.0","id":3,"method":"fs/read_text_file","params":{"sessionId":"s","path":"/etc/hosts"}}`
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 512)
		n, _ := inR.Read(buf)
		got <- string(buf[:n])
	}()
	if evs := proto.Parse([]byte(line)); len(evs) != 0 {
		t.Fatalf("events: %+v", evs)
	}
	select {
	case frame := <-got:
		if !contains(frame, `"error"`) {
			t.Fatalf("unanswered request: %q", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no response written")
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
