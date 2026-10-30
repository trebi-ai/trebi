package visor_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/trebi/internal/visor"
)

func TestPanes(t *testing.T) {
	home, err := os.MkdirTemp("", "tp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) }) // a test folder
	o := agentvisor.Options{Home: home, Client: "test", Env: map[string]string{"SHELL": "/bin/sh", "PS1": "$ "}}
	visor.Configure(&o)
	v, err := agentvisor.New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close(context.Background())
	p := visor.NewPanes(func(context.Context) (visor.Host, error) { return v, nil })
	ctx := t.Context()

	id, err := p.Open(ctx, visor.PaneSpec{Name: "login-x", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := p.Find(ctx, "login-x"); !ok || got != id {
		t.Fatalf("Find = %q, %v; want %q", got, ok, id)
	}
	if _, ok := p.Find(ctx, "other"); ok {
		t.Fatal("Find found a pane with another name")
	}
	if err := p.Send(ctx, id, "echo one-$((1+1))\n"); err != nil {
		t.Fatal(err)
	}
	waitPane(t, p, id, "one-2")
	pane := p.Pane(id)
	if err := pane.Send(ctx, "echo two"); err != nil {
		t.Fatal(err)
	}
	if err := pane.Key(ctx, "enter"); err != nil {
		t.Fatal(err)
	}
	waitPane(t, p, id, "\ntwo")
	if !p.Alive(ctx, id) {
		t.Fatal("the pane is not alive")
	}
	if err := p.Kill(ctx, id); err != nil {
		t.Fatal(err)
	}
	if p.Alive(ctx, id) {
		t.Fatal("the pane is alive after Kill")
	}
	if err := p.Kill(ctx, id); err != nil {
		t.Fatalf("a second Kill: %v", err)
	}
	if out, err := p.History(ctx, id); err != nil || !strings.Contains(out, "one-2") {
		t.Fatalf("History = %q, %v", out, err)
	}
}

func TestKeyBytes(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]string{
		"Enter": "\r", "enter": "\r", "C-c": "\x03", "c-z": "\x1a", "ctrl-d": "\x04", "Up": "\x1b[A",
		"BSpace": "\x7f", "DC": "\x1b[3~", "PageDown": "\x1b[6~", "q": "q",
	} {
		if got := visor.KeyBytes(key); got != want {
			t.Errorf("KeyBytes(%q) = %q, want %q", key, got, want)
		}
	}
}

func waitPane(t *testing.T, p *visor.Panes, id, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		sc, err := p.Screen(t.Context(), id)
		if err == nil && strings.Contains(sc, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the screen has no %q:\n%s (err %v)", want, sc, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
