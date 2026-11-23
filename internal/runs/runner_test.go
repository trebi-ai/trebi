package runs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/folderenv"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/uploads"
)

func TestFailBeforeLaunch(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(dir, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()

	run, err := svc.FailBeforeLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "claude", Reason: runs.ReasonSchedule,
			PromptPreview: "do it",
		},
		Prompt: "do it",
	}, runs.EndAuth, map[string]string{
		"reason":  "auth_missing",
		"message": "schedule: harness claude blocked (auth_missing)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != runs.StatusFailed || run.EndReason != runs.EndAuth {
		t.Fatalf("status=%s end=%s want failed/auth", run.Status, run.EndReason)
	}
	if run.PaneSession != "" {
		t.Fatalf("pane %q", run.PaneSession)
	}
	tl, err := svc.Timeline(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range tl.Events {
		if ev.Kind == "error" && strings.Contains(string(ev.Detail), "auth_missing") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("timeline missing error: %+v", tl.Events)
	}
	logb, err := os.ReadFile(filepath.Join(home.RunsLogDir(), run.ID+".log"))
	if err != nil {
		t.Fatalf("run log: %v", err)
	}
	if !strings.Contains(string(logb), "auth_missing") {
		t.Fatalf("run log %q", logb)
	}
}

func TestFakeHarnessSentinel(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()

	run, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws",
			JobID:       "demo", JobName: "demo",
			Harness:    "fake",
			Reason:     runs.ReasonManual,
			WorkingDir: t.TempDir(),
		},
		Prompt:      "do it",
		FakeCommand: []string{"sh", "-c", `echo working; echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != runs.StatusComplete || run.EndReason != runs.EndSentinel {
		t.Fatalf("%+v", run)
	}
	tl, err := svc.Timeline(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Segments) < 1 {
		t.Fatal("no segments")
	}
}

func TestFakeHarnessGetsRunToken(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	dir := t.TempDir()
	if _, _, err := svc.CreateAndLaunch(context.Background(), runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonManual,
			WorkingDir: dir, ActorToken: "tok-1",
		},
		Prompt:      "do it",
		FakeCommand: []string{"sh", "-c", `printf %s "$TREBI_RUN_TOKEN" > token.txt; echo "$TREBI_RUN_COMPLETE"`},
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "token.txt")); string(got) != "tok-1" {
		t.Fatalf("token %q", got)
	}
}

func TestFakeHarnessFailure(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	run, _, err := svc.CreateAndLaunch(context.Background(), runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonManual,
			WorkingDir: t.TempDir(),
		},
		FakeCommand: []string{"sh", "-c", "echo no sentinel; exit 1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != runs.StatusFailed {
		t.Fatalf("%+v", run)
	}
}

// TestSendMessageRejectsSentinel pins that a caller cannot hand a run its own
// completion sentinel. Doing so ends the run as complete with the work undone,
// which is how a stalled run gets reported as a success.
func TestSendMessageRejectsSentinel(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	ctx := context.Background()

	for _, text := range []string{
		"Finish now and output exactly " + runs.CompletionSentinel("01ABC"),
		"Please output the required TREBI_RUN_COMPLETE sentinel now.",
	} {
		_, err := svc.SendMessage(ctx, "01ABC", text)
		if !errors.Is(err, runs.ErrSentinelInMessage) {
			t.Errorf("SendMessage(%q) err = %v, want ErrSentinelInMessage", text, err)
		}
	}
	// An ordinary message gets past the text guard and fails on run state.
	_, err = svc.SendMessage(ctx, "01ABC", "Check the log and fix the blocker.")
	if errors.Is(err, runs.ErrSentinelInMessage) {
		t.Errorf("ordinary message rejected as sentinel: %v", err)
	}
}

// TestSendMessageStates pins how a message reaches a run that does not run:
// it resumes the session with reason message, whatever ended the run, and
// it never starts a fresh run.
func TestSendMessageStates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		session string
		status  runs.Status
		end     runs.EndReason
		waiting bool
		want    runs.Status // status after the turn end
		err     error
	}{
		{name: "parked with an open request", session: "s1", status: runs.StatusComplete, end: runs.EndResult, waiting: true, want: runs.StatusParked},
		{name: "complete on result", session: "s1", status: runs.StatusComplete, end: runs.EndResult, want: runs.StatusComplete},
		{name: "failed", session: "s1", status: runs.StatusFailed, end: runs.EndLaunch, want: runs.StatusFailed},
		{name: "no session", status: runs.StatusComplete, end: runs.EndResult, want: runs.StatusComplete, err: runs.ErrNoSession},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, _, ctx := openTurnSvc(t)
			gate := &readyGate{name: runs.GateRequest, waiting: tc.waiting}
			rs := &resumeStub{}
			svc.SetParker(runs.NewParker(svc, rs, gate))
			run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Harness: "claude", SessionID: tc.session})
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.Terminate(ctx, run.ID, tc.status, tc.end, nil); err != nil {
				t.Fatal(err)
			}
			if got, _ := svc.Get(ctx, run.ID); got.Status != tc.want {
				t.Fatalf("status before the message = %s, want %s", got.Status, tc.want)
			}
			res, err := svc.SendMessage(ctx, run.ID, "Also check the totals.")
			if tc.err != nil {
				if !errors.Is(err, tc.err) || len(rs.calls) != 0 || len(rs.fresh) != 0 {
					t.Fatalf("err = %v, resumes %d, fresh %d", err, len(rs.calls), len(rs.fresh))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Delivery != runs.DeliveryResumed || res.RunID != run.ID || res.Turn != 2 {
				t.Fatalf("result %+v", res)
			}
			if len(rs.calls) != 1 || rs.calls[0].Reason != runs.ReasonMessage || rs.calls[0].Prompt != "Also check the totals." {
				t.Fatalf("resumes %+v", rs.calls)
			}
			if len(gate.settled) != 0 {
				t.Fatalf("a waiting request was settled: %+v", gate.settled)
			}
		})
	}
}

// TestMessageResumeCarriesAttachments: a message with an image resumes a
// parked run, and the resume carries the image.
func TestMessageResumeCarriesAttachments(t *testing.T) {
	t.Parallel()
	svc, _, ctx := openTurnSvc(t)
	rs := &resumeStub{}
	svc.SetParker(runs.NewParker(svc, rs, &readyGate{name: runs.GateRequest, waiting: true}))
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Harness: "claude", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	img := uploads.Attachment{ID: "upl_1", MIME: "image/png", Path: "/h/upl_1.png"}
	if _, err := svc.SendMessage(ctx, run.ID, "What is in this image?", img); err != nil {
		t.Fatal(err)
	}
	if len(rs.calls) != 1 || len(rs.calls[0].Attachments) != 1 || rs.calls[0].Attachments[0] != img {
		t.Fatalf("resumes %+v", rs.calls)
	}
}

// TestMessageThenAnswer pins the order on a parked run: a message resumes it
// at once with reason message and the request stays open. The answer later
// resumes it with reason request.
func TestMessageThenAnswer(t *testing.T) {
	t.Parallel()
	svc, _, ctx := openTurnSvc(t)
	gate := &readyGate{name: runs.GateRequest, waiting: true}
	rs := &resumeStub{}
	p := runs.NewParker(svc, rs, gate)
	svc.SetParker(p)
	run, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws1", JobID: "demo", JobName: "demo", Harness: "claude", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Terminate(ctx, run.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendMessage(ctx, run.ID, "One more thing."); err != nil {
		t.Fatal(err)
	}
	if len(rs.calls) != 1 || rs.calls[0].Reason != runs.ReasonMessage {
		t.Fatalf("message resume %+v", rs.calls)
	}
	if err := p.Wake(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if len(rs.calls) != 1 {
		t.Fatalf("a wake while the request waits resumed again: %+v", rs.calls)
	}
	if got, _ := svc.Get(ctx, run.ID); got.Status != runs.StatusParked {
		t.Fatalf("status %s, want parked", got.Status)
	}
	gate.waiting = false
	gate.d = []runs.ParkDelivery{{Gate: runs.GateRequest, Prompt: "answers", At: time.Now(), Reason: runs.ReasonRequest}}
	if err := p.Wake(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if len(rs.calls) != 2 || rs.calls[1].Reason != runs.ReasonRequest || rs.calls[1].Prompt != "answers" {
		t.Fatalf("answer resume %+v", rs.calls)
	}
}

// A run that leaves no log answers `run logs` with not_found forever, so the
// agent cannot see what its own run did.
func TestFakeHarnessWritesRunLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TREBI_HOME", home)
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, _, err := runs.New(st, nil, nil).CreateAndLaunch(context.Background(), runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonManual,
			WorkingDir: t.TempDir(),
		},
		FakeCommand: []string{"sh", "-c", "echo hello-from-the-pane"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "logs", "runs", run.ID+".log"))
	if err != nil {
		t.Fatalf("run log: %v", err)
	}
	if !strings.Contains(string(data), "hello-from-the-pane") {
		t.Fatalf("log = %q", data)
	}
}

// Codex mints its own rollout id, so its transcript is found by the directory
// the run launched in. The run row is the only place that survives to hold it.
func TestLaunchPersistsWorkingDir(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := runs.New(st, nil, nil)
	dir := t.TempDir()
	run, _, err := svc.CreateAndLaunch(context.Background(), runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonManual,
			WorkingDir: dir,
		},
		FakeCommand: []string{"sh", "-c", "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.WorkingDir != dir {
		t.Fatalf("launch working dir = %q, want %q", run.WorkingDir, dir)
	}
	got, err := svc.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkingDir != dir {
		t.Fatalf("reloaded working dir = %q, want %q", got.WorkingDir, dir)
	}
}

// stubComposer records the scope and opts the runner hands it.
type stubComposer struct {
	calls int
	scope runs.WakeScope
	opts  runs.WakeOpts
	text  string
	err   error
}

func (c *stubComposer) ComposeWakePrompt(_ context.Context, scope runs.WakeScope, opts runs.WakeOpts) (string, error) {
	c.calls++
	c.scope = scope
	c.opts = opts
	return c.text, c.err
}

func TestWakeComposeShapesPrompt(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		kind string
	}{
		{"agent", runs.KindAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			subSt, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer subSt.Close()
			c := &stubComposer{text: "# Instructions\nCOMPOSED wake"}
			svc := runs.New(subSt, c, nil)
			run, _, err := svc.CreateAndLaunch(ctx, wakeLaunchOpts(t, tc.kind, "cmo",
				&runs.WakeOpts{Body: "raw body", ExtraPrompt: "extra", Message: "msg"}))
			if err != nil {
				t.Fatal(err)
			}
			if c.calls != 1 {
				t.Fatalf("composer calls = %d", c.calls)
			}
			if c.scope.WorkspaceID != "ws" || c.scope.Kind != tc.kind || c.scope.Name != "cmo" {
				t.Fatalf("scope = %+v", c.scope)
			}
			if c.opts.Body != "raw body" || c.opts.ExtraPrompt != "extra" || c.opts.Message != "msg" {
				t.Fatalf("opts = %+v", c.opts)
			}
			if run.PromptPreview != "# Instructions\nCOMPOSED wake" {
				t.Fatalf("preview = %q", run.PromptPreview)
			}
		})
	}
}

func TestWakeFallbacks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name     string
		composer *stubComposer
		kind     string
		prompt   string
		want     string
	}{
		{name: "nil composer", kind: runs.KindAgent, want: "body\nextra"},
		{name: "composer error", composer: &stubComposer{err: errors.New("boom")}, kind: runs.KindAgent, want: "body\nextra"},
		{name: "composer empty", composer: &stubComposer{text: "  "}, kind: runs.KindAgent, want: "body\nextra"},
		{name: "job kind skips composer", composer: &stubComposer{text: "X"}, kind: runs.KindJob, want: "body\nextra"},
		{name: "empty kind skips composer", composer: &stubComposer{text: "X"}, kind: "", want: "body\nextra"},
		{name: "no extra prompt", kind: runs.KindAgent, want: "body\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			wake := &runs.WakeOpts{Body: "body\n", ExtraPrompt: "extra"}
			if tc.name == "no extra prompt" {
				wake = &runs.WakeOpts{Body: "body\n"}
			}
			var composer runs.WakeComposer
			if tc.composer != nil {
				composer = tc.composer
			}
			lopts := wakeLaunchOpts(t, tc.kind, "demo", wake)
			lopts.Prompt = tc.prompt
			run, _, err := runs.New(st, composer, nil).CreateAndLaunch(ctx, lopts)
			if err != nil {
				t.Fatal(err)
			}
			if run.PromptPreview != tc.want {
				t.Fatalf("preview = %q, want %q", run.PromptPreview, tc.want)
			}
		})
	}
}

// A set Prompt wins: the composer must not run.
func TestWakePromptWins(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := &stubComposer{text: "COMPOSED"}
	lopts := wakeLaunchOpts(t, runs.KindAgent, "demo", &runs.WakeOpts{Body: "body"})
	lopts.Prompt = "explicit"
	run, _, err := runs.New(st, c, nil).CreateAndLaunch(context.Background(), lopts)
	if err != nil {
		t.Fatal(err)
	}
	if c.calls != 0 {
		t.Fatalf("composer calls = %d", c.calls)
	}
	if run.PromptPreview != "explicit" {
		t.Fatalf("preview = %q", run.PromptPreview)
	}
}

// TestRequestHintOnJobLaunch pins the requests text: a job or agent launch
// gets it after the prompt, the assistant does not.
func TestRequestHintOnJobLaunch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ kind, want string }{
		{runs.KindJob, "do it\n\nREQUESTS"},
		{runs.KindAgent, "do it\n\nREQUESTS"},
		{"assistant", "do it"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			svc := runs.New(st, nil, nil)
			svc.SetParker(runs.NewParker(svc, nil, &gateStub{hint: "REQUESTS"}))
			lopts := wakeLaunchOpts(t, tc.kind, "demo", nil)
			lopts.Prompt = "do it"
			run, _, err := svc.CreateAndLaunch(context.Background(), lopts)
			if err != nil {
				t.Fatal(err)
			}
			if run.PromptPreview != tc.want {
				t.Fatalf("preview = %q, want %q", run.PromptPreview, tc.want)
			}
		})
	}
}

func wakeLaunchOpts(t *testing.T, kind, job string, wake *runs.WakeOpts) runs.LaunchOpts {
	t.Helper()
	return runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: job, JobName: job, Kind: kind, Harness: "fake",
			Reason: runs.ReasonManual, WorkingDir: t.TempDir(),
		},
		Wake:        wake,
		FakeCommand: []string{"sh", "-c", "true"},
	}
}

// parkedChain builds a service whose runs always park, plus one parked fake
// run to resume from.
func parkedChain(t *testing.T, fake string) (*runs.Service, *runs.Run, string) {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := runs.New(st, nil, nil)
	svc.SetParker(runs.NewParker(svc, nil, &gateStub{open: true}))
	dir := t.TempDir()
	run, _, err := svc.CreateAndLaunch(context.Background(), runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonManual, WorkingDir: dir,
		},
		Prompt:      "ask first",
		FakeCommand: []string{"sh", "-c", fake},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != runs.StatusParked || run.SessionID == "" {
		t.Fatalf("first run: %s session=%q", run.Status, run.SessionID)
	}
	return svc, run, dir
}

// TestResumeRequestNoAttemptCount pins the cap rule: a request resume does not
// count toward MaxResumeAttempts; a plain resume still stops at the cap.
func TestResumeRequestNoAttemptCount(t *testing.T) {
	t.Parallel()
	fake := `echo "$TREBI_RUN_COMPLETE"`
	svc, prev, _ := parkedChain(t, fake)
	ctx := context.Background()
	base := runs.LaunchOpts{FakeCommand: []string{"sh", "-c", fake}}
	for i := range 6 {
		next, err := svc.Resume(ctx, prev.ID, base, runs.ResumeOpts{Reason: runs.ReasonRequest, Prompt: "answer"})
		if err != nil {
			t.Fatalf("request resume %d: %v", i, err)
		}
		if next.ResumeAttempts != 0 || next.ID != prev.ID || next.TurnCount != i+2 {
			t.Fatalf("request resume %d: attempts=%d id=%s turns=%d", i, next.ResumeAttempts, next.ID, next.TurnCount)
		}
		prev = next
	}
	var err error
	for i := range runs.MaxResumeAttempts + 1 {
		prev, err = svc.Resume(ctx, prev.ID, base, runs.ResumeOpts{Reason: runs.ReasonResume})
		if i < runs.MaxResumeAttempts && err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "resume cap") {
		t.Fatalf("plain resume past the cap: %v", err)
	}
}

// TestResumeAcceptsParked pins the guard: a parked run resumes as its next
// turn.
func TestResumeAcceptsParked(t *testing.T) {
	t.Parallel()
	fake := `echo "$TREBI_RUN_COMPLETE"`
	svc, prev, _ := parkedChain(t, fake)
	ctx := context.Background()
	next, err := svc.Resume(ctx, prev.ID, runs.LaunchOpts{FakeCommand: []string{"sh", "-c", fake}},
		runs.ResumeOpts{Reason: runs.ReasonRequest, Prompt: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	if next.SessionID != prev.SessionID || next.ID != prev.ID || next.TurnCount != 2 {
		t.Fatalf("session %q id %s turns %d, want %q %s 2", next.SessionID, next.ID, next.TurnCount, prev.SessionID, prev.ID)
	}
	turns, err := svc.Turns(ctx, prev.ID)
	if err != nil || len(turns) != 2 || turns[1].Reason != runs.ReasonRequest {
		t.Fatalf("turns %+v %v", turns, err)
	}
}

// TestPaneResumePromptExact pins the fake pane contract: the resumed command
// sees the session to resume and the exact prompt.
func TestPaneResumePromptExact(t *testing.T) {
	t.Parallel()
	fake := `printf %s "$TREBI_RESUME_SESSION_ID" > resume.txt; printf %s "$TREBI_FAKE_PROMPT" > prompt.txt; echo "$TREBI_RUN_COMPLETE"`
	svc, prev, dir := parkedChain(t, fake)
	prompt := "[trebi] The person answered.\n\n```json\n{\"text\": \"a `b` \\\"c\\\" <d> \"}\n```\n"
	if _, err := svc.Resume(context.Background(), prev.ID, runs.LaunchOpts{FakeCommand: []string{"sh", "-c", fake}},
		runs.ResumeOpts{Reason: runs.ReasonRequest, Prompt: prompt}); err != nil {
		t.Fatal(err)
	}
	gotSess, _ := os.ReadFile(filepath.Join(dir, "resume.txt"))
	if string(gotSess) != prev.SessionID {
		t.Fatalf("resume session %q, want %q", gotSess, prev.SessionID)
	}
	gotPrompt, _ := os.ReadFile(filepath.Join(dir, "prompt.txt"))
	if string(gotPrompt) != prompt {
		t.Fatalf("prompt %q, want %q", gotPrompt, prompt)
	}
}

// TestFolderEnvRecord pins the run record of a folder .env load: key names
// in the row and the run log, the value only in the harness process.
func TestFolderEnvRecord(t *testing.T) {
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
	svc := runs.New(st, nil, nil)
	out := filepath.Join(dir, "env.txt")
	sum := &folderenv.Summary{Found: true, Enabled: true, Path: "/w/.env", Keys: []string{"TOKEN", "SHARED"},
		Skipped: []folderenv.Skip{{Line: 3, Reason: folderenv.ReasonNoEquals}}, Overridden: []string{"SHARED"}}
	run, _, err := svc.CreateAndLaunch(context.Background(), runs.LaunchOpts{
		CreateOpts:  runs.CreateOpts{WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", FolderEnv: sum},
		Prompt:      "hi",
		Env:         map[string]string{"TOKEN": "secret-value", "SHARED": "from-job"},
		FakeCommand: []string{"sh", "-c", `printf %s "$TOKEN" > ` + out + `; echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FolderEnv == nil || !reflect.DeepEqual(*got.FolderEnv, *sum) {
		t.Fatalf("folder_env = %+v, want %+v", got.FolderEnv, sum)
	}
	if b, _ := os.ReadFile(out); string(b) != "secret-value" {
		t.Fatalf("harness env TOKEN = %q", b)
	}
	log, err := os.ReadFile(filepath.Join(home.RunsLogDir(), run.ID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "Loaded 2 values from .env: TOKEN, SHARED") || !strings.Contains(string(log), "Skipped .env line 3") ||
		strings.Contains(string(log), "secret-value") {
		t.Fatalf("run log = %q", log)
	}
}

func TestLaunchDefaultMaxDuration(t *testing.T) {
	for _, tc := range []struct {
		name, cfg string
		set, want int64
	}{
		{"unset", "", 0, (24 * time.Hour).Milliseconds()},
		{"config", "defaults:\n  session:\n    max_duration: 2h\n", 0, (2 * time.Hour).Milliseconds()},
		{"off", "defaults:\n  session:\n    max_duration: \"off\"\n", 0, 0},
		{"job", "", 5000, 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx := openRuns(t)
			if tc.cfg != "" {
				if err := os.WriteFile(filepath.Join(os.Getenv("TREBI_HOME"), "config.yaml"), []byte(tc.cfg), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			r, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
				CreateOpts: runs.CreateOpts{
					WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonManual,
					MaxDurationMS: tc.set,
				},
				Prompt:      "a",
				FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Get(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Session.MaxDurationMS != tc.want {
				t.Fatalf("max_duration_ms=%d want %d", got.Session.MaxDurationMS, tc.want)
			}
		})
	}
}

func TestCreateAndLaunchRefusesArchivedJob(t *testing.T) {
	t.Parallel()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	idx := func(id, name, kind, owner, archivedAt string) {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO job_index (id, workspace_id, name, path, kind, owner_id, archived_at, updated_at)
			VALUES (?, 'ws', ?, ?, ?, ?, NULLIF(?, ''), '2026-11-18T10:00:00Z')`, id, name, name, kind, owner, archivedAt); err != nil {
			t.Fatal(err)
		}
	}
	const at = "2026-11-18T10:00:00Z"
	idx("agt_live", "live", "agent", "", "")
	idx("agt_old", "old", "agent", "", at)
	idx("jb_own", "live/own", "job", "agt_live", at)
	idx("jb_kid", "old/kid", "job", "agt_old", "")
	idx("jb_ok", "live/ok", "job", "agt_live", "")
	svc := runs.New(st, nil, nil)
	launch := func(id string, reason runs.RunReason) error {
		_, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
			CreateOpts: runs.CreateOpts{
				WorkspaceID: "ws", JobID: id, JobName: id, Harness: "fake", Reason: reason, WorkingDir: t.TempDir(),
			},
			FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
		})
		return err
	}
	cases := []struct {
		id      string
		reason  runs.RunReason
		refused bool
		msg     string
	}{
		{"jb_own", runs.ReasonManual, true, "Unarchive this job first."},
		{"agt_old", runs.ReasonManual, true, "Unarchive this agent first."},
		{"jb_kid", runs.ReasonManual, true, "Unarchive the agent old first."},
		{"jb_own", runs.ReasonMessage, true, "Unarchive this job first."},
		{"jb_own", runs.ReasonResume, true, "Unarchive this job first."},
		{"jb_own", runs.ReasonRequest, false, ""},
		{"jb_own", runs.ReasonReply, false, ""},
		{"jb_ok", runs.ReasonManual, false, ""},
		{"agt_live", runs.ReasonManual, false, ""},
	}
	for _, c := range cases {
		err := launch(c.id, c.reason)
		if c.refused {
			if !errors.Is(err, runs.ErrJobArchived) || runs.JobArchivedMessage(err) != c.msg {
				t.Errorf("%s/%s: err = %v, msg = %q; want %q", c.id, c.reason, err, runs.JobArchivedMessage(err), c.msg)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s/%s: %v", c.id, c.reason, err)
		}
	}
}

func TestStripCompletion(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in, want string }{
		{"only the sentinel", "Answer.\n\nTREBI_RUN_COMPLETE_r1", "Answer."},
		{"bold with key", "Answer.\n**TREBI_RUN_COMPLETE_r1_2**", "Answer."},
		{"list item in backticks", "Answer.\n- `TREBI_RUN_COMPLETE`", "Answer."},
		{"end of line", "Done. TREBI_RUN_COMPLETE_r1.", "Done."},
		{"middle of line", "Done TREBI_RUN_COMPLETE_r1 now", "Done  now"},
		{"no key", "Done. TREBI_RUN_COMPLETE!", "Done."},
		{"no sentinel", "  Plain text.\n\nMore.  ", "  Plain text.\n\nMore.  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runs.StripCompletion(tc.in); got != tc.want {
				t.Fatalf("StripCompletion(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsNoReply(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in string
		want     bool
	}{
		{"only the token", "TREBI_NO_REPLY", true},
		{"bold", "**TREBI_NO_REPLY**", true},
		{"with text", "TREBI_NO_REPLY - nothing to add", true},
		{"no token", "Here is the answer.", false},
		{"near word", "NO_REPLY", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runs.IsNoReply(tc.in); got != tc.want {
				t.Fatalf("IsNoReply(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestChannelText(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in, want string }{
		{"no reply", "TREBI_NO_REPLY", ""},
		{"no reply with text", "Sure.\nTREBI_NO_REPLY", ""},
		{"failure line", "TREBI_RUN_FAILED_r1: x", ""},
		{"failure line after text", "Could not do it.\nTREBI_RUN_FAILED_r1: x", "Could not do it."},
		{"failure mark in a line", "I will end with `TREBI_RUN_FAILED` soon.\nKept.", "Kept."},
		{"sentinel and text", "Answer.\nTREBI_RUN_COMPLETE_r1", "Answer."},
		{"plain text", "  Plain text.\n", "Plain text."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runs.ChannelText(tc.in); got != tc.want {
				t.Fatalf("ChannelText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestQueueItemKeepsTrace(t *testing.T) {
	t.Parallel()
	o := runs.LaunchOpts{CreateOpts: runs.CreateOpts{
		WorkspaceID: "ws1", JobID: "jb1", JobName: "ceo/digest", Reason: runs.ReasonManual,
		TraceID: "T", ChainPath: []string{"x", "jb1"},
	}}
	item := o.QueueItem("manual")
	if item.TraceID != "T" || !reflect.DeepEqual(item.Launch.ChainPath, []string{"x", "jb1"}) {
		t.Fatalf("item trace=%q launch=%+v", item.TraceID, item.Launch)
	}
	plain := (&runs.LaunchOpts{}).QueueItem("manual")
	if plain.TraceID != "" || plain.Launch.ChainPath != nil {
		t.Fatalf("plain item %+v %+v", plain, plain.Launch)
	}
}
