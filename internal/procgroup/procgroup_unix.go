//go:build unix

package procgroup

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

func setGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := Kill(cmd, syscall.SIGTERM)
		time.AfterFunc(cmd.WaitDelay/2, func() {
			_ = Kill(cmd, syscall.SIGKILL) // the group can be gone already
		})
		return err
	}
}

// Kill sends sig to the process group of a started command.
func Kill(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}

// KillLeft sends SIGKILL to the group of m when its leader still runs. A
// group whose leader ended is not found. It reports if it sent the signal.
func (m Mark) KillLeft(ctx context.Context) bool {
	if m.PGID <= 1 || !m.alive(ctx) {
		return false
	}
	if pgid, err := syscall.Getpgid(m.PGID); err != nil || pgid != m.PGID {
		return false
	}
	return syscall.Kill(-m.PGID, syscall.SIGKILL) == nil
}
