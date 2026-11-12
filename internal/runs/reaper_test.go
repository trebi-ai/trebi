package runs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/memory"
	"github.com/trebi-ai/trebi/internal/pricing"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/visor"
)

// stubHost is a session host that records each send. refuse makes a send
// with images fail as unsupported.
type stubHost struct {
	visor.Host  // the terminal methods are not used
	mu          sync.Mutex
	refuse      bool
	inputs      []agentvisor.Input
	onInterrupt func()
}

func (h *stubHost) Start(context.Context, agentvisor.StartSpec) (agentvisor.Session, error) {
	return agentvisor.Session{}, agentvisor.ErrUnsupported
}

func (h *stubHost) Session(_ context.Context, id string) (agentvisor.Session, error) {
	return agentvisor.Session{ID: id, State: agentvisor.StateIdle}, nil
}

func (h *stubHost) Send(_ context.Context, _ string, in agentvisor.Input) (agentvisor.Delivery, error) {
	if h.refuse && len(in.Attachments) > 0 {
		return "", agentvisor.ErrUnsupported
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inputs = append(h.inputs, in)
	return agentvisor.DeliveryPrompted, nil
}

func (h *stubHost) Interrupt(context.Context, string) error {
	if h.onInterrupt != nil {
		h.onInterrupt()
	}
	return nil
}

func (h *stubHost) Answer(context.Context, string, string, agentvisor.Answer) error { return nil }

func (h *stubHost) Stop(context.Context, string) error { return nil }

func (h *stubHost) Events(context.Context, agentvisor.Filter, uint64) (*agentvisor.EventStream, error) {
	return nil, agentvisor.ErrUnsupported
}

// cliReaperFixture builds a service + idle CLI run with a tracker already at
// a terminal result.
func cliReaperFixture(t *testing.T, opts CreateOpts) (*Service, *Run, *cliRun) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := New(st, nil, nil)
	opts.Harness = "claude"
	opts.Runtime = "cli"
	run, err := svc.Create(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	cr := &cliRun{sess: newStubCLISession(), tracker: agentwire.NewTracker()}
	cr.tracker.Observe(agentwire.Event{Type: agentwire.EventResult, Result: &agentwire.Result{Subtype: "success"}})
	svc.registerCLIRun(run.ID, cr)
	return svc, run, cr
}

// TestReaperCLIIdleTimeout pins the event-clock idle timeout (G.5): a
// genuinely idle CLI session times out, no pane involved.
func TestReaperCLIIdleTimeout(t *testing.T) {
	svc, run, _ := cliReaperFixture(t, CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", IdleTimeoutMS: 1000})
	r := NewReaper(svc)
	now := time.Now()
	r.checkOne(context.Background(), run, now, nil, false)                    // opens the idle clock
	r.checkOne(context.Background(), run, now.Add(2*time.Second), nil, false) // over the window
	got, err := svc.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusTimedOut || got.EndReason != EndIdleTimeout {
		t.Fatalf("run: status=%s end=%s", got.Status, got.EndReason)
	}
}

// TestReaperCLIKeepAliveSuppressesMaxDuration pins the TUI parity fix: a
// keep-alive window outlives max duration, then closes with keep_alive.
func TestReaperCLIKeepAliveSuppressesMaxDuration(t *testing.T) {
	svc, run, cr := cliReaperFixture(t, CreateOpts{
		WorkspaceID: "ws", JobID: "j", JobName: "j", MaxDurationMS: 1000, KeepAliveMS: 60_000,
	})
	r := NewReaper(svc)
	cr.markKeepAlive(time.Now())
	r.checkOne(context.Background(), run, time.Now().Add(2*time.Second), nil, false)
	got, err := svc.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning {
		t.Fatalf("keep-alive run ended early: status=%s end=%s", got.Status, got.EndReason)
	}
	// Past the keep-alive window (and max duration): close with keep_alive.
	r.checkOne(context.Background(), run, time.Now().Add(2*time.Minute), nil, false)
	got, err = svc.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusComplete || got.EndReason != EndKeepAlive {
		t.Fatalf("run: status=%s end=%s", got.Status, got.EndReason)
	}
}

func TestDetectCompletion(t *testing.T) {
	t.Parallel()
	id := "01TESTRUNID00000000000000001"
	sent := CompletionSentinel(id)
	if !DetectCompletion("done\n"+sent+"\n", id) {
		t.Fatal("expected detect")
	}
	// echo of instruction should not count
	echo := "When done, output exactly this: " + sent
	if DetectCompletion(echo, id) {
		t.Fatal("echo should not count")
	}
}

func TestReportedFailure(t *testing.T) {
	t.Parallel()
	id := "01TESTRUNID00000000000000001"
	sent := FailureSentinel(id)
	cases := []struct {
		name, text, key, want string
	}{
		{"line", "No network.\n" + sent + ": LunchMoney is not reachable\n", id, "LunchMoney is not reachable"},
		{"bare prefix, any key", "x\nTREBI_RUN_FAILED: no login", "", "no login"},
		{"bullet and backticks", "• `" + sent + ": no disk`", id, "no disk"},
		{"pane border", "│ " + sent + ": no disk │", id, "no disk"},
		{"last wins", sent + ": first\n" + sent + ": second", id, "second"},
		{"prompt echo", "in one line: " + sent + ": <reason>", id, ""},
		{"wrapped echo", "line and the reason in one line:\n" + sent + ": <reason>", id, ""},
		{"empty reason", sent + ":", id, ""},
		{"other turn", FailureSentinel(id+"_2") + ": no disk", id, ""},
		{"mid sentence", "I will print " + sent + ": x if it fails", id, ""},
	}
	for _, tc := range cases {
		got, ok := ReportedFailure(tc.text, tc.key)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: got %q %v, want %q", tc.name, got, ok, tc.want)
		}
	}
}

func TestCountSentinels(t *testing.T) {
	t.Parallel()
	id := "01TESTRUNID00000000000000001"
	sent := CompletionSentinel(id)
	screen := "working\n" + sent + "\n" +
		"Fix MEMORY.md. Trebi checks the file again when you finish. Then print the completion line again: `" + sent + "`\n" +
		"  " + sent + "  \n"
	if n := countSentinels(screen, id); n != 2 {
		t.Fatalf("countSentinels = %d, want 2 (exact lines only)", n)
	}
	if n := countSentinels(screen, "other"); n != 0 {
		t.Fatalf("countSentinels other run = %d, want 0", n)
	}
}

func TestReaperMaxDuration(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil, nil)
	ctx := context.Background()
	run, err := svc.Create(ctx, CreateOpts{
		WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "fake",
		MaxDurationMS: 1, // 1ms
	})
	if err != nil {
		t.Fatal(err)
	}
	// backdate started_at
	_, _ = st.DB().Exec(`UPDATE runs SET started_at=? WHERE id=?`,
		time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), run.ID)

	r := NewReaper(svc)
	r.interval = time.Hour // we call Sweep manually
	r.Sweep(ctx)

	got, err := svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusTimedOut {
		t.Fatalf("status=%s want timed_out", got.Status)
	}
	if got.EndReason != EndMaxDuration {
		t.Fatalf("end=%s want max_duration", got.EndReason)
	}
}

