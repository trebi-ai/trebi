package visor_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/trebi/internal/visor"
)

func TestObservedSessions(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	home, err := os.MkdirTemp("", "tv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) }) // a test folder
	o := agentvisor.Options{Home: home, Client: "test"}
	visor.Configure(&o)
	v, err := agentvisor.New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close(context.Background())
	obs := visor.NewObserved(func(context.Context) (visor.Host, error) { return v, nil })

	cwd := t.TempDir()
	for _, step := range []struct {
		event string
		extra map[string]any
	}{
		{"SessionStart", map[string]any{"source": "startup"}},
		{"UserPromptSubmit", map[string]any{"prompt": "Add a route."}},
		{"Stop", map[string]any{"last_assistant_message": "The route is in place."}},
		{"PermissionRequest", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "make test"}}},
	} {
		p := map[string]any{"session_id": "v-1", "cwd": cwd, "hook_event_name": step.event, "transcript_path": "/tmp/t.jsonl"}
		for k, x := range step.extra {
			p[k] = x
		}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Hook(t.Context(), agentvisor.HookCall{Harness: "claude", Payload: data}); err != nil {
			t.Fatalf("hook %s: %v", step.event, err)
		}
	}

	list, err := obs.List(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	s := list[0]
	if s.Harness != "claude" || s.Folder != cwd || s.State != "waiting" || s.Waiting != 1 || s.Turns != 1 {
		t.Fatalf("session: %+v", s)
	}
	got, turns, err := obs.Get(t.Context(), s.ID)
	if err != nil || got.ID != s.ID {
		t.Fatalf("get: %+v %v", got, err)
	}
	want := []visor.ObservedTurn{{Role: "user", Text: "Add a route."}, {Role: "assistant", Text: "The route is in place."}, {Role: "request", Text: "Wants to use Bash."}}
	if len(turns) != len(want) {
		t.Fatalf("turns: %+v", turns)
	}
	for i, w := range want {
		if turns[i].Role != w.Role || turns[i].Text != w.Text {
			t.Fatalf("turn %d: %+v, want %+v", i, turns[i], w)
		}
	}
	if _, _, err := obs.Get(t.Context(), "ses_nope"); !errors.Is(err, agentvisor.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}

	for _, c := range []struct {
		on   bool
		want string
	}{{true, visor.HooksOn}, {false, visor.HooksOff}} {
		state, err := obs.SetHooks(t.Context(), "claude", c.on)
		if err != nil || state != c.want {
			t.Fatalf("set hooks %v: %s %v", c.on, state, err)
		}
		if state, err := obs.Hooks(t.Context(), "claude"); err != nil || state != c.want {
			t.Fatalf("hooks after %v: %s %v", c.on, state, err)
		}
	}
	if _, err := os.Stat(filepath.Join(user, ".claude", "settings.json")); err != nil {
		t.Fatalf("the user login has no settings: %v", err)
	}
	if state, err := obs.Hooks(t.Context(), "gemini"); err != nil || state != visor.HooksUnsupported {
		t.Fatalf("no hooks: %s %v", state, err)
	}
}
