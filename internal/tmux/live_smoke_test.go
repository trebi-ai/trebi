//go:build live_tui

package tmux

import (
	"os/exec"
	"testing"
	"time"
)

// Live smoke (go test -tags live_tui): for every harness whose binary is on
// PATH, launch it in a fresh tmux session in a temp dir and require ready
// within 60s — startup dialogs and all. Missing binaries skip cleanly.
func TestLiveTUISmoke(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	for _, tool := range AllTools() {
		tool := tool
		t.Run(tool.Name(), func(t *testing.T) {
			bin, err := lookPathToolBinary(tool.Name())
			if err != nil {
				t.Skipf("%s not on PATH", tool.Name())
			}
			sess, err := NewSession(t.TempDir())
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			defer func() { _ = KillSession(sess) }()

			plan := tool.PrepareLaunch(bin)
			cmd := plan.Command
			// Apply isolation env so hooks/auth land under harness when set.
			if len(plan.Env) > 0 {
				var parts []string
				for k, v := range plan.Env {
					parts = append(parts, k+"="+v)
				}
				if err := Send(sess, "export "+joinEnvExport(parts)+"\n"); err != nil {
					t.Fatalf("export env: %v", err)
				}
			}
			if err := Send(sess, cmd+"\n"); err != nil {
				t.Fatalf("launch: %v", err)
			}
			if err := WaitReadyTool(sess, tool, 60*time.Second); err != nil {
				screen, _ := ReadScreen(sess)
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