type memoryStub struct{ mem *MemoryLaunch }

func (m memoryStub) MemoryLaunchFor(context.Context, *Run) *MemoryLaunch { return m.mem }

// TestReaperMaxDurationSkipsMemory pins that a run that did not complete is
// not checked: run_failed covers it, and the agent never reached its Log step.
func TestReaperMaxDurationSkipsMemory(t *testing.T) {
	svc, st, run, _ := reaperMemoryFixture(t, CreateOpts{MaxDurationMS: 1})
	ctx := context.Background()
	_, _ = st.DB().Exec(`UPDATE runs SET started_at=? WHERE id=?`,
		time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), run.ID)
	r := NewReaper(svc)
	r.interval = time.Hour
	r.Sweep(ctx)

	got, err := svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EndReason != EndMaxDuration {
		t.Fatalf("end=%s want max_duration", got.EndReason)
	}
	tl, err := svc.Timeline(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range tl.Events {
		if strings.HasPrefix(ev.Kind, "memory.") {
			t.Fatalf("unexpected run event %s", ev.Kind)
		}
	}
	if bus := memoryBusRows(t, st, run.ID); len(bus) != 0 {
		t.Fatalf("memory bus rows = %v, want none", bus)
	}
}

// TestReaperSentinelMemoryValid pins the close signal: a run that passes the
// gate at its sentinel writes memory.valid.
func TestReaperSentinelMemoryValid(t *testing.T) {
	svc, st, run, p := reaperMemoryFixture(t, CreateOpts{})
	doc := &memory.Doc{Context: "Client sends CSV.", Log: []memory.Entry{{At: "2026-09-14 09:00", Text: "ran"}}}
	if err := os.WriteFile(p, doc.Marshal(), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewReaper(svc)
	ctx := context.Background()
	r.gateCompletion(ctx, run, CompletionSentinel(run.sentinelKey())+"\n", time.Now())

	got, err := svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusComplete {
		t.Fatalf("status=%s want complete", got.Status)
	}
	if bus := memoryBusRows(t, st, run.ID); bus["memory.valid"] != 1 || bus["memory.invalid"] != 0 {
		t.Fatalf("memory bus rows = %v", bus)
	}
}

func TestFakeHarnessSentinelViaCreateAndLaunch(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	_ = os.Setenv("TREBI_HOME", dir)
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil, nil)
	ctx := context.Background()
	run, _, err := svc.CreateAndLaunch(ctx, LaunchOpts{
		CreateOpts: CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake",
			PromptPreview: "hi",
		},
		Prompt:      "hi",
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusComplete {
		t.Fatalf("status=%s", run.Status)
	}
	if run.EndReason != EndSentinel {
		t.Fatalf("end=%s", run.EndReason)
	}
}

func TestPendingPromptRoundtrip(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil, nil)
	svc.markLaunching("r1", true)
	if !svc.IsLaunching("r1") {
		t.Fatal("launching")
	}
	svc.markLaunching("r1", false)
	if svc.IsLaunching("r1") {
		t.Fatal("cleared")
	}
	svc.stashPendingPrompt("r1", pendingPrompt{session: "s", prompt: "hi", harness: "claude"})
	p, ok := svc.takePendingPrompt("r1")
	if !ok || p.prompt != "hi" || p.session != "s" {
		t.Fatalf("%v %v", ok, p)
	}
	if _, ok := svc.takePendingPrompt("r1"); ok {
		t.Fatal("second take")
	}
}

