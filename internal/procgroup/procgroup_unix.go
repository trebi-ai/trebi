//go:build unix

package procgroup

import (
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
