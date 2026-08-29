//go:build !unix

package home

import "os"

// AcquireDaemonLock is a best-effort pid file on non-unix (no flock).
func AcquireDaemonLock() (*DaemonLock, error) {
	if err := os.MkdirAll(Base(), 0o755); err != nil {
		return nil, err
	}
	path := DaemonLockPath()
	if f, err := os.Open(path); err == nil {
		held := alreadyRunningFrom(f)
		_ = f.Close()
		if held.(AlreadyRunningError).PID > 0 {
			return nil, held
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	pid := os.Getpid()
	if err := writeLockPID(f, pid); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &DaemonLock{f: f, pid: pid}, nil
}

func unlockDaemonFile(*os.File) error { return nil }

func inspectDaemonLock() (held bool, pid int, err error) {
	f, err := os.Open(DaemonLockPath())
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, err
	}
	defer f.Close()
	pid = readLockPID(f)
	return pid > 0, pid, nil
}
