//go:build windows

package cli

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procPlatform carries the Windows job object so KillTree terminates the
// whole process tree even when the child spawned helpers after start.
type procPlatform struct {
	job windows.Handle
}

// afterStart assigns the child to a kill-on-close job object. Assignment can
// fail when the daemon itself already runs inside a job (CI, some terminals);
// that is not fatal, it only weakens tree-kill to root-process kill.
func (p *procPlatform) afterStart(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return nil
	}
	err = windows.AssignProcessToJobObject(job, handle)
	windows.CloseHandle(handle)
	if err != nil {
		windows.CloseHandle(job)
		return nil
	}
	p.job = job
	return nil
}

// afterWait releases the job handle; KILL_ON_JOB_CLOSE reaps any stragglers.
func (p *procPlatform) afterWait() {
	if p.job != 0 {
		_ = windows.CloseHandle(p.job)
		p.job = 0
	}
}

// killTreePlatform terminates the whole job when one was assigned, else the
// root process. signalGroup remains the fallback for a bare process.
func (p *Proc) killTreePlatform() {
	if p.platform.job != 0 {
		_ = windows.TerminateJobObject(p.platform.job, 1)
		return
	}
	p.signalGroup(sigKill())
}

// rejectShim refuses npm .cmd/.bat wrappers: they re-parse the command line
// and a structured-protocol child must be a native executable (Part D).
func rejectShim(path string) error {
	low := strings.ToLower(path)
	if strings.HasSuffix(low, ".cmd") || strings.HasSuffix(low, ".bat") {
		return &shimError{path: path}
	}
	return nil
}

// procSysProcAttr starts the child in its own console process group.
func procSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func processGroupID(pid int) int { return pid }

func sigTerm() os.Signal { return os.Interrupt }
func sigKill() os.Signal { return os.Kill }

func (p *Proc) signalGroup(sig os.Signal) {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(sig)
	}
}

// procStartToken uses the process creation time as the positive-match token,
// mirroring lstart= on Unix.
func procStartToken(pid int) string {
	if pid <= 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		return ""
	}
	return time.Unix(0, created.Nanoseconds()).UTC().Format(time.RFC3339Nano)
}

func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}

// killGroup kills one process; the registry fallback has no job handle, so a
// boot reconcile targets the recorded server/child root only.
func killGroup(_, pid int) {
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
