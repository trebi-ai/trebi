package runs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

// cliFixture builds a runs service with the fake subprocess driver injected
// and a fake `claude` binary on PATH so resolveCommand/commandUnavailable
// take the vendor path without touching a real vendor CLI.
func cliFixture(t *testing.T, drv agentwire.Driver) (*runs.Service, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	bin := t.TempDir()
	fake := filepath.Join(bin, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	st, err := store.OpenPath(filepath.Join(dir, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := runs.New(st, nil, files.New(files.DefaultMaxBytes))
	svc.SetCLIDriver(drv)
	// Let every event pump finish before the store/home teardown: a pump
	// writing its ledger or a session segment after Close() is a test race,
	// not a product bug.
	t.Cleanup(func() { waitCLIIdle(t, svc) })
	return svc, st, t.TempDir()
}

// waitCLIIdle drains live subprocess sessions before teardown.
func waitCLIIdle(t *testing.T, svc *runs.Service) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if svc.CLIActiveCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	svc.CloseCLIRuns(context.Background())
}

func launchCLIJob(t *testing.T, svc *runs.Service, opts ...func(*runs.LaunchOpts)) *runs.Run {
	t.Helper()
	lopts := runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", Job: "demo", Kind: runs.KindJob, Harness: "claude",
			Reason: runs.ReasonManual, WorkingDir: t.TempDir(),
		},
		Prompt: "do the thing",
	}
	for _, fn := range opts {
		fn(&lopts)
	}
	run, _, err := svc.CreateAndLaunch(context.Background(), lopts)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func waitTerminal(t *testing.T, svc *runs.Service, runID string) *runs.Run {
	t.Helper()
	run, err := svc.WaitFor(context.Background(), runID, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Status.IsTerminal() {
		t.Fatalf("run %s still %s", runID, run.Status)
	}
	return run
}

// TestCLIJobCompletesWithoutTmux is the phase-0 acceptance bar: a job
// completes through the subprocess runtime with no tmux session.
func TestCLIJobCompletesWithoutTmux(t *testing.T) {
	svc, _, _ := cliFixture(t, agentwire.FakeDriver{})
	run := launchCLIJob(t, svc)

	if run.Runtime != "cli" {
		t.Fatalf("runtime = %q", run.Runtime)
	}
	if run.TmuxSession != "" {
		t.Fatalf("tmux session leaked: %q", run.TmuxSession)
	}
	done := waitTerminal(t, svc, run.ID)
	if done.Status != runs.StatusComplete || done.EndReason != runs.EndResult {
		t.Fatalf("run: status=%s end=%s", done.Status, done.EndReason)
	}
	if done.RuntimePID == 0 {
		t.Fatalf("runtime_pid not recorded")
	}
	if done.Attach == nil || done.Attach.Kind != "events" {
		t.Fatalf("attach: %+v", done.Attach)
	}
	if _, err := os.Stat(filepath.Join(home.RunsLogDir(), run.ID+".log")); err != nil {
		t.Fatalf("run log: %v", err)
	}
}

// TestCLIOneShotRunningMidTurn pins that a one-shot run reads running, not
// idle, between init and result: the prompt is on argv, not stdin.
func TestCLIOneShotRunningMidTurn(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "go")
	svc, _, _ := cliFixture(t, agentwire.FakeDriver{
		Script: `echo '{"type":"init","session_id":"s1"}'; echo '{"type":"assistant","text":"working"}'; ` +
			`while [ ! -f ` + gate + ` ]; do sleep 0.02; done; echo '{"type":"result","subtype":"success"}'`,
	})
	run := launchCLIJob(t, svc)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(filepath.Join(home.RunsLogDir(), run.ID+".log")); strings.Contains(string(data), "working") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := svc.SessionState(run.ID); got != runs.SessionRunning {
		t.Fatalf("session state mid-turn = %q, want running", got)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, svc, run.ID)
}

// TestCLIFailingResultClassifies pulls a limit result through the whole path.
func TestCLIFailingResultClassifies(t *testing.T) {
	svc, st, _ := cliFixture(t, agentwire.FakeDriver{
		Script: `read -r line; echo '{"type":"result","subtype":"error_during_execution","is_error":true,"text":"You have hit your session limit","end_reason":"limit"}'`,
	})
	run := launchCLIJob(t, svc)
	done := waitTerminal(t, svc, run.ID)
	if done.Status != runs.StatusFailed || done.EndReason != runs.EndLimit {
		t.Fatalf("run: status=%s end=%s", done.Status, done.EndReason)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic='harness.limit_exceeded'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("limit bus event missing")
	}
}

// TestCLIKillTreeKillsChild pins process-group kill through the service.
func TestCLIKillTreeKillsChild(t *testing.T) {
	svc, _, _ := cliFixture(t, agentwire.FakeDriver{Script: `sleep 60`})
	run := launchCLIJob(t, svc)
	deadline := time.Now().Add(5 * time.Second)
	for run.RuntimePID == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		got, err := svc.Get(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		run = got
	}
	if run.RuntimePID == 0 {
		t.Fatal("pid not recorded")
	}
	pid := run.RuntimePID
	if err := svc.Kill(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	done := waitTerminal(t, svc, run.ID)
	if done.Status != runs.StatusExited || done.EndReason != runs.EndKill {
		t.Fatalf("run: status=%s end=%s", done.Status, done.EndReason)
	}
	waitProcGone(t, pid)
}

// TestCLILogStreamsBeforeTerminate pins the streaming run log (Part H): the
// assistant text exists while the process is still alive.
func TestCLILogStreamsBeforeTerminate(t *testing.T) {
	svc, _, _ := cliFixture(t, agentwire.FakeDriver{
		Script: `read -r line; echo '{"type":"assistant","text":"working on it"}'; sleep 60`,
	})
	run := launchCLIJob(t, svc)
	path := filepath.Join(home.RunsLogDir(), run.ID+".log")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			_ = svc.Kill(context.Background(), run.ID)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run log was not streamed before terminate")
}

// TestCLIApprovalRoundtrip maps a wire permission to the approval inbox and
// forwards the decision back to the session.
func TestCLIApprovalRoundtrip(t *testing.T) {
	svc, _, _ := cliFixture(t, agentwire.FakeDriver{
		Script: `read -r line
echo '{"type":"permission","id":"p1","tool":"Bash","question":"Allow Bash?"}'
read -r decision
echo '{"type":"result","subtype":"success"}'`,
	})
	run := launchCLIJob(t, svc)

	deadline := time.Now().Add(5 * time.Second)
	var approvalID string
	for time.Now().Before(deadline) {
		if svc.SessionState(run.ID) == runs.SessionWaiting {
			list, err := svc.ListApprovals(context.Background(), runs.ApprovalListOpts{State: "pending", Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range list {
				if a.RunID == run.ID {
					approvalID = a.ID
				}
			}
			if approvalID != "" {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if approvalID == "" {
		t.Fatalf("no open approval; state=%s", svc.SessionState(run.ID))
	}
	if _, err := svc.DecideApproval(context.Background(), run.ID, approvalID, "allow"); err != nil {
		t.Fatal(err)
	}
	done := waitTerminal(t, svc, run.ID)
	if done.Status != runs.StatusComplete {
		t.Fatalf("run: status=%s end=%s", done.Status, done.EndReason)
	}
}

// TestCLIMemoryFixCycle proves the memory fix is a second prompt on the same
// session, not a pane paste.
func TestCLIMemoryFixCycle(t *testing.T) {
	root := t.TempDir()
	fixed := filepath.Join(root, "fixed.md")
	if err := os.WriteFile(fixed, append(memory.Skeleton(), []byte("- [2026-01-01 00:00] ran\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	svc, _, _ := cliFixture(t, agentwire.FakeDriver{
		Script: `read -r line
echo '{"type":"result","subtype":"success"}'
read -r fix
cp "` + fixed + `" "$TREBI_MEMORY_PATH"
echo '{"type":"result","subtype":"success"}'`,
	})
	run := launchCLIJob(t, svc, func(o *runs.LaunchOpts) {
		o.WorkingDir = root
		o.Memory = &runs.MemoryLaunch{
			Root: root, Rel: filepath.Join("demo", "MEMORY.md"),
			Opt: memory.Options{MaxLogs: 100, RequireUpdate: true, InjectLogs: 20},
		}
	})
	done := waitTerminal(t, svc, run.ID)
	if done.Status != runs.StatusComplete {
		t.Fatalf("run: status=%s end=%s", done.Status, done.EndReason)
	}
	kinds := runEventKinds(t, svc, run.ID)
	if kinds["memory.fix_requested"] != 1 {
		t.Fatalf("memory events: %v", kinds)
	}
}

// TestCLIRecoverMarksSessionGone simulates a daemon restart: the ledger child
// is killed and the live row reconciles to session_gone.
func TestCLIRecoverMarksSessionGone(t *testing.T) {
	svc, st, _ := cliFixture(t, agentwire.FakeDriver{Script: `sleep 60`})
	run := launchCLIJob(t, svc)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := svc.Get(context.Background(), run.ID)
		if err == nil && got.RuntimePID != 0 {
			run = got
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	pid := run.RuntimePID
	if pid == 0 {
		t.Fatal("pid not recorded")
	}
	// A fresh service has no live registry, exactly like a booted daemon.
	svc2 := runs.New(st, nil, nil)
	svc2.RecoverCLI(context.Background())
	got, err := svc2.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runs.StatusExited || got.EndReason != runs.EndSessionGone {
		t.Fatalf("recovered run: status=%s end=%s", got.Status, got.EndReason)
	}
	waitProcGone(t, pid)
	_ = svc
}

// TestCLIConcurrentApprovals pins the keyed approval path (A.10): two wire
// permissions each open their own row, and each decision reaches the session.
func TestCLIConcurrentApprovals(t *testing.T) {
	svc, _, _ := cliFixture(t, agentwire.FakeDriver{
		Script: `read -r line
echo '{"type":"permission","id":"p1","tool":"Bash","question":"first?"}'
echo '{"type":"permission","id":"p2","tool":"Bash","question":"second?"}'
read -r d1
read -r d2
echo '{"type":"result","subtype":"success"}'`,
	})
	run := launchCLIJob(t, svc)
	ctx := context.Background()

	pending := func() map[string]string {
		list, err := svc.ListApprovals(ctx, runs.ApprovalListOpts{State: "pending", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		byKey := map[string]string{}
		for _, a := range list {
			if a.RunID == run.ID {
				byKey[a.Key] = a.ID
			}
		}
		return byKey
	}
	deadline := time.Now().Add(5 * time.Second)
	var byKey map[string]string
	for time.Now().Before(deadline) {
		byKey = pending()
		if byKey["p1"] != "" && byKey["p2"] != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if byKey["p1"] == "" || byKey["p2"] == "" {
		t.Fatalf("expected one row per wire permission: %v", byKey)
	}
	// Decide the newer one first: the older must stay pending and the session
	// must still get both answers for the run to complete.
	if _, err := svc.DecideApproval(ctx, run.ID, byKey["p2"], "allow"); err != nil {
		t.Fatal(err)
	}
	left := pending()
	if left["p1"] == "" {
		t.Fatalf("deciding p2 cleared p1: %v", left)
	}
	if left["p2"] != "" {
		t.Fatalf("p2 still pending: %v", left)
	}
	if _, err := svc.DecideApproval(ctx, run.ID, byKey["p1"], "allow"); err != nil {
		t.Fatal(err)
	}
	done := waitTerminal(t, svc, run.ID)
	if done.Status != runs.StatusComplete {
		t.Fatalf("run: status=%s end=%s", done.Status, done.EndReason)
	}
}

// TestCLITurnsPrefersWireEvents pins the Part H transcript order: the turn
// list is rebuilt from run_events, not the vendor file.
func TestCLITurnsPrefersWireEvents(t *testing.T) {
	svc, _, _ := cliFixture(t, agentwire.FakeDriver{
		Script: `read -r line
echo '{"type":"assistant","text":"hello from wire"}'
echo '{"type":"tool","id":"t1","name":"Bash","status":"completed","output":"ok"}'
echo '{"type":"result","subtype":"success"}'`,
	})
	run := launchCLIJob(t, svc)
	waitTerminal(t, svc, run.ID)

	turns, err := svc.CLITurns(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sawText, sawTool bool
	for _, turn := range turns {
		if turn.Role == "assistant" && strings.Contains(turn.Text, "hello from wire") {
			sawText = true
		}
		for _, tool := range turn.Tools {
			if tool.Name == "Bash" && tool.Output == "ok" {
				sawTool = true
			}
		}
	}
	if !sawText || !sawTool {
		t.Fatalf("turns: %+v", turns)
	}
}

// TestCLITurnsMatchesToolResultByID covers the claude wire shape: the
// tool_result frame carries only the id. The opened call keeps its name and
// input, the completion stamps the turn, and a call that never closes keeps
// its start time instead of a zero timestamp.
func TestCLITurnsMatchesToolResultByID(t *testing.T) {
	svc, _, _ := cliFixture(t, agentwire.FakeDriver{
		Script: `read -r line
echo '{"type":"tool","id":"t1","name":"Bash","status":"started","input":"{}"}'
echo '{"type":"tool","id":"t1","name":"","status":"completed","output":"ok"}'
echo '{"type":"tool","id":"t2","name":"Read","status":"started","input":"{}"}'
echo '{"type":"result","subtype":"success"}'`,
	})
	run := launchCLIJob(t, svc)
	waitTerminal(t, svc, run.ID)

	turns, err := svc.CLITurns(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var bashAt, readAt *time.Time
	bashOut := ""
	for i := range turns {
		for _, tool := range turns[i].Tools {
			at := turns[i].At
			switch tool.Name {
			case "Bash":
				bashOut, bashAt = tool.Output, &at
			case "Read":
				readAt = &at
			}
		}
	}
	if bashAt == nil || bashOut != "ok" {
		t.Fatalf("Bash turn: out=%q at=%v turns=%+v", bashOut, bashAt, turns)
	}
	if bashAt.IsZero() {
		t.Fatalf("Bash turn must keep the completion time: %+v", turns)
	}
	if readAt == nil {
		t.Fatalf("Read turn missing: %+v", turns)
	}
	if readAt.IsZero() {
		t.Fatalf("started-only turn must keep its start time: %+v", turns)
	}
}

// TestCLIEventsSince covers the attach cursor: events replay from id 0 and
// resume after the last seen id.
func TestCLIEventsSince(t *testing.T) {
	svc, _, _ := cliFixture(t, agentwire.FakeDriver{
		Script: `read -r line; echo '{"type":"assistant","text":"hi"}'; echo '{"type":"result","subtype":"success"}'`,
	})
	run := launchCLIJob(t, svc)
	waitTerminal(t, svc, run.ID)

	events, err := svc.EventsSince(context.Background(), run.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 {
		t.Fatalf("events: %+v", events)
	}
	var lastID int64
	kinds := map[string]bool{}
	for _, ev := range events {
		if ev.ID <= lastID {
			t.Fatalf("event ids not ascending: %d after %d", ev.ID, lastID)
		}
		lastID = ev.ID
		kinds[ev.Kind] = true
	}
	if !kinds["launch"] || !kinds["assistant"] {
		t.Fatalf("event kinds: %v", kinds)
	}
	rest, err := svc.EventsSince(context.Background(), run.ID, lastID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 {
		t.Fatalf("tail should be empty: %+v", rest)
	}
}

func waitProcGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("process %d survived", pid)
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// captureDriver runs the fake driver and keeps every StartRequest.
type captureDriver struct {
	agentwire.FakeDriver
	mu   sync.Mutex
	reqs []agentwire.StartRequest
}

func (d *captureDriver) Start(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	d.record(req)
	return d.FakeDriver.Start(ctx, req)
}

func (d *captureDriver) Resume(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	d.record(req)
	return d.FakeDriver.Resume(ctx, req)
}

func (d *captureDriver) record(req agentwire.StartRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reqs = append(d.reqs, req)
}

func (d *captureDriver) last(t *testing.T) agentwire.StartRequest {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.reqs) == 0 {
		t.Fatal("no start request")
	}
	return d.reqs[len(d.reqs)-1]
}

// mcpResolver gives every name one stdio MCP server.
type mcpResolver struct{}

func (mcpResolver) ResolveActions(_ context.Context, _ string, names []string) ([]runs.ConnectorAction, error) {
	out := make([]runs.ConnectorAction, 0, len(names))
	for _, n := range names {
		out = append(out, runs.ConnectorAction{Name: n, MCP: &agentwire.MCPServer{
			Name: n, Command: "npx", Env: map[string]string{"TOKEN": "secret-" + n},
		}})
	}
	return out, nil
}

// TestCLIMCPActionNotInjectedTwice proves the CLI runtime passes MCP actions
// only through StartRequest.MCPServers (P1-19): gemini is not refused, and
// no harness gets the pane flags too.
func TestCLIMCPActionNotInjectedTwice(t *testing.T) {
	drv := &captureDriver{}
	svc, _, _ := cliFixture(t, drv)
	svc.WithActions(mcpResolver{})
	fakeBinaries(t, "gemini", "claude")
	for _, h := range []string{"gemini", "claude"} {
		run := launchCLIJob(t, svc, func(o *runs.LaunchOpts) {
			o.Harness = h
			o.JobConnectors = []string{"gh"}
		})
		if run.Runtime != "cli" {
			t.Fatalf("%s runtime = %q", h, run.Runtime)
		}
		req := drv.last(t)
		if len(req.MCPServers) != 1 || req.MCPServers[0].Name != "gh" {
			t.Fatalf("%s servers = %+v", h, req.MCPServers)
		}
		if strings.Contains(strings.Join(req.ExtraArgs, " "), "--mcp-config") {
			t.Fatalf("%s extra args = %v", h, req.ExtraArgs)
		}
		waitTerminal(t, svc, run.ID)
	}
}

// TestResumeKeepsConnectors proves a resume resolves the connector names of
// the run it resumes (P1-20).
func TestResumeKeepsConnectors(t *testing.T) {
	drv := &captureDriver{}
	svc, _, _ := cliFixture(t, drv)
	svc.WithActions(mcpResolver{})
	fakeBinaries(t, "claude")
	ctx := context.Background()
	prev, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", Job: "demo", Harness: "claude", Runtime: "cli",
		SessionID: "sess-1", Connectors: []string{"gh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, prev.ID, runs.StatusFailed, runs.EndLaunch, nil); err != nil {
		t.Fatal(err)
	}
	run, err := svc.Resume(ctx, prev.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := drv.last(t)
	if len(req.MCPServers) != 1 || req.MCPServers[0].Name != "gh" {
		t.Fatalf("resume servers = %+v", req.MCPServers)
	}
	waitTerminal(t, svc, run.ID)
}

// fakeBinaries puts no-op vendor binaries on PATH.
func fakeBinaries(t *testing.T, names ...string) {
	t.Helper()
	bin := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANTHROPIC_API_KEY", "test")
	t.Setenv("GEMINI_API_KEY", "test")
}
