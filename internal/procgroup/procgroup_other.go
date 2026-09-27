//go:build !unix

package procgroup

import (
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
