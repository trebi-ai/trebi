//go:build unix

package cli

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// procPlatform is empty on Unix; Setpgid already gives group control.
type procPlatform struct{}

func (p *procPlatform) afterStart(cmd *exec.Cmd) error { return nil }
func (p *procPlatform) afterWait()                     {}

// rejectShim is Windows-only; Unix resolves shebang executables itself.
func rejectShim(path string) error { return nil }

// procSysProcAttr puts the child in its own process group so kill escalates to
// the whole tree (npm wrappers, shells, language servers).
func procSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func (p *Proc) killTreePlatform() { p.signalGroup(sigKill()) }

// processGroupID is PID once Setpgid is in effect.
func processGroupID(pid int) int { return pid }

func sigTerm() os.Signal { return syscall.SIGTERM }
func sigKill() os.Signal { return syscall.SIGKILL }

// signalGroup targets the process group first (negative pid), then the
// process, so a child that changed its own group still dies.
func (p *Proc) signalGroup(sig os.Signal) {
	if s, ok := sig.(syscall.Signal); ok && p.pgid > 0 {
		if err := syscall.Kill(-p.pgid, s); err == nil {
			return
		}
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(sig)
	}
}

// procStartToken returns an identity token for a live pid: its start time.
// A mismatched token means the pid was reused and is not ours.
func procStartToken(pid int) string {
	if pid <= 0 {
		return ""
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// procAlive reports whether pid exists.
func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// killGroup SIGKILLs the process group, then the process.
func killGroup(pgid, pid int) {
	if pgid > 0 {
		if syscall.Kill(-pgid, syscall.SIGKILL) == nil {
			return
		}
	}
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
