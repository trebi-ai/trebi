package daemon_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/conversations"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/llm"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestNativeAssistantSayE2E(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("NATIVE_E2E_KEY", "sk-e2e")
	t.Cleanup(func() { home.SetBase("") })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	defer srv.Close()

	yaml := "assistant:\n  model: gpt-test\n  api_format: openai\n  api_key_env: NATIVE_E2E_KEY\n  base_url: " + srv.URL + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	wsPath := filepath.Join(dir, "ws")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, "TREBI.md"), []byte("---\n---\n\nYou are the assistant.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := daemon.New(st, daemon.Options{
		QueueInterval:     50 * time.Millisecond,
		TurnWatchInterval: time.Hour,
		AlertsInterval:    time.Hour,
	})
	asst := config.AssistantConfig{Model: "gpt-test", APIFormat: "openai", BaseURL: srv.URL, APIKeyEnv: "NATIVE_E2E_KEY"}
	prov, err := llm.NewWithHTTP(asst, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	cp.Runs.SetNativeHooks(runs.NativeHooks{Provider: prov})

	ctx := context.Background()
	ws, err := cp.Workspaces.Add(ctx, wsPath, "native-ws")
	if err != nil {
		t.Fatal(err)
	}
	conv, err := cp.Conversations.Create(ctx, conversations.CreateOpts{
		Workspace: ws.ID, OriginChannel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp.Start(runCtx)
	defer cp.Stop()

	if _, err := cp.Conversations.Send(ctx, conv.ID, conversations.SendOpts{
		Text: "ping", ChannelType: "cli", ChannelRef: ws.ID,
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := cp.Conversations.ListMessages(ctx, conv.ID, conversations.MessageListOpts{Limit: 20})
		if err == nil {
			for _, m := range msgs {
				if m.Role == conversations.RoleAssistant && m.Text == "pong" {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	msgs, _ := cp.Conversations.ListMessages(ctx, conv.ID, conversations.MessageListOpts{Limit: 20})
	t.Fatalf("no native reply: %+v", msgs)
}
