//go:build unix

package home

import (
	"os"
	"syscall"
)

// AcquireDaemonLock takes an exclusive flock on ~/.trebi/daemon.lock and
// writes this process's pid. On conflict it returns AlreadyRunningError
// without mutating api.json or the store.
func AcquireDaemonLock() (*DaemonLock, error) {
	if err := os.MkdirAll(Base(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(DaemonLockPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		held := alreadyRunningFrom(f)
		_ = f.Close()
		return nil, held
	}
	pid := os.Getpid()
	if err := writeLockPID(f, pid); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, err
	}
	return &DaemonLock{f: f, pid: pid}, nil
}

func unlockDaemonFile(f *os.File) error {
	if f == nil {
		return nil
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func inspectDaemonLock() (held bool, pid int, err error) {
	f, err := os.OpenFile(DaemonLockPath(), os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true, readLockPID(f), nil
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, 0, nil
}
