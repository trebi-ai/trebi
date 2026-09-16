package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// startTestProc runs a shell script as a Proc and returns it.
func startTestProc(t *testing.T, script string) *Proc {
	t.Helper()
	proc, err := StartProc(context.Background(), ProcOpts{Path: "sh", Args: []string{"-c", script}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return proc
}

// TestGracefulClosePreservesFinalLine pins A.15: closing stdin lets the child
// flush its last line before any signal.
func TestGracefulClosePreservesFinalLine(t *testing.T) {
	t.Parallel()
	proc := startTestProc(t, `cat >/dev/null; echo "last assistant line"; sleep 0.05`)
	if err := proc.gracefulClose(3 * time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, _ := io.ReadAll(proc.Stdout())
	if !strings.Contains(string(got), "last assistant line") {
		t.Fatalf("final line lost: %q", got)
	}
}

// TestKillTreeKillsGrandchild pins process-group escalation.
func TestKillTreeKillsGrandchild(t *testing.T) {
	t.Parallel()
	proc := startTestProc(t, `sleep 60 & echo "child $!"; sleep 60`)
	line, err := bufio.NewReader(proc.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		t.Fatalf("no child pid: %q", line)
	}
	var child int
	if _, err := fmt.Sscanf(fields[1], "%d", &child); err != nil {
		t.Fatalf("child pid: %v", err)
	}
	if !procAlive(child) {
		t.Fatalf("grandchild %d should be alive", child)
	}
	_ = proc.KillTree()
	deadline := time.Now().Add(3 * time.Second)
	for procAlive(child) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if procAlive(child) {
		t.Fatalf("grandchild %d survived process-group kill", child)
	}
}

// TestFrameReassemblyPartialLines feeds one JSON frame split across writes.
func TestFrameReassemblyPartialLines(t *testing.T) {
	t.Parallel()
	proc := startTestProc(t, `printf '{"type":"assist'; sleep 0.1; printf 'ant","text":"hello"}\n'; sleep 0.05`)
	s := startWire(context.Background(), "fake", proc, fakeProtocol{})
	var got []Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				if len(got) == 0 || got[0].Type != EventAssistant || got[0].Text != "hello" {
					t.Fatalf("events: %+v", got)
				}
				return
			}
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timeout waiting for events: %+v", got)
		}
	}
}

// TestGiantToolPayloadFitsFrame pins the 1 MiB frame budget.
func TestGiantToolPayloadFitsFrame(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 300<<10)
	script := fmt.Sprintf(`printf '{"type":"tool","name":"Bash","status":"completed","output":"%s"}\n'`, big)
	proc := startTestProc(t, script)
	s := startWire(context.Background(), "fake", proc, fakeProtocol{})
	select {
	case ev := <-s.Events():
		if ev.Type != EventTool || ev.Tool == nil || len(ev.Tool.Output) != len(big) {
			t.Fatalf("giant payload mangled: type=%s len=%d", ev.Type, len(ev.Tool.Output))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for giant frame")
	}
}

// TestRegistryLedgerReconcileKillsOwnedChild kills a positively matched pid.
func TestRegistryLedgerReconcileKillsOwnedChild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := dir + "/cli-procs.json"
	reg := NewRegistry(path)
	proc, err := StartProc(context.Background(), ProcOpts{Path: "sh", Args: []string{"-c", "sleep 60"}})
	if err != nil {
		t.Fatal(err)
	}
	s := startWire(context.Background(), "fake", proc, fakeProtocol{})
	reg.Add("run-1", s)
	if proc.PID() <= 0 {
		t.Fatal("no pid")
	}
	killed, stale := reg.Reconcile()
	if len(killed) != 1 || killed[0] != "run-1" || len(stale) != 1 {
		t.Fatalf("killed=%v stale=%v", killed, stale)
	}
	deadline := time.Now().Add(3 * time.Second)
	for procAlive(proc.PID()) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if procAlive(proc.PID()) {
		t.Fatalf("child %d survived reconcile", proc.PID())
	}
}

// TestRegistryTrackServer pins the helper ledger shape (E.3): Track records a
// non-session process and Remove drops it.
func TestRegistryTrackServer(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/servers.json"
	reg := NewRegistry(path)
	reg.Track("opencode:key", "opencode", 4242, 4242, []string{"/bin/opencode", "serve", "--port", "1234"})
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("ledger: %+v", list)
	}
	if list[0].RunID != "opencode:key" || list[0].Tool != "opencode" || len(list[0].Argv) != 4 {
		t.Fatalf("entry: %+v", list[0])
	}
	reg.Remove("opencode:key")
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("after remove: %+v", got)
	}
}

// TestRejectShimAcceptsNative pins the native path (Windows refusal has its
// own build-tagged test).
func TestRejectShimAcceptsNative(t *testing.T) {
	t.Parallel()
	if err := rejectShim("/usr/local/bin/claude"); err != nil {
		t.Fatalf("native binary refused: %v", err)
	}
}

// TestRegistryDoesNotKillReusedPID verifies the start-token guard.
func TestRegistryDoesNotKillReusedPID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := dir + "/cli-procs.json"
	reg := NewRegistry(path)
	entries := []LedgerEntry{{RunID: "run-x", PID: 99999999, StartToken: "not-ours"}}
	data, _ := json.Marshal(entries)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	killed, stale := reg.Reconcile()
	if len(killed) != 0 || len(stale) != 1 {
		t.Fatalf("killed=%v stale=%v", killed, stale)
	}
}
