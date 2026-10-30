//go:build live_tui

package agentcli

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/trebi/internal/visor"
)

// TestMain lets the test binary run as the holder of a pane.
func TestMain(m *testing.M) {
	if code, ok := visor.Hold(os.Args); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// Live smoke (go test -tags live_tui): for every harness whose binary is on
// PATH, launch it in a fresh pane in a temp dir and require ready within
// 60s — startup dialogs and all. Missing binaries skip cleanly.
func TestLiveTUISmoke(t *testing.T) {
	home, err := os.MkdirTemp("", "tl")
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
	panes := visor.NewPanes(func(context.Context) (visor.Host, error) { return v, nil })
	ctx := t.Context()
	for _, tool := range AllTools() {
		tool := tool
		t.Run(tool.Name(), func(t *testing.T) {
			bin, err := lookPathToolBinary(tool.Name())
			if err != nil {
				t.Skipf("%s not on PATH", tool.Name())
			}
			id, err := panes.Open(ctx, visor.PaneSpec{Dir: t.TempDir()})
			if err != nil {
				t.Fatalf("open a pane: %v", err)
			}
			defer func() { _ = panes.Kill(context.Background(), id) }()
			sess := panes.Pane(id)

			plan := tool.PrepareLaunch(bin)
			cmd := plan.Command
			// Apply isolation env so hooks/auth land under harness when set.
			if len(plan.Env) > 0 {
				var parts []string
				for k, v := range plan.Env {
					parts = append(parts, k+"="+v)
				}
				if err := sess.Send(ctx, "export "+joinEnvExport(parts)+"\n"); err != nil {
					t.Fatalf("export env: %v", err)
				}
			}
			if err := sess.Send(ctx, cmd+"\n"); err != nil {
				t.Fatalf("launch: %v", err)
			}
			if err := WaitReadyTool(ctx, sess, tool, 60*time.Second); err != nil {
				screen, _ := sess.Screen(ctx)
				t.Fatalf("not ready in 60s: %v\nfinal screen:\n%s", err, screen)
			}
		})
	}
}

func lookPathToolBinary(name string) (string, error) {
	names := []string{name}
	if name == Cursor {
		names = []string{CursorAgentBinary, Cursor}
	}
	var last error
	for _, n := range names {
		p, err := exec.LookPath(n)
		if err == nil {
			return p, nil
		}
		last = err
	}
	return "", last
}

func joinEnvExport(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}