func TestCapturePaneLogStampsCaptures(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	if err := home.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := NewReaper(New(st, nil, nil))
	run := &Run{ID: "01CAPTURE000000000000000001"}
	r.capturePaneLog(run, "first line")
	r.capturePaneLog(run, "first line\nsecond line")

	data, err := os.ReadFile(filepath.Join(home.RunsLogDir(), run.ID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if n := strings.Count(got, "msg=pane.capture"); n != 2 {
		t.Fatalf("capture headers = %d in %q", n, got)
	}
	if !strings.Contains(got, "first line") || !strings.Contains(got, "second line") {
		t.Fatalf("log %q", got)
	}
	if n := strings.Count(got, "lines=1"); n != 2 {
		t.Fatalf("each delta is one line: %q", got)
	}
	// An unchanged pane appends nothing, so no empty capture is stamped.
	r.capturePaneLog(run, "first line\nsecond line")
	again, err := os.ReadFile(filepath.Join(home.RunsLogDir(), run.ID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != got {
		t.Fatalf("unchanged pane should not append:\n%s", again)
	}
}

func TestPaneDelta(t *testing.T) {
	t.Parallel()
	if paneDelta("a\nb", "a\nb") != "" {
		t.Fatal("identical")
	}
	got := paneDelta("a\nb\nc", "b\nc\nd")
	if got != "d" {
		t.Fatalf("scroll got %q", got)
	}
	if paneDelta("", "hello") != "hello" {
		t.Fatal("first")
	}
	spinner := paneDelta(
		"working\nesc to interrupt\n❯ ",
		"working\nesc to interrupt\n❯ ",
	)
	if spinner != "" {
		t.Fatalf("spinner %q", spinner)
	}
	redraw := paneDelta("aaaa\nbbbb\ncccc\ndddd", "aaaa\nXXXX\ncccc\ndddd")
	if redraw != "" {
		t.Fatalf("similar redraw should skip, got %q", redraw)
	}
}

func TestPaneCaptureHeader(t *testing.T) {
	t.Parallel()
	line := paneCaptureHeader("one\ntwo\nthree")
	for _, want := range []string{"time=", "level=INFO", "msg=pane.capture", "source=reaper", "lines=3"} {
		if !strings.Contains(line, want) {
			t.Fatalf("header %q missing %q", line, want)
		}
	}
	if got := paneCaptureHeader("one\ntwo\n"); !strings.Contains(got, "lines=2") {
		t.Fatalf("trailing newline should not count as a line: %q", got)
	}
	if got := paneCaptureHeader("  \n "); !strings.Contains(got, "lines=0") {
		t.Fatalf("blank capture %q", got)
	}
}

// openGate reports an open request for every run.
type openGate struct{}

func (openGate) Name() string                                                       { return GateRequest }
func (openGate) Open(context.Context, string) (bool, bool, error)                   { return true, false, nil }
func (openGate) RunEnded(context.Context, string, Status)                           {}
func (openGate) Claim(context.Context, string) ([]ParkDelivery, error)              { return nil, nil }
func (openGate) Settle(context.Context, string, []ParkDelivery, ResumeResult) error { return nil }
func (openGate) Stale(context.Context) ([]string, error)                            { return nil, nil }
func (openGate) PromptHint(LaunchOpts) string                                       { return "" }

// TestIdleWithOpenRequestParks pins the pane rule: an idle pane with an open
// request parks at once, without the idle timeout.
func TestIdleWithOpenRequestParks(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := New(st, nil, nil)
	svc.SetParker(NewParker(svc, nil, openGate{}))
	run, err := svc.Create(context.Background(), CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", IdleTimeoutMS: 3_600_000})
	if err != nil {
		t.Fatal(err)
	}
	r := NewReaper(svc)
	if !r.checkIdle(context.Background(), run, "> ", time.Now()) {
		t.Fatal("checkIdle did not stop")
	}
	got, err := svc.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusParked || got.EndReason != EndRequest {
		t.Fatalf("run: status=%s end=%s", got.Status, got.EndReason)
	}
}

// TestReaperPerTurnLimit: max duration counts from the start of the turn, so
// a reopened run is not ended by the age of its first turn.
func TestReaperPerTurnLimit(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil, nil)
	ctx := context.Background()
	opts := CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", Harness: "fake", MaxDurationMS: time.Hour.Milliseconds()}
	run, err := svc.Create(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`UPDATE runs SET started_at=?, turn_started_at=? WHERE id=?`, old, old, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, StatusComplete, EndResult, nil); err != nil {
		t.Fatal(err)
	}
	opts.Reason = ReasonTrigger
	if _, err := svc.Reopen(ctx, run.ID, opts); err != nil {
		t.Fatal(err)
	}
	r := NewReaper(svc)
	r.interval = time.Hour
	r.Sweep(ctx)
	got, err := svc.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EndReason == EndMaxDuration {
		t.Fatalf("turn 2 ended by the age of turn 1: %s %s", got.Status, got.EndReason)
	}
}

// TestKeepAliveTokens: when the reaper closes a keep-alive turn, the turn
// keeps the usage of every result it had, not only the last one.
func TestKeepAliveTokens(t *testing.T) {
	svc, run, cr := cliReaperFixture(t, CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", KeepAliveMS: 60_000})
	cr.turn = run.TurnCount
	cr.addUsage(&pricing.TokenUsage{Input: 100, Output: 10, Model: "default"})
	cr.addUsage(&pricing.TokenUsage{Input: 50, Output: 5, Model: "default"})
	cr.markKeepAlive(time.Now())
	r := NewReaper(svc)
	r.checkOne(context.Background(), run, time.Now().Add(2*time.Minute), nil, false)
	got, err := svc.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EndReason != EndKeepAlive || got.Tokens.Input != 150 || got.Tokens.Output != 15 {
		t.Fatalf("run end %s tokens %+v", got.EndReason, got.Tokens)
	}
	turns, err := svc.Turns(context.Background(), run.ID)
	if err != nil || len(turns) != 1 || turns[0].Tokens.Input != 150 {
		t.Fatalf("turns %+v err %v", turns, err)
	}
}

// TestTeardownKeepsTurnTokens: the turn tokens survive a stop that
// unregisters the CLI run before the terminal write.
func TestTeardownKeepsTurnTokens(t *testing.T) {
	cases := []struct {
		name string
		end  EndReason
		stop func(*Service, *Run, *cliRun)
	}{
		{"keep-alive", EndKeepAlive, func(svc *Service, run *Run, cr *cliRun) {
			cr.markKeepAlive(time.Now())
			NewReaper(svc).checkOne(context.Background(), run, time.Now().Add(2*time.Minute), nil, false)
		}},
		{"kill", EndKill, func(svc *Service, run *Run, _ *cliRun) {
			if err := svc.Kill(context.Background(), run.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"replace", EndReplace, func(svc *Service, run *Run, _ *cliRun) {
			svc.overlapReplace(context.Background(), []*Run{run})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, run, cr := cliReaperFixture(t, CreateOpts{WorkspaceID: "ws", JobID: "j", JobName: "j", KeepAliveMS: 60_000})
			h := &stubHost{}
			cr.sess = stubSession(h)
			h.onInterrupt = func() { svc.unregisterCLIRun(run.ID, cr) }
			cr.turn = run.TurnCount
			cr.addUsage(&pricing.TokenUsage{Input: 100, Output: 10, Model: "default"})
			cr.addUsage(&pricing.TokenUsage{Input: 50, Output: 5, Model: "default"})
			tc.stop(svc, run, cr)
			got, err := svc.Get(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.EndReason != tc.end || got.Tokens.Input != 150 || got.Tokens.Output != 15 {
				t.Fatalf("run end %s tokens %+v", got.EndReason, got.Tokens)
			}
			turns, err := svc.Turns(context.Background(), run.ID)
			if err != nil || len(turns) != 1 || turns[0].Tokens.Input != 150 {
				t.Fatalf("turns %+v err %v", turns, err)
			}
		})
	}
}

// reaperMemoryFixture builds a run whose MEMORY.md is the skeleton it
// launched with, so the gate finds it unchanged.
func reaperMemoryFixture(t *testing.T, opts CreateOpts) (*Service, *store.Store, *Run, string) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	_ = home.EnsureDirs()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	root := t.TempDir()
	p := filepath.Join(root, "demo", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, memory.Skeleton(), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil, files.New(files.DefaultMaxBytes))
	svc.SetMemoryResolver(memoryStub{&MemoryLaunch{
		Root: root, Rel: filepath.Join("demo", "MEMORY.md"),
		Opt: memory.Options{MaxLogs: 100, RequireUpdate: true, InjectLogs: 20},
	}})
	opts.WorkspaceID, opts.JobID, opts.JobName, opts.Harness = "ws", "demo", "demo", "fake"
	opts.MemoryHash = memory.Hash(memory.Skeleton())
	run, err := svc.Create(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, run, p
}

func memoryBusRows(t *testing.T, st *store.Store, runID string) map[string]int {
	t.Helper()
	rows, err := st.DB().Query(`SELECT topic FROM event_log WHERE topic LIKE 'memory.%' AND entity_id=?`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var topic string
		if err := rows.Scan(&topic); err != nil {
			t.Fatal(err)
		}
		out[topic]++
	}
	return out
}

// stubSession is a visor session on h with no events.
func stubSession(h *stubHost) *visor.Session { return visor.Quiet(h, "stub") }

func newStubCLISession() *visor.Session { return stubSession(&stubHost{}) }
