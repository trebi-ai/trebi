package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
)

func TestNormalizeBaseURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, format, want string
	}{
		{"https://api.openai.com", "openai", "https://api.openai.com/v1"},
		{"https://api.openai.com/", "openai", "https://api.openai.com/v1"},
		{"https://openrouter.ai/api/v1", "openai", "https://openrouter.ai/api/v1"},
		{"https://litellm.example/my/prefix", "openai", "https://litellm.example/my/prefix/v1"},
		{"https://api.anthropic.com", "anthropic", "https://api.anthropic.com"},
	}
	for _, tc := range cases {
		got := NormalizeBaseURL(tc.in, tc.format)
		if got != tc.want {
			t.Errorf("NormalizeBaseURL(%q,%q)=%q want %q", tc.in, tc.format, got, tc.want)
		}
	}
}

func TestMapStop(t *testing.T) {
	t.Parallel()
	if mapStop("stop") != StopEndTurn || mapStop("tool_calls") != StopToolUse {
		t.Fatal("basic")
	}
	if mapStop("refusal") != StopRefusal || mapStop("content_filter") != StopContentFilter {
		t.Fatal("terminal")
	}
	if mapStop("nope") != StopUnknown {
		t.Fatal("unknown")
	}
}

func TestOpenAITextAndToolAnd401(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(r.URL.RawQuery, "fail=1") || r.Header.Get("X-Fail") == "401":
			writeJSON(w, 401, `{"error":{"message":"bad key","type":"invalid_request_error"}}`)
		case strings.Contains(string(body), `"name":"bash"`):
			writeJSON(w, 200, openaiToolBody)
		default:
			writeJSON(w, 200, openaiTextBody)
		}
	}))
	defer srv.Close()

	t.Setenv("LLM_TEST_KEY", "sk-test")
	cfg := config.AssistantConfig{Model: "gpt-4", APIFormat: "openai", BaseURL: srv.URL, APIKeyEnv: "LLM_TEST_KEY"}
	p, err := NewWithHTTP(cfg, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	resp, err := p.Chat(ctx, Request{Model: "gpt-4", Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 16})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hello" || resp.Stop != StopEndTurn || resp.Usage.Input != 10 {
		t.Fatalf("%+v", resp)
	}
	if !strings.HasSuffix(gotPath, "/chat/completions") {
		t.Fatalf("path %s", gotPath)
	}

	resp, err = p.Chat(ctx, Request{
		Model: "gpt-4", Messages: []Message{{Role: "user", Content: "run"}},
		Tools: []Tool{{Name: "bash", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Stop != StopToolUse || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "bash" {
		t.Fatalf("%+v", resp)
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 401, `{"error":{"message":"nope","type":"invalid_request_error"}}`)
	}))
	defer fail.Close()
	cfg.BaseURL = fail.URL
	p, err = NewWithHTTP(cfg, fail.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(ctx, Request{Model: "gpt-4", Messages: []Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401: %v", err)
	}
}

func TestAnthropicTextAndToolAnd401(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"name":"bash"`) {
			writeJSON(w, 200, anthropicToolBody)
			return
		}
		writeJSON(w, 200, anthropicTextBody)
	}))
	defer srv.Close()

	t.Setenv("LLM_TEST_KEY", "sk-ant")
	cfg := config.AssistantConfig{Model: "claude-sonnet-4-5", APIFormat: "anthropic", BaseURL: srv.URL, APIKeyEnv: "LLM_TEST_KEY"}
	p, err := NewWithHTTP(cfg, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	resp, err := p.Chat(ctx, Request{Model: "claude-sonnet-4-5", Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 32})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hello" || resp.Stop != StopEndTurn {
		t.Fatalf("%+v", resp)
	}
	if !strings.HasSuffix(gotPath, "/v1/messages") {
		t.Fatalf("path %s", gotPath)
	}

	resp, err = p.Chat(ctx, Request{
		Model: "claude-sonnet-4-5", Messages: []Message{{Role: "user", Content: "run"}},
		Tools: []Tool{{Name: "bash", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Stop != StopToolUse || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "bash" {
		t.Fatalf("%+v", resp)
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 401, `{"type":"error","error":{"type":"authentication_error","message":"nope"}}`)
	}))
	defer fail.Close()
	cfg.BaseURL = fail.URL
	p, err = NewWithHTTP(cfg, fail.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(ctx, Request{Model: "claude-sonnet-4-5", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 8})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401: %v", err)
	}
}

func TestStopUnknown(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"mystery","message":{"role":"assistant","content":""}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()
	t.Setenv("LLM_TEST_KEY", "k")
	cfg := config.AssistantConfig{Model: "gpt-4", BaseURL: srv.URL, APIKeyEnv: "LLM_TEST_KEY"}
	p, err := NewWithHTTP(cfg, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), Request{Model: "gpt-4", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Stop != StopUnknown || resp.RawStop != "mystery" {
		t.Fatalf("%+v", resp)
	}
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

const openaiTextBody = `{"id":"c1","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`

const openaiToolBody = `{"id":"c2","object":"chat.completion","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"pwd\"}"}}]}}],"usage":{"prompt_tokens":11,"completion_tokens":8,"total_tokens":19}}`

const anthropicTextBody = `{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"model":"claude-sonnet-4-5","stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":2}}`

const anthropicToolBody = `{"id":"m2","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"pwd"}}],"model":"claude-sonnet-4-5","stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":6}}`
