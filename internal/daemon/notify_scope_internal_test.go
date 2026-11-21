package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/notify"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// TestChannelProblemScope proves a notify step on a channel outside its
// scope gets the scope problem, so the step is skipped with a warning.
func TestChannelProblemScope(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := connectors.New(st, workspaces.New(st), nil)
	m := manifest.Manifest{Schema: manifest.SchemaID, Name: "chat", Title: "chat", Description: "Test.", Version: "1.0.0",
		Events: &manifest.Events{Protocol: manifest.ProtocolConnector, Command: "true", Types: []manifest.EventType{{Type: "message"}}}, Channel: &manifest.Channel{}}
	if _, err := svc.Create(context.Background(), connectors.Connection{Type: "chat", Slug: "chat", From: connectors.FromInline,
		Manifest: &m, Scope: connectors.Scope{"ws_a"}}); err != nil {
		t.Fatal(err)
	}
	ch := connectorChannels{conn: svc}
	if got := ch.ChannelProblem("chat", "ws_b"); got != notify.ProblemScope {
		t.Fatalf("out of scope problem %q", got)
	}
	if got := ch.ChannelProblem("chat", "ws_a"); got == notify.ProblemScope {
		t.Fatalf("in scope problem %q", got)
	}
}

// TestNotifyGlobalReloadsOnChange proves Global loads config.yaml only when
// the file changes.
func TestNotifyGlobalReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Email.Admins = []string{"a@example.com"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	l := &notifyLayers{}
	if _, _, admins := l.Global(); len(admins) != 1 || admins[0] != "a@example.com" {
		t.Fatalf("first load: %v", admins)
	}
	// A sentinel survives a call with no file change: Global did not load.
	l.setGlobal(&config.Config{Email: config.EmailConfig{Admins: []string{"sentinel"}}})
	if _, _, admins := l.Global(); len(admins) != 1 || admins[0] != "sentinel" {
		t.Fatalf("unchanged file reloaded: %v", admins)
	}
	cfg.Email.Admins = []string{"b@example.com", "c@example.com"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if _, _, admins := l.Global(); len(admins) != 2 || admins[0] != "b@example.com" {
		t.Fatalf("changed file not reloaded: %v", admins)
	}
}
