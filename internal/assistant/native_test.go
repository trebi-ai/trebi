package assistant

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/agent-wire/native"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// sseBody wraps one OpenAI chunk payload into the streamable wire format.
func sseBody(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// chunk is one chat.completion.chunk shell with the given delta and finish.
func chunk(delta, finish string) string {
	out := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":` + delta
	if finish == "" {
		return out + `,"finish_reason":null}]}`
	}
	return out + `,"finish_reason":"` + finish + `"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`
}

// textTurn is one full streamed answer: a content delta, then stop.
func textTurn(text string) string {
	return sseBody(chunk(`{"role":"assistant","content":`+sseQuote(text)+`}`, ""), chunk(`{}`, "stop"))
}

func sseQuote(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\n':
			b = append(b, '\\', 'n')
		default:
			b = append(b, byte(r))
		}
	}
	return string(append(b, '"'))
}

// toolStartTurn streams one write tool call: the call header, the argument
// fragments, then the tool_calls finish.
func toolStartTurn() string {
	return sseBody(
		chunk(`{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write","arguments":""}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"approval.txt\","}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"\"content\":\"hi\"}"}}]}`, ""),
		chunk(`{}`, "tool_calls"),
	)
}

// scriptServer answers the first stream with bodies[0], the second with
// bodies[1], and every later one with the last body.
func scriptServer(t *testing.T, bodies ...string) config.NativeConfig {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		body := bodies[len(bodies)-1]
		if calls < len(bodies) {
			body = bodies[calls]
		}
		calls++
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return config.NativeConfig{
		Model: "test-model", APIKeyEnv: "TREBI_TEST_KEY",
		BaseURL: srv.URL, APIFormat: "openai",
	}
}

