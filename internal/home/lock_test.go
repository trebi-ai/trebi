package home

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcquireDaemonLock_Conflict(t *testing.T) {
	if os.Getenv("TREBI_LOCK_HELPER") == "1" {
		lock, err := AcquireDaemonLock()
		if err != nil {
			os.Exit(2)
		}
		defer lock.Release()
		os.Stdout.Write([]byte("held\n"))
		_ = os.Stdout.Sync()
		time.Sleep(30 * time.Second)
		return
	}

	dir := t.TempDir()
	SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { SetBase("") })

	apiPath := APIJSONPath()
	if err := os.WriteFile(apiPath, []byte(`{"url":"http://127.0.0.1:7433","token":"x","port":7433}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(apiPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestAcquireDaemonLock_Conflict")
	cmd.Env = append(os.Environ(), "TREBI_LOCK_HELPER=1", "TREBI_HOME="+dir)
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 32)
		n, _ := stdout.Read(buf)
		got <- string(buf[:n])
	}()
	var heldOut string
	select {
	case heldOut = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("helper never acquired the lock")
	}
	if !strings.Contains(heldOut, "held") {
		t.Fatalf("helper output %q", heldOut)
	}

	_, err = AcquireDaemonLock()
	if err == nil {
		t.Fatal("second acquire must fail")
	}
	var already AlreadyRunningError
	if !errors.As(err, &already) {
		t.Fatalf("want AlreadyRunningError, got %T %v", err, err)
	}
	if already.PID != cmd.Process.Pid {
		t.Fatalf("pid=%d want helper %d (%v)", already.PID, cmd.Process.Pid, err)
	}
	if !strings.Contains(err.Error(), "daemon already running") {
		t.Fatalf("message: %v", err)
	}
	if !strings.Contains(err.Error(), "http://127.0.0.1:7433") {
		t.Fatalf("want api.json addr in message: %v", err)
	}

	after, err := os.ReadFile(apiPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("second acquire must not rewrite api.json")
	}
	if _, err := os.Stat(filepath.Join(dir, "trebi.db")); err == nil {
		t.Fatal("second acquire must not create the store")
	}
}

func TestDoctorDaemon_StaleAPIJSON(t *testing.T) {
	dir := t.TempDir()
	SetBase(dir)
	t.Cleanup(func() { SetBase("") })
	if err := os.WriteFile(APIJSONPath(), []byte(`{"url":"http://127.0.0.1:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	info := DoctorDaemon()
	if info.APIJSONURL != "http://127.0.0.1:1" {
		t.Fatalf("%+v", info)
	}
	if info.APIJSONReachable {
		t.Fatal("port 1 must not answer health")
	}
}
