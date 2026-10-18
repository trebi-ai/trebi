//go:build !unix

package procgroup

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

// setGroup keeps the default cancel: the daemon targets darwin and linux.
func setGroup(*exec.Cmd) {}

// Kill kills the process. The signal is not used on this platform.
func Kill(cmd *exec.Cmd, _ syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// KillLeft kills the leader of m when it still runs. It reports if it did.
func (m Mark) KillLeft(ctx context.Context) bool {
	if m.PGID <= 0 || !m.alive(ctx) {
		return false
	}
	p, err := os.FindProcess(m.PGID)
	return err == nil && p.Kill() == nil
}