// nativeTestRT registers the native driver over a scripted OpenAI endpoint
// and isolates home and config.
func nativeTestRT(t *testing.T, cfg config.NativeConfig) (*agentwire.Runtime, *Store, *store.Store) {
	t.Helper()
	base := t.TempDir()
	home.SetBase(base)
	t.Cleanup(func() { home.SetBase("") })
	t.Setenv("TREBI_TEST_KEY", "sk-test")

	st, err := store.OpenPath(filepath.Join(base, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ast := NewStore(st)
	rt := agentwire.New(agentwire.Options{ClientName: "trebi-test"})
	RegisterNativeDriver(rt, func() config.NativeConfig { return cfg }, nil, ast, 0, nil)
	return rt, ast, st
}

// startNative opens one native session through the runtime.
func startNative(t *testing.T, rt *agentwire.Runtime, id, dir string, perms agentwire.PermissionPolicy) agentwire.Session {
	t.Helper()
	sess, err := rt.Start(context.Background(), agentwire.StartRequest{
		Harness:     agentwire.Native,
		SessionID:   id,
		WorkingDir:  dir,
		Model:       "test-model",
		Permissions: perms,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// drainIdle reads events until the session goes idle and reports whether a
// permission request arrived plus the last completed tool output.
func drainIdle(t *testing.T, events <-chan agentwire.Event) (asked bool, toolOut string) {
	t.Helper()
	for evt := range events {
		switch {
		case evt.Type == agentwire.EventPermission && evt.Permission != nil:
			asked = true
		case evt.Type == agentwire.EventTool && evt.Tool != nil && evt.Tool.Output != "":
			toolOut = evt.Tool.Output
		case evt.Type == agentwire.EventStatus && evt.Status == agentwire.StatusIdle:
			return asked, toolOut
		}
	}
	t.Fatal("the event stream closed before the turn went idle")
	return false, ""
}

// TestNativeSessionEmitsWireEvents drives one native turn through the
// runtime over a fake OpenAI endpoint.
func TestNativeSessionEmitsWireEvents(t *testing.T) {
	cfg := scriptServer(t, textTurn("hello from native"))
	rt, _, _ := nativeTestRT(t, cfg)
	sess := startNative(t, rt, "conv-n1", t.TempDir(), agentwire.PermissionPolicy{})
	if sess.ID() != "conv-n1" {
		t.Fatalf("vendor id %q", sess.ID())
	}
	ctx := context.Background()
	if err := sess.Prompt(ctx, agentwire.Prompt{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	var sawText, sawResult bool
	for evt := range sess.Events() {
		if evt.Type == agentwire.EventAssistant && evt.Text == "hello from native" {
			sawText = true
		}
		if evt.Type == agentwire.EventResult {
			sawResult = evt.Result != nil && !evt.Result.IsError
		}
		if evt.Type == agentwire.EventStatus && evt.Status == agentwire.StatusIdle {
			break
		}
	}
	if !sawText || !sawResult {
		t.Fatalf("text=%v result=%v", sawText, sawResult)
	}
	if err := sess.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestNativeResumeFromLog checks that a second session reloads the
// persisted message log and continues it.
func TestNativeResumeFromLog(t *testing.T) {
	cfg := scriptServer(t, textTurn("first answer"), textTurn("second answer"))
	rt, _, _ := nativeTestRT(t, cfg)
	ctx := context.Background()
	sess := startNative(t, rt, "conv-n2", t.TempDir(), agentwire.PermissionPolicy{})
	if err := sess.Prompt(ctx, agentwire.Prompt{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	drainIdle(t, sess.Events())
	if err := sess.Close(ctx); err != nil {
		t.Fatal(err)
	}

	msgs, err := native.FileStore(filepath.Join(home.Base(), "assistant", "native")).
		Load(ctx, "conv-n2")
	if err != nil || len(msgs) == 0 {
		t.Fatalf("persisted messages %v %v", msgs, err)
	}
	if msgs[0].Role != "user" || msgs[0].Parts[0].Text == nil || msgs[0].Parts[0].Text.Text != "first" {
		t.Fatalf("first message %+v", msgs[0])
	}

	resumed := startNative(t, rt, "conv-n2", t.TempDir(), agentwire.PermissionPolicy{})
	if err := resumed.Prompt(ctx, agentwire.Prompt{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	drainIdle(t, resumed.Events())
	if err := resumed.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestNativeDailyCapBlocks pins the BeforeCall guard over assistant_sessions.
func TestNativeDailyCapBlocks(t *testing.T) {
	_, ast, st := nativeTestRT(t, config.NativeConfig{Model: "m", APIKeyEnv: "TREBI_TEST_KEY"})
	ctx := context.Background()
	if _, err := st.DB().Exec(`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at)
		VALUES ('cn', 'ws', 'k', '', '2026-09-26T00:00:00Z', datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	if err := ast.Create(ctx, Record{
		ConversationID: "cn", WorkspaceID: "ws", Harness: "native", Mode: "auto_edit",
		Status: StatusIdle, TokensIn: 100, TokensOut: 23,
	}); err != nil {
		t.Fatal(err)
	}
	if err := NativeDailyCapGuard(ctx, ast, 0); err != nil {
		t.Fatalf("a zero cap never blocks: %v", err)
	}
	if err := NativeDailyCapGuard(ctx, ast, 200); err != nil {
		t.Fatalf("123 tokens under a cap of 200 must pass: %v", err)
	}
	if err := NativeDailyCapGuard(ctx, ast, 100); err == nil {
		t.Fatal("123 tokens over a cap of 100 must block")
	}
}

// TestNativeAskModeAsksBeforeWrite pins the ask posture: the native loop
// asks before a mutating tool call, a denial reaches the model as the tool
// result, and the file is never written.
func TestNativeAskModeAsksBeforeWrite(t *testing.T) {
	cfg := scriptServer(t, toolStartTurn(), textTurn("done"))
	rt, _, _ := nativeTestRT(t, cfg)
	ws := t.TempDir()
	ctx := context.Background()
	sess := startNative(t, rt, "conv-approve", ws, agentwire.PermissionPolicy{Mode: agentwire.PermissionAsk})

	permID := make(chan string, 1)
	go func() {
		for evt := range sess.Events() {
			if evt.Type == agentwire.EventPermission && evt.Permission != nil {
				permID <- evt.Permission.ID
				return
			}
		}
		close(permID)
	}()
	promptErr := make(chan error, 1)
	go func() { promptErr <- sess.Prompt(ctx, agentwire.Prompt{Text: "write the file"}) }()

	select {
	case id := <-permID:
		if err := sess.AnswerPermission(ctx, id, agentwire.Decision{Allow: false, Message: "not now"}); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the permission request")
	}
	if err := <-promptErr; err != nil {
		t.Fatal(err)
	}
	_, toolOut := drainIdle(t, sess.Events())
	if !strings.Contains(toolOut, "not now") {
		t.Fatalf("tool output = %q, want the denial note", toolOut)
	}
	if _, statErr := os.Stat(filepath.Join(ws, "approval.txt")); !os.IsNotExist(statErr) {
		t.Fatal("the denied write must not touch the workspace")
	}
	if err := sess.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestNativeAutoModeRunsWriteWithoutAsking pins the auto short-circuit: the
// same write runs with no permission event.
func TestNativeAutoModeRunsWriteWithoutAsking(t *testing.T) {
	cfg := scriptServer(t, toolStartTurn(), textTurn("done"))
	rt, _, _ := nativeTestRT(t, cfg)
	ws := t.TempDir()
	ctx := context.Background()
	sess := startNative(t, rt, "conv-approve-auto", ws, agentwire.PermissionPolicy{Mode: agentwire.PermissionAuto})

	asked := false
	go func() {
		for evt := range sess.Events() {
			if evt.Type == agentwire.EventPermission {
				asked = true
			}
		}
	}()
	if err := sess.Prompt(ctx, agentwire.Prompt{Text: "write the file"}); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(ws, "approval.txt")); statErr != nil {
		t.Fatalf("the allowed write is missing: %v", statErr)
	}
	if err := sess.Close(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if asked {
		t.Fatal("auto mode must not ask")
	}
}
