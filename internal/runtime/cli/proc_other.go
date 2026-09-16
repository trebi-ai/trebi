//go:build !unix && !windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

// procPlatform is empty off Unix/Windows; there is no group or job primitive.
type procPlatform struct{}

func (p *procPlatform) afterStart(cmd *exec.Cmd) error { return nil }
func (p *procPlatform) afterWait()                     {}

// rejectShim: executable resolution is the platform's problem here.
func rejectShim(path string) error { return nil }

// procSysProcAttr is a no-op on plan9/js/wasi.
func procSysProcAttr() *syscall.SysProcAttr { return nil }

func processGroupID(pid int) int { return 0 }

func sigTerm() os.Signal { return os.Interrupt }
func sigKill() os.Signal { return os.Kill }

func (p *Proc) signalGroup(sig os.Signal) {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(sig)
	}
}

func (p *Proc) killTreePlatform() { p.signalGroup(sigKill()) }

func procStartToken(pid int) string { return "" }

func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}

func killGroup(_, pid int) {
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
