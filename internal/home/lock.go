package home

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// DaemonLock is a process-lifetime exclusive lock on daemon.lock.
type DaemonLock struct {
	f   *os.File
	pid int
}

// PID is the process id written into the lock file.
func (l *DaemonLock) PID() int {
	if l == nil {
		return 0
	}
	return l.pid
}

// Release drops the flock and closes the file.
func (l *DaemonLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlockDaemonFile(l.f)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}

// AlreadyRunningError is returned when a second daemon races the lock.
type AlreadyRunningError struct {
	PID  int
	Addr string
}

func (e AlreadyRunningError) Error() string {
	if e.PID > 0 && e.Addr != "" {
		return fmt.Sprintf("daemon already running (pid %d, listening on %s)", e.PID, e.Addr)
	}
	if e.PID > 0 {
		return fmt.Sprintf("daemon already running (pid %d)", e.PID)
	}
	if e.Addr != "" {
		return fmt.Sprintf("daemon already running (listening on %s)", e.Addr)
	}
	return "daemon already running"
}

func alreadyRunningFrom(lockFile *os.File) error {
	pid := readLockPID(lockFile)
	addr := APIJSONURL()
	return AlreadyRunningError{PID: pid, Addr: addr}
}

func writeLockPID(f *os.File, pid int) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	_, err := fmt.Fprintf(f, "%d\n", pid)
	if err != nil {
		return err
	}
	return f.Sync()
}

func readLockPID(f *os.File) int {
	if f == nil {
		return 0
	}
	if _, err := f.Seek(0, 0); err != nil {
		return 0
	}
	buf := make([]byte, 64)
	n, err := f.Read(buf)
	if err != nil || n == 0 {
		return 0
	}
	line := strings.TrimSpace(string(buf[:n]))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(line))
	return pid
}

// APIJSONURL returns the url field of api.json, or empty.
func APIJSONURL() string {
	b, err := os.ReadFile(APIJSONPath())
	if err != nil {
		return ""
	}
	var doc struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return ""
	}
	return strings.TrimSpace(doc.URL)
}

// DaemonDoctorInfo is the lock + api.json snapshot for trebi doctor.
type DaemonDoctorInfo struct {
	LockHeld         bool   `json:"lock_held"`
	LockPID          int    `json:"lock_pid,omitempty"`
	APIJSONURL       string `json:"api_json_url,omitempty"`
	APIJSONReachable bool   `json:"api_json_reachable"`
}

// DoctorDaemon reports the lock holder and whether api.json answers health.
func DoctorDaemon() DaemonDoctorInfo {
	info := DaemonDoctorInfo{APIJSONURL: APIJSONURL()}
	held, pid, _ := inspectDaemonLock()
	info.LockHeld = held
	info.LockPID = pid
	if info.APIJSONURL != "" {
		info.APIJSONReachable = probeAPIHealth(info.APIJSONURL)
	}
	return info
}

func probeAPIHealth(rawURL string) bool {
	base := strings.TrimRight(rawURL, "/")
	client := &http.Client{Timeout: 400 * time.Millisecond}
	resp, err := client.Get(base + "/v1/system/health")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}
