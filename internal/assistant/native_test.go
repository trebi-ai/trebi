package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/llm"
	"github.com/trebi-ai/trebi/internal/runtime/native"
	"github.com/trebi-ai/trebi/internal/store"
)

// scriptedProvider answers Chat with a fixed queue of responses.
type scriptedProvider struct {
	mu    sync.Mutex
	resps []*llm.Response
	calls []llm.Request
}

func (p *scriptedProvider) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req)
	if len(p.resps) == 0 {
		return nil, errors.New("script exhausted")
	}
	r := p.resps[0]
	p.resps = p.resps[1:]
	return r, nil
}

// openAIBody is the smallest completion the adapters accept.
func openAIBody(text string) string {
	return `{"choices":[{"message":{"role":"assistant","content":` + quote(text) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func nativeTestRuntime(t *testing.T, resp *llm.Response) (*NativeDriver, *scriptedProvider, *Store, *store.Store) {
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

	// The provider is a fake HTTP server speaking OpenAI completions.
	if resp != nil {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, openAIBody(resp.Text))
		}))
		t.Cleanup(srv.Close)
		cfg := config.NativeConfig{
			Model: "test-model", APIKeyEnv: "TREBI_TEST_KEY",
			BaseURL: srv.URL, APIFormat: "openai",
		}
		driver := NewNativeDriver(cfg, NewStore(st), nil, 0, nil)
		return driver, nil, NewStore(st), st
	}
	cfg := config.NativeConfig{Model: "test-model", APIKeyEnv: "TREBI_TEST_KEY"}
	return NewNativeDriver(cfg, NewStore(st), nil, 0, nil), nil, NewStore(st), st
}

// TestNativeSessionEmitsWireEvents drives the native loop through the
// adapter over a fake OpenAI endpoint.
func TestNativeSessionEmitsWireEvents(t *testing.T) {
	driver, _, _, _ := nativeTestRuntime(t, &llm.Response{Text: "hello from native", Stop: llm.StopEndTurn})
	sess, err := driver.Start(context.Background(), agentwire.StartRequest{
		SessionID:    "conv-n1",
		WorkingDir:   t.TempDir(),
		Instructions: "be brief",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID() != "conv-n1" {
		t.Fatalf("vendor id %q", sess.ID())
	}
	if err := sess.Prompt(context.Background(), agentwire.Prompt{Text: "hi"}); err != nil {
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
	if err := sess.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestNativeResumeFromTranscript checks that a second process reloads the
// persisted message log.
func TestNativeResumeFromTranscript(t *testing.T) {
	driver, _, _, _ := nativeTestRuntime(t, &llm.Response{Text: "second answer", Stop: llm.StopEndTurn})
	ctx := context.Background()
	sess, err := driver.Start(ctx, agentwire.StartRequest{SessionID: "conv-n2", WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Prompt(ctx, agentwire.Prompt{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	for evt := range sess.Events() {
		if evt.Type == agentwire.EventStatus && evt.Status == agentwire.StatusIdle {
			break
		}
	}
	_ = sess.Close(ctx)

	msgs, err := native.LoadMessages("conv-n2")
	if err != nil || len(msgs) == 0 {
		t.Fatalf("persisted messages %v %v", msgs, err)
	}
	if msgs[0].Role != "user" || msgs[0].Content != "first" {
		t.Fatalf("first message %+v", msgs[0])
	}

	resumed, err := driver.Resume(ctx, agentwire.StartRequest{SessionID: "conv-n2", WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Prompt(ctx, agentwire.Prompt{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	drained := false
	for evt := range resumed.Events() {
		if evt.Type == agentwire.EventStatus && evt.Status == agentwire.StatusIdle {
			drained = true
			break
		}
	}
	if !drained {
		t.Fatal("resumed turn never went idle")
	}
}

// TestNativeDailyCapReadsSessions covers TokensToday over assistant_sessions.
func TestNativeDailyCapReadsSessions(t *testing.T) {
	driver, _, ast, st := nativeTestRuntime(t, nil)
	if _, err := st.DB().Exec(`INSERT INTO conversations (id, workspace_id, key, title, created_at, updated_at)
		VALUES ('cn', 'ws', 'k', '', '2026-09-26T00:00:00Z', datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	if err := ast.Create(context.Background(), Record{
		ConversationID: "cn", WorkspaceID: "ws", Harness: "native", Mode: "auto_edit",
		Status: StatusIdle, TokensIn: 100, TokensOut: 23,
	}); err != nil {
		t.Fatal(err)
	}
	today, err := ast.TokensToday(context.Background(), "native")
	if err != nil || today != 123 {
		t.Fatalf("tokens today %d %v", today, err)
	}
	d, err := driver.Detect(context.Background(), "native")
	if err != nil || !d.Supported {
		t.Fatalf("detect %+v %v", d, err)
	}
	if _, err := driver.Detect(context.Background(), "claude"); err == nil {
		t.Fatal("native driver must refuse other harnesses")
	}
}
