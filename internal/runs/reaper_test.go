package runs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runtime/cli"
	"github.com/trebi-ai/trebi/internal/store"
)

// stubCLISession is a minimal cli.Session for reaper tests.
type stubCLISession struct {
	events chan cli.Event
	done   chan struct{}
}

func newStubCLISession() *stubCLISession {
	return &stubCLISession{events: make(chan cli.Event), done: make(chan struct{})}
}

func (s *stubCLISession) Provider() string                         { return "stub" }
func (s *stubCLISession) ID() string                               { return "stub" }
func (s *stubCLISession) PID() int                                 { return 1 }
func (s *stubCLISession) Prompt(context.Context, cli.Prompt) error { return nil }
func (s *stubCLISession) Interrupt(context.Context) error          { return nil }
func (s *stubCLISession) Events() <-chan cli.Event                 { return s.events }
func (s *stubCLISession) Close(context.Context) error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return nil
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
	cr := &cliRun{sess: newStubCLISession(), tracker: cli.NewTracker()}
	cr.tracker.Observe(cli.Event{Type: cli.EventResult, Result: &cli.Result{Subtype: "success"}})
	svc.registerCLIRun(run.ID, cr)
	return svc, run, cr
}

// TestReaperCLIIdleTimeout pins the event-clock idle timeout (G.5): a
// genuinely idle CLI session times out, no pane involved.
func TestReaperCLIIdleTimeout(t *testing.T) {
	svc, run, _ := cliReaperFixture(t, CreateOpts{WorkspaceID: "ws", Job: "j", IdleTimeoutMS: 1000})
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
		WorkspaceID: "ws", Job: "j", MaxDurationMS: 1000, KeepAliveMS: 60_000,
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

func TestCountSentinels(t *testing.T) {
	t.Parallel()
	id := "01TESTRUNID00000000000000001"
	sent := CompletionSentinel(id)
	screen := "working\n" + sent + "\n" +
		"Fix MEMORY.md, then print the completion line again: `" + sent + "`\n" +
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
		WorkspaceID: "ws", Job: "j", Harness: "fake",
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
			WorkspaceID: "ws", Job: "demo", Harness: "fake",
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
