package runs_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/trebi-ai/trebi/internal/tmux"
	"github.com/trebi-ai/trebi/internal/tools"
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
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("tools:\n  claude: {enabled: true}\n  codex: {enabled: true}\n  grok: {enabled: true}\n  opencode: {enabled: true}\n  cursor: {enabled: true}\n  pi: {enabled: true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

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
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Kind: runs.KindJob, Harness: "claude",
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

// TestCLIRunningMidTurn pins that a run reads running, not idle, between
// init and result.
func TestCLIRunningMidTurn(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "go")
	svc, _, _ := cliFixture(t, agentwire.FakeDriver{
		Script: `read -r line; echo '{"type":"init","session_id":"s1"}'; echo '{"type":"assistant","text":"working"}'; ` +
			`while [ ! -f ` + gate + ` ]; do sleep 0.02; done; echo '{"type":"result","subtype":"success"}'`,
	})
	run := launchCLIJob(t, svc)
	waitLog(t, run.ID, "working")
	if got := svc.SessionState(run.ID); got != runs.SessionRunning {
		t.Fatalf("session state mid-turn = %q, want running", got)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, svc, run.ID)
}

// TestCLIMessageMidTurn pins the two live paths of a message: a harness that
// steers adds it to the running turn, and one that cannot holds it and sends
// it as the next turn when the result lands. Each prompt is a user event with
// its origin and delivery.
func TestCLIMessageMidTurn(t *testing.T) {
	cases := []struct {
		name    string
		noSteer bool
		want    string
		reply   string
	}{
		{name: "steer", want: "steered", reply: "steer: Also check totals."},
		{name: "held", noSteer: true, want: "held", reply: "next: Also check totals."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := filepath.Join(t.TempDir(), "go")
			script := `read -r line; echo '{"type":"init","session_id":"s1"}'; echo '{"type":"assistant","text":"working"}'; `
			if tc.noSteer {
				script += `while [ ! -f ` + gate + ` ]; do sleep 0.02; done; echo '{"type":"result","subtype":"success"}'; ` +
					`read -r next; echo '{"type":"assistant","text":"next: Also check totals."}'; echo '{"type":"result","subtype":"success"}'`
			} else {
				script += `read -r next; echo '{"type":"assistant","text":"steer: Also check totals."}'; echo '{"type":"result","subtype":"success"}'`
			}
			svc, _, _ := cliFixture(t, agentwire.FakeDriver{Script: script, NoSteer: tc.noSteer})
			run := launchCLIJob(t, svc)
			waitLog(t, run.ID, "working")
			res, err := svc.SendMessage(context.Background(), run.ID, "Also check totals.")
			if err != nil {
				t.Fatal(err)
			}
			if res.Delivery != tc.want || res.RunID != run.ID {
				t.Fatalf("result %+v, want delivery %s", res, tc.want)
			}
			if err := os.WriteFile(gate, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			done := waitTerminal(t, svc, run.ID)
			if done.Status != runs.StatusComplete {
				t.Fatalf("run: status=%s end=%s", done.Status, done.EndReason)
			}
			turns, err := svc.CLITurns(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var origins []string
			var replied bool
			for _, turn := range turns {
				if turn.Role == "user" {
					origins = append(origins, turn.Origin+"/"+turn.Delivery)
				}
				replied = replied || (turn.Role == "assistant" && turn.Text == tc.reply)
			}
			want := []string{runs.OriginLaunch + "/prompted", runs.OriginMessage + "/" + tc.want}
			if !slices.Equal(origins, want) || !replied {
				t.Fatalf("user turns %v (want %v), replied %v: %+v", origins, want, replied, turns)
			}
		})
	}
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

// startErrDriver fails every Start with err, as a vendor handshake does.
type startErrDriver struct {
	agentwire.FakeDriver
	err error
}

func (d startErrDriver) Start(context.Context, agentwire.StartRequest) (agentwire.Session, error) {
	return nil, d.err
}

// TestCLIStartFailureClassifies ends a handshake login failure as auth with
// a hint and the login alert event. Other start errors stay launch.
func TestCLIStartFailureClassifies(t *testing.T) {
	cases := []struct {
		name string
		err  string
		end  runs.EndReason
		auth int
	}{
		{"login", `cursor: jsonrpc error -32000: Authentication required ({"message":"Authentication required. Please run 'agent login' first."})`, runs.EndAuth, 1},
		{"wire", "opencode PUT /session/s1/instructions/entries/agentwire.instructions: HTTP 404", runs.EndLaunch, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, _ := cliFixture(t, startErrDriver{err: errors.New(tc.err)})
			fakeBinaries(t, "claude")
			run := launchCLIJob(t, svc)
			done := waitTerminal(t, svc, run.ID)
			if done.Status != runs.StatusFailed || done.EndReason != tc.end {
				t.Fatalf("run: status=%s end=%s, want failed/%s", done.Status, done.EndReason, tc.end)
			}
			var n int
			if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic='auth.probe.failed'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != tc.auth {
				t.Fatalf("auth.probe.failed events = %d, want %d", n, tc.auth)
			}
			evs, err := svc.EventsSince(context.Background(), run.ID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			var hint bool
			for _, ev := range evs {
				hint = hint || (ev.Kind == "error" && strings.Contains(string(ev.Detail), `"hint"`))
			}
			if hint != (tc.auth == 1) {
				t.Fatalf("hint on error event = %v: %+v", hint, evs)
			}
		})
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

// TestCLIRecoverParksRestart simulates a crash: a fresh service finds live
// cli rows at boot. A row with a session parks with end reason restart, a
// row without one ends session_gone, and the ledger child is killed.
func TestCLIRecoverParksRestart(t *testing.T) {
	svc, st, _ := cliFixture(t, agentwire.FakeDriver{Script: `sleep 60`})
	ctx := context.Background()
	run := waitRunPID(t, svc, launchCLIJob(t, svc).ID)
	bare, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "opencode", Runtime: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh service has no live registry, exactly like a booted daemon.
	svc2 := runs.New(st, nil, nil)
	svc2.RecoverCLI(ctx)
	cases := []struct {
		id     string
		status runs.Status
		end    runs.EndReason
		event  string
	}{
		{id: run.ID, status: runs.StatusParked, end: runs.EndRestart, event: "paused"},
		{id: bare.ID, status: runs.StatusExited, end: runs.EndSessionGone, event: "recovered"},
	}
	for _, tc := range cases {
		got, err := svc2.Get(ctx, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != tc.status || got.EndReason != tc.end {
			t.Fatalf("run %s: status=%s end=%s, want %s/%s", tc.id, got.Status, got.EndReason, tc.status, tc.end)
		}
		if runEventKinds(t, svc2, tc.id)[tc.event] != 1 {
			t.Fatalf("run %s: no %s event", tc.id, tc.event)
		}
	}
	waitProcGone(t, run.RuntimePID)
}

// TestCLIPauseAtShutdown pins PauseCLIRuns: each live run gets its final row
// at once, the child stops, and the draining pump never rewrites the row.
func TestCLIPauseAtShutdown(t *testing.T) {
	const init = `read -r l; echo '{"type":"init","session_id":"s1"}'; `
	cases := []struct {
		name      string
		script    string
		keepAlive int64
		ready     func(t *testing.T, svc *runs.Service, runID string)
		status    runs.Status
		end       runs.EndReason
		input     int64
		expired   int
	}{
		{
			name:   "mid-turn",
			script: init + `echo '{"type":"assistant","text":"working"}'; sleep 60`,
			ready: func(t *testing.T, svc *runs.Service, id string) {
				waitLog(t, id, "working")
				waitSessionState(t, svc, id, runs.SessionRunning)
			},
			status: runs.StatusParked, end: runs.EndRestart,
		},
		{
			name: "second vendor turn keeps usage",
			script: init + `echo '{"type":"result","subtype":"success","usage":{"input":7,"output":3}}'; ` +
				`read -r l2; echo '{"type":"assistant","text":"second"}'; sleep 60`,
			keepAlive: int64(time.Hour / time.Millisecond),
			ready: func(t *testing.T, svc *runs.Service, id string) {
				waitSessionState(t, svc, id, runs.SessionIdle)
				if _, err := svc.SendMessage(context.Background(), id, "one more step"); err != nil {
					t.Fatal(err)
				}
				waitLog(t, id, "second")
				waitSessionState(t, svc, id, runs.SessionRunning)
			},
			status: runs.StatusParked, end: runs.EndRestart, input: 7,
		},
		{
			name:   "waiting for an approval",
			script: init + `echo '{"type":"permission","id":"p1","tool":"Bash","question":"run it?"}'; sleep 60`,
			ready: func(t *testing.T, svc *runs.Service, id string) {
				waitSessionState(t, svc, id, runs.SessionWaiting)
			},
			status: runs.StatusParked, end: runs.EndRestart, expired: 1,
		},
		{
			name:      "keep-alive window",
			script:    init + `echo '{"type":"result","subtype":"success"}'; sleep 60`,
			keepAlive: int64(time.Hour / time.Millisecond),
			ready: func(t *testing.T, svc *runs.Service, id string) {
				waitSessionState(t, svc, id, runs.SessionIdle)
			},
			status: runs.StatusComplete, end: runs.EndKeepAlive,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _ := cliFixture(t, agentwire.FakeDriver{Script: tc.script})
			ctx := context.Background()
			run := launchCLIJob(t, svc, func(o *runs.LaunchOpts) { o.KeepAliveMS = tc.keepAlive })
			run = waitRunPID(t, svc, run.ID)
			tc.ready(t, svc, run.ID)

			start := time.Now()
			svc.PauseCLIRuns(ctx)
			if d := time.Since(start); d > 10*time.Second {
				t.Fatalf("pause took %s", d)
			}
			waitProcGone(t, run.RuntimePID)
			waitCLIIdle(t, svc)
			// The pump drained after the pause: the row must be as the pause left it.
			got, err := svc.Get(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.status || got.EndReason != tc.end {
				t.Fatalf("status=%s end=%s, want %s/%s", got.Status, got.EndReason, tc.status, tc.end)
			}
			if got.Tokens.Input != tc.input {
				t.Fatalf("input tokens = %d, want %d", got.Tokens.Input, tc.input)
			}
			expired, err := svc.ListApprovals(ctx, runs.ApprovalListOpts{State: "expired", Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(expired) != tc.expired {
				t.Fatalf("expired approvals = %d, want %d", len(expired), tc.expired)
			}
			if pending, _ := svc.PendingApproval(ctx, run.ID); pending != nil {
				t.Fatalf("approval still pending: %+v", pending)
			}
		})
	}
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

// TestCLITurnsKeepsLongText pins that a long reply reaches the transcript
// whole. A 2000-byte cap cut run reports in the middle.
func TestCLITurnsKeepsLongText(t *testing.T) {
	svc, _, _ := cliFixture(t, agentwire.FakeDriver{
		Script: `read -r line
text=$(awk 'BEGIN { for (i = 0; i < 1000; i++) printf "line %04d. ", i; printf "END" }')
echo "{\"type\":\"assistant\",\"text\":\"$text\"}"
echo '{"type":"result","subtype":"success"}'`,
	})
	run := launchCLIJob(t, svc)
	waitTerminal(t, svc, run.ID)

	turns, err := svc.CLITurns(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range turns {
		if turn.Role == "assistant" && strings.HasSuffix(turn.Text, "END") && len(turn.Text) > 10000 {
			return
		}
	}
	t.Fatalf("long reply cut: %+v", turns)
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
		out = append(out, runs.ConnectorAction{ID: n, Slug: n, MCP: &agentwire.MCPServer{
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

// proxyResolver gives one online server behind the Trebi proxy.
type proxyResolver struct{}

// loginResolver gives a connection that needs a login.
type loginResolver struct{}

func (proxyResolver) ResolveActions(_ context.Context, _ string, names []string) ([]runs.ConnectorAction, error) {
	return []runs.ConnectorAction{{ID: names[0], Slug: names[0], RunToken: true, Note: "The token of Granola expires at noon.",
		MCP: &agentwire.MCPServer{Name: names[0], URL: "http://127.0.0.1:7433/v1/connectors/" + names[0] + "/mcp"}}}, nil
}

func (loginResolver) ResolveActions(context.Context, string, []string) ([]runs.ConnectorAction, error) {
	return nil, &runs.ConnectorLoginError{Connector: "con_1", Title: "Granola"}
}

// TestCLIProxyActionGetsRunToken proves a proxy server gets the run token
// of its own run, and the note goes to the run log.
func TestCLIProxyActionGetsRunToken(t *testing.T) {
	drv := &captureDriver{}
	svc, _, _ := cliFixture(t, drv)
	svc.WithActions(proxyResolver{})
	fakeBinaries(t, "claude")
	run := launchCLIJob(t, svc, func(o *runs.LaunchOpts) {
		o.Harness = "claude"
		o.JobConnectors = []string{"granola"}
	})
	req := drv.last(t)
	tok := req.MCPServers[0].Headers[tools.HeaderRunToken]
	if tok == "" {
		t.Fatalf("headers = %v", req.MCPServers[0].Headers)
	}
	if a, err := svc.ActorForToken(context.Background(), tok); err == nil && a.RunID != run.ID {
		t.Fatalf("token of run %s, want %s", a.RunID, run.ID)
	}
	b, _ := os.ReadFile(filepath.Join(home.RunsLogDir(), run.ID+".log"))
	if !strings.Contains(string(b), "The token of Granola expires at noon.") {
		t.Fatalf("run log = %s", b)
	}
	waitTerminal(t, svc, run.ID)
}

// TestConnectorLoginFailsBeforeLaunch proves a run that needs a connection
// with no login is a failed row with reason connector_login, and nothing
// starts.
func TestConnectorLoginFailsBeforeLaunch(t *testing.T) {
	drv := &captureDriver{}
	svc, _, _ := cliFixture(t, drv)
	svc.WithActions(loginResolver{})
	fakeBinaries(t, "claude")
	run := launchCLIJob(t, svc, func(o *runs.LaunchOpts) {
		o.Harness = "claude"
		o.JobConnectors = []string{"granola"}
	})
	if run.Status != runs.StatusFailed || run.EndReason != runs.EndConnectorLogin {
		t.Fatalf("run = %s %s", run.Status, run.EndReason)
	}
	drv.mu.Lock()
	n := len(drv.reqs)
	drv.mu.Unlock()
	if n != 0 {
		t.Fatal("the harness started")
	}
	b, _ := os.ReadFile(filepath.Join(home.RunsLogDir(), run.ID+".log"))
	if !strings.Contains(string(b), "Log in again to Granola") {
		t.Fatalf("run log = %s", b)
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
		WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "claude", Runtime: "cli",
		SessionID: "sess-1", Connectors: []string{"gh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, prev.ID, runs.StatusFailed, runs.EndLaunch, nil); err != nil {
		t.Fatal(err)
	}
	run, err := svc.Resume(ctx, prev.ID, runs.LaunchOpts{}, runs.ResumeOpts{})
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

// parkedCLIRun makes a parked CLI run with session sess-1 in its own
// working dir.
func parkedCLIRun(t *testing.T, svc *runs.Service) (*runs.Run, string) {
	t.Helper()
	fakeBinaries(t, "claude")
	ctx := context.Background()
	dir := t.TempDir()
	prev, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "demo", JobName: "demo", Kind: runs.KindJob, Harness: "claude", Runtime: "cli",
		SessionID: "sess-1", WorkingDir: dir, ConversationKey: "ck",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, prev.ID, runs.StatusExited, runs.EndKill, nil); err != nil {
		t.Fatal(err)
	}
	return prev, dir
}

// TestResumeKeepsWorkingDir pins the fix: a resume starts in the working dir
// of the parent, not the daemon cwd.
func TestResumeKeepsWorkingDir(t *testing.T) {
	drv := &captureDriver{}
	svc, _, _ := cliFixture(t, drv)
	prev, dir := parkedCLIRun(t, svc)
	run, err := svc.Resume(context.Background(), prev.ID, runs.LaunchOpts{}, runs.ResumeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	req := drv.last(t)
	if req.WorkingDir != dir || req.SessionID != "sess-1" {
		t.Fatalf("working dir %q session %q, want %q sess-1", req.WorkingDir, req.SessionID, dir)
	}
	got := waitTerminal(t, svc, run.ID)
	if got.WorkingDir != dir || got.ConversationKey != "ck" {
		t.Fatalf("row working dir %q conversation %q", got.WorkingDir, got.ConversationKey)
	}
}

// TestResumeRebuildsFromJob pins the base: model, effort, and env come from
// the job options; the session comes from the parent.
func TestResumeRebuildsFromJob(t *testing.T) {
	drv := &captureDriver{}
	svc, _, _ := cliFixture(t, drv)
	svc.WithActions(mcpResolver{})
	prev, _ := parkedCLIRun(t, svc)
	base := runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{Model: "opus"}, Effort: "high", Env: map[string]string{"JOB_ENV": "1"},
		JobConnectors: []string{"gh"},
	}
	base.WorkingDir = "/elsewhere"
	run, err := svc.Resume(context.Background(), prev.ID, base, runs.ResumeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	req := drv.last(t)
	if req.Model != "opus" || req.Effort != "high" || req.Env["JOB_ENV"] != "1" {
		t.Fatalf("model %q effort %q env %v", req.Model, req.Effort, req.Env["JOB_ENV"])
	}
	if len(req.MCPServers) != 1 || req.MCPServers[0].Name != "gh" {
		t.Fatalf("servers %+v", req.MCPServers)
	}
	if req.SessionID != "sess-1" || req.WorkingDir == "/elsewhere" {
		t.Fatalf("session %q working dir %q", req.SessionID, req.WorkingDir)
	}
	waitTerminal(t, svc, run.ID)
}

// TestCLIResumePromptExact pins the handoff: the first prompt of a request
// resume reaches the session byte for byte.
func TestCLIResumePromptExact(t *testing.T) {
	out := filepath.Join(t.TempDir(), "prompt.json")
	drv := &captureDriver{FakeDriver: agentwire.FakeDriver{
		Script: `read -r line; printf '%s' "$line" > ` + out + `; echo '{"type":"result","subtype":"success"}'`,
	}}
	svc, _, _ := cliFixture(t, drv)
	prev, _ := parkedCLIRun(t, svc)
	prompt := "[trebi] The person answered 1 request.\n\n```json\n{\n  \"text\": \"line 1\\nline `2` \\\"q\\\" <html> \"\n}\n```\n\nContinue the task with this answer."
	run, err := svc.Resume(context.Background(), prev.ID, runs.LaunchOpts{}, runs.ResumeOpts{Reason: runs.ReasonRequest, Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, svc, run.ID)
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Text != prompt {
		t.Fatalf("prompt\n got %q\nwant %q", frame.Text, prompt)
	}
	turns, err := svc.Turns(context.Background(), run.ID)
	if err != nil || len(turns) != 2 || turns[1].Reason != runs.ReasonRequest {
		t.Fatalf("turns %+v err %v", turns, err)
	}
}

// profileDriver plays one fake wire profile from testdata/fakewire: start.sh
// for a new session, resume.sh for a reopen. No resume.sh means the harness
// cannot resume.
type profileDriver struct {
	agentwire.FakeDriver
	dir  string
	mu   sync.Mutex
	reqs []agentwire.StartRequest
}

func (d *profileDriver) Start(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	return d.run(ctx, req, "start.sh")
}

func (d *profileDriver) Resume(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	return d.run(ctx, req, "resume.sh")
}

func (d *profileDriver) run(ctx context.Context, req agentwire.StartRequest, name string) (agentwire.Session, error) {
	d.mu.Lock()
	d.reqs = append(d.reqs, req)
	d.mu.Unlock()
	script, err := os.ReadFile(filepath.Join(d.dir, name))
	if os.IsNotExist(err) {
		return nil, agentwire.ErrResumeUnsupported
	}
	if err != nil {
		return nil, err
	}
	return agentwire.FakeDriver{Script: string(script)}.Start(ctx, req)
}

// TestHarnessResumeProfiles runs turn 1 and a reopen for each fake wire
// profile, and checks the run, its turns, and the run log.
func TestHarnessResumeProfiles(t *testing.T) {
	cases := []struct {
		profile    string
		resumeFrom string   // session id the reopen asks the harness for
		sessions   []string // session id of each turn
		runSession string
	}{
		{"claude-same-id", "sess-a", []string{"sess-a", "sess-a"}, "sess-a"},
		{"codex-adopt-id", "thr-1", []string{"thr-1", "thr-1"}, "thr-1"},
		{"fork-id", "sess-a", []string{"sess-a", "sess-b"}, "sess-b"},
		{"no-resume", "sess-a", []string{"sess-a", "sess-a"}, "sess-a"},
	}
	for _, c := range cases {
		t.Run(c.profile, func(t *testing.T) {
			drv := &profileDriver{dir: filepath.Join("testdata", "fakewire", c.profile)}
			svc, _, _ := cliFixture(t, drv)
			fakeBinaries(t, "claude")
			ctx := context.Background()
			first := launchCLIJob(t, svc)
			waitTerminal(t, svc, first.ID)

			run, err := svc.Resume(ctx, first.ID, runs.LaunchOpts{}, runs.ResumeOpts{Reason: runs.ReasonResume})
			if c.profile == "no-resume" {
				if !errors.Is(err, runs.ErrResumeUnsupported) {
					t.Fatalf("err %v, want ErrResumeUnsupported", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			done := waitTerminal(t, svc, first.ID)
			if run != nil && run.ID != first.ID {
				t.Fatalf("reopen made run %s, want %s", run.ID, first.ID)
			}
			if done.TurnCount != 2 || done.SessionID != c.runSession || done.Status != runs.StatusFailed {
				t.Fatalf("run turns %d session %q status %s", done.TurnCount, done.SessionID, done.Status)
			}
			drv.mu.Lock()
			reqs := slices.Clone(drv.reqs)
			drv.mu.Unlock()
			if len(reqs) != 2 || reqs[1].SessionID != c.resumeFrom {
				t.Fatalf("driver requests %d, resume session %q", len(reqs), reqs[len(reqs)-1].SessionID)
			}
			if reqs[1].WorkingDir == "" || reqs[1].WorkingDir != reqs[0].WorkingDir {
				t.Fatalf("resume dir %q, want %q", reqs[1].WorkingDir, reqs[0].WorkingDir)
			}
			turns, err := svc.Turns(ctx, first.ID)
			if err != nil || len(turns) != 2 {
				t.Fatalf("turns %+v err %v", turns, err)
			}
			for i, tr := range turns {
				if tr.SessionID != c.sessions[i] {
					t.Fatalf("turn %d session %q, want %q", tr.N, tr.SessionID, c.sessions[i])
				}
			}
			if turns[0].Tokens.Input != 100 {
				t.Fatalf("turn 1 tokens %+v", turns[0].Tokens)
			}
			all, err := svc.List(ctx, runs.ListOpts{Workspace: "ws"})
			if err != nil || len(all) != 1 {
				t.Fatalf("runs %d err %v", len(all), err)
			}
			if c.profile == "no-resume" {
				if turns[1].Status != runs.StatusFailed || turns[1].EndReason != runs.EndLaunch {
					t.Fatalf("turn 2 %s/%s, want failed/launch", turns[1].Status, turns[1].EndReason)
				}
				return
			}
			if turns[1].Tokens.Input != 50 || done.Tokens.Input != 150 {
				t.Fatalf("turn 2 tokens %+v run tokens %+v", turns[1].Tokens, done.Tokens)
			}
			log, err := os.ReadFile(filepath.Join(home.RunsLogDir(), first.ID+".log"))
			if err != nil {
				t.Fatal(err)
			}
			one, two := strings.Index(string(log), "frame turn 1"), strings.Index(string(log), "frame turn 2")
			if one < 0 || two < one {
				t.Fatalf("run log does not hold both turns in order:\n%s", log)
			}
			if c.profile != "fork-id" {
				return
			}
			// The next reopen resumes the forked id.
			if _, err := svc.Resume(ctx, first.ID, runs.LaunchOpts{}, runs.ResumeOpts{Reason: runs.ReasonResume}); err != nil {
				t.Fatal(err)
			}
			waitTerminal(t, svc, first.ID)
			drv.mu.Lock()
			last := drv.reqs[len(drv.reqs)-1]
			drv.mu.Unlock()
			if last.SessionID != "sess-b" {
				t.Fatalf("turn 3 resumed %q, want sess-b", last.SessionID)
			}
		})
	}
}

// waitLog waits until the run log holds text.
func waitLog(t *testing.T, runID, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(filepath.Join(home.RunsLogDir(), runID+".log")); strings.Contains(string(data), text) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run log never held %q", text)
}

// TestWithRequestsMergesOneTurn pins the session feed: a request is one turn
// at its first event with its latest state, and its answer prompt has origin
// request.
func TestWithRequestsMergesOneTurn(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := cliFixture(t, agentwire.FakeDriver{
		Script: `read -r line; echo '{"type":"init","session_id":"s1"}'; echo '{"type":"assistant","text":"asked"}'; echo '{"type":"result","subtype":"success"}'`,
	})
	run := launchCLIJob(t, svc)
	waitTerminal(t, svc, run.ID)
	svc.AddEvent(ctx, run.ID, runs.EventRequest, "trebi", tmux.TurnRequest{ID: "R1", Status: "pending", Title: "Ship it", Forms: 1})
	svc.AddEvent(ctx, run.ID, runs.EventRequest, "trebi", tmux.TurnRequest{ID: "R1", Status: "pending", Title: "Ship it", Forms: 2})
	svc.AddEvent(ctx, run.ID, runs.EventRequest, "trebi", tmux.TurnRequest{ID: "R1", Status: "answered", Title: "Ship it", Forms: 2})
	next, err := svc.Resume(ctx, run.ID, runs.LaunchOpts{}, runs.ResumeOpts{Prompt: "answers", Reason: runs.ReasonRequest})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, svc, next.ID)
	turns, err := svc.CLITurns(ctx, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	turns, err = svc.WithRequests(ctx, next.ID, turns)
	if err != nil {
		t.Fatal(err)
	}
	var reqs []tmux.TurnRequest
	var origins []string
	for _, turn := range turns {
		switch turn.Role {
		case runs.EventRequest:
			reqs = append(reqs, *turn.Request)
		case "user":
			origins = append(origins, turn.Origin)
		}
	}
	if len(reqs) != 1 || reqs[0].Status != "answered" || reqs[0].Forms != 2 {
		t.Fatalf("request turns %+v", reqs)
	}
	if !slices.Equal(origins, []string{runs.OriginLaunch, runs.OriginRequest}) {
		t.Fatalf("origins %v: %+v", origins, turns)
	}
	if i := slices.IndexFunc(turns, func(t tmux.Turn) bool { return t.Role == runs.EventRequest }); i == 0 || i == len(turns)-1 {
		t.Fatalf("request turn at %d is not between the turns: %+v", i, turns)
	}
}

// waitRunPID waits until the run row has the child pid.
func waitRunPID(t *testing.T, svc *runs.Service, runID string) *runs.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := svc.Get(context.Background(), runID)
		if err == nil && got.RuntimePID != 0 {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("pid not recorded")
	return nil
}

// waitSessionState waits until the run session is in state.
func waitSessionState(t *testing.T, svc *runs.Service, runID string, state runs.SessionState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if svc.SessionState(runID) == state {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session state = %q, want %q", svc.SessionState(runID), state)
}
