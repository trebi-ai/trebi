package runs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
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

// TestCLIRunModel pins the model order of a result: the usage model, then
// the session model from init, then the launch model. The init model goes on
// the run row and the turn row.
func TestCLIRunModel(t *testing.T) {
	cases := []struct {
		name, usage, init, launch, want string
	}{
		{name: "launch only", launch: "opus", want: "opus"},
		{name: "init wins over launch", init: "claude-opus-5-5", launch: "opus", want: "claude-opus-5-5"},
		{name: "usage wins", usage: "claude-sonnet-5-5", init: "claude-opus-5-5", launch: "opus", want: "claude-sonnet-5-5"},
		{name: "none", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := &cliRun{}
			cr.setModel(tc.init)
			res := &agentwire.Result{}
			res.Usage.Model = tc.usage
			if got := cr.tokenUsage(res, tc.launch).Model; got != tc.want {
				t.Fatalf("model = %q, want %q", got, tc.want)
			}
		})
	}
	if (&cliRun{}).tokenUsage(nil, "opus") != nil {
		t.Fatal("no result must give no usage")
	}

	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil, nil)
	ctx := context.Background()
	run, err := svc.Create(ctx, CreateOpts{WorkspaceID: "ws", JobID: "j", Harness: "claude", Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	svc.setRunModel(ctx, run.ID, 1, "claude-opus-5-5")
	got, err := svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "claude-opus-5-5" {
		t.Fatalf("run model = %q", got.Model)
	}
	var turnModel string
	if err := st.DB().QueryRow(`SELECT tokens_model FROM run_turns WHERE run_id=? AND n=1`, run.ID).Scan(&turnModel); err != nil {
		t.Fatal(err)
	}
	if turnModel != "claude-opus-5-5" {
		t.Fatalf("turn model = %q", turnModel)
	}
}
