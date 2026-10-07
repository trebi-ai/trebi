package runs

import (
	"os"
	"path/filepath"
	"testing"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// TestWantsCLI pins the runtime selection (Part F/J).
func TestWantsCLI(t *testing.T) {
	svc := &Service{cliDriver: agentwire.FakeDriver{}}
	claude, _ := tmux.ByName("claude")
	if !svc.wantsCLI(claude, LaunchOpts{}) {
		t.Fatal("default runtime must be cli")
	}
	if svc.wantsCLI(claude, LaunchOpts{CreateOpts: CreateOpts{Runtime: "tui"}}) {
		t.Fatal("runtime: tui must force the pane")
	}
	t.Setenv("TREBI_HARNESS_RUNTIME", "tui")
	if svc.wantsCLI(claude, LaunchOpts{}) {
		t.Fatal("kill switch must force the pane")
	}
	t.Setenv("TREBI_HARNESS_RUNTIME", "")
	// Without an override, a driverless harness stays on the pane.
	plain := &Service{}
	grok, _ := tmux.ByName("grok")
	if plain.wantsCLI(grok, LaunchOpts{}) {
		t.Fatal("driverless harness must stay on the pane")
	}
	if !plain.wantsCLI(claude, LaunchOpts{}) {
		t.Fatal("claude has a built-in driver")
	}
}

// TestWantsCLIFileRuntime pins Part J: tools.<name>.runtime tui and the
// global harness.runtime kill switch both force the pane; other tools keep
// the cli default.
func TestWantsCLIFileRuntime(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	writeCfg := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	svc := &Service{cliDriver: agentwire.FakeDriver{}}
	claude, _ := tmux.ByName("claude")
	codex, _ := tmux.ByName("codex")

	writeCfg("harness:\n  runtime: tui\n")
	if svc.wantsCLI(claude, LaunchOpts{}) {
		t.Fatal("global kill switch must force the pane")
	}
	writeCfg("tools:\n  claude:\n    runtime: tui\n")
	if svc.wantsCLI(claude, LaunchOpts{}) {
		t.Fatal("tools.claude.runtime must force the pane")
	}
	if !svc.wantsCLI(codex, LaunchOpts{}) {
		t.Fatal("another tool must stay on the cli default")
	}
	writeCfg("tools:\n  claude:\n    runtime: cli\n")
	if !svc.wantsCLI(claude, LaunchOpts{}) {
		t.Fatal("tools.claude.runtime cli must restore the wire")
	}
}

