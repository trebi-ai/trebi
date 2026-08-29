package cli

import (
	"context"
	"os"
	"strconv"
	"syscall"
	"time"
)

const parentFDEnv = "TREBI_PARENT_FD"

// startParentWatch cancels when the eval (or other) parent dies, including
// SIGKILL: the parent holds the write end of a pipe passed as ExtraFiles.
func startParentWatch(cancel context.CancelFunc, parentPID int) {
	if parentPID <= 0 {
		parentPID, _ = strconv.Atoi(os.Getenv("TREBI_PARENT_PID"))
	}
	fd := parentWatchFile()
	if parentPID <= 0 && fd == nil {
		return
	}
	go watchParent(cancel, parentPID, fd)
}

func parentWatchFile() *os.File {
	v := os.Getenv(parentFDEnv)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 3 {
		return nil
	}
	return os.NewFile(uintptr(n), "parent-watch")
}

func watchParent(cancel context.CancelFunc, pid int, fd *os.File) {
	if fd != nil {
		buf := make([]byte, 1)
		_, _ = fd.Read(buf)
		cancel()
		return
	}
	if pid <= 0 {
		return
	}
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		if !processAlive(pid) {
			cancel()
			return
		}
	}
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
