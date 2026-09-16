//go:build live_cli

package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/tmux"
)

// TestLiveOpenCodeAuth: `opencode auth list` under the shared env.
func TestLiveOpenCodeAuth(t *testing.T) {
	if !liveEnabled(t, "opencode") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "opencode")
}

// TestLiveOpenCodeServe: localhost server with password, health, session
// create, prompt_async, SSE until turn complete, abort.
func TestLiveOpenCodeServe(t *testing.T) {
	if !liveEnabled(t, "opencode") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "opencode")

	cmd, env := livePlan(t, "opencode")
	_ = cmd
	req := StartRequest{
		Tool:           "opencode",
		BaseCommand:    liveBin("opencode"),
		WorkingDir:     t.TempDir(),
		Env:            env,
		Persistent:     true,
		PermissionMode: "",
	}
	s, err := openCodeStart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	if s.ID() == "" {
		t.Fatal("no session id")
	}
	if s.PID() <= 0 {
		t.Fatal("no server pid")
	}
	if err := s.Prompt(context.Background(), Prompt{Text: "Reply with exactly PONG and nothing else.", Kind: "job"}); err != nil {
		t.Fatal(err)
	}
	text, res := assistantText(t, s, 4*time.Minute)
	if res.Result == nil || res.Result.IsError {
		t.Fatalf("result: %+v", res.Result)
	}
	if !pongMatch(text) {
		t.Fatalf("assistant text = %q", text)
	}
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("abort: %v", err)
	}
	ShutdownOpenCodeServers()
}

// TestLiveOpenCodeDirectoryRouting: one server, two cwds via the
// x-opencode-directory header; each session reports its own directory.
func TestLiveOpenCodeDirectoryRouting(t *testing.T) {
	if !liveEnabled(t, "opencode") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "opencode")

	env := map[string]string{}
	if tool, ok := tmux.ByName("opencode"); ok {
		plan := tool.PrepareLaunch(liveBin("opencode"))
		for k, v := range plan.Env {
			env[k] = v
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srv, err := openCodeServers.acquireServer(ctx, liveBin("opencode"), env, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer openCodeServers.shutdown()

	dirA, dirB := t.TempDir(), t.TempDir()
	resolved := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	create := func(dir string) string {
		var res struct {
			ID string `json:"id"`
		}
		if err := srv.call(ctx, "POST", "/session", dir, map[string]any{}, &res); err != nil {
			t.Fatalf("create in %s: %v", dir, err)
		}
		return res.ID
	}
	idA, idB := create(dirA), create(dirB)
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("session ids: %q %q", idA, idB)
	}
	lookup := func(id string) string {
		var info struct {
			Directory string `json:"directory"`
		}
		if err := srv.call(ctx, "GET", "/session/"+id, "", nil, &info); err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		return info.Directory
	}
	if got := lookup(idA); resolved(got) != resolved(dirA) {
		t.Fatalf("session A directory = %q, want %q", got, dirA)
	}
	if got := lookup(idB); resolved(got) != resolved(dirB) {
		t.Fatalf("session B directory = %q, want %q", got, dirB)
	}
}
