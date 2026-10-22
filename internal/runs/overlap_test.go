package runs_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestIfRunningSkip(t *testing.T) {
	svc, ctx := openRuns(t)
	live, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonManual,
		TmuxSession: "trebi-live-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if live.Status != runs.StatusRunning {
		t.Fatalf("%s", live.Status)
	}

	r2, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonTrigger,
			PromptPreview: "b",
		},
		Prompt: "b", IfRunning: "skip",
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "skip" {
		t.Fatalf("outcome=%s", outcome)
	}
	if r2.Status != runs.StatusSkipped {
		t.Fatalf("status=%s want skipped", r2.Status)
	}
	if r2.EndReason != runs.EndSkip {
		t.Fatalf("end_reason=%s want skip", r2.EndReason)
	}
	// original still running
	got, _ := svc.Get(ctx, live.ID)
	if got.Status != runs.StatusRunning {
		t.Fatalf("live status=%s", got.Status)
	}
	skipped, _ := svc.Get(ctx, r2.ID)
	if b := skipped.BlockedBy; b == nil || b.RunID != live.ID || b.Status != runs.StatusRunning || b.StartedAt.IsZero() {
		t.Fatalf("blocked_by=%+v want %s", b, live.ID)
	}
}

func TestIfRunningReplace(t *testing.T) {
	svc, ctx := openRuns(t)
	live, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", TmuxSession: "trebi-live-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonManual,
		},
		Prompt: "x", IfRunning: "replace",
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "replace" {
		t.Fatalf("outcome=%s", outcome)
	}
	got, _ := svc.Get(ctx, live.ID)
	if got.Status != runs.StatusExited || got.EndReason != runs.EndReplace {
		t.Fatalf("replaced run: status=%s end=%s", got.Status, got.EndReason)
	}
	if r2.Status != runs.StatusComplete && r2.Status != runs.StatusRunning {
		t.Fatalf("new run status=%s", r2.Status)
	}
}

func TestIfRunningCoalesce(t *testing.T) {
	svc, ctx := openRuns(t)
	live, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", TmuxSession: "trebi-live-3",
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonTrigger,
		},
		Prompt: "coalesce me", Conversation: true, IfRunning: "skip",
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "coalesce" {
		t.Fatalf("outcome=%s", outcome)
	}
	if r2.ID != live.ID {
		t.Fatalf("coalesce should return existing id %s got %s", live.ID, r2.ID)
	}
	// timeline event coalesce
	tl, err := svc.Timeline(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range tl.Events {
		if e.Kind == "coalesce" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected coalesce event in timeline: %+v", tl.Events)
	}
}

func TestIfRunningBypass(t *testing.T) {
	svc, ctx := openRuns(t)
	_, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", TmuxSession: "trebi-live-4",
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonManual,
		},
		Prompt: "parallel", BypassOverlap: true,
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "" {
		t.Fatalf("outcome=%s", outcome)
	}
	if r2.Status != runs.StatusComplete && r2.Status != runs.StatusRunning {
		t.Fatalf("status=%s", r2.Status)
	}
	// two rows for job demo
	list, err := svc.List(ctx, runs.ListOpts{Workspace: "ws", Job: "demo", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 2 {
		t.Fatalf("want ≥2 runs got %d", len(list))
	}
}

// TestIfRunningQueueDefault proves an empty if_running resolves to queue: a
// live run returns ErrRunActive and no row is created.
func TestIfRunningQueueDefault(t *testing.T) {
	svc, ctx := openRuns(t)
	if _, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", TmuxSession: "trebi-live-q",
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonManual},
		Prompt:     "later",
	})
	if !errors.Is(err, runs.ErrRunActive) {
		t.Fatalf("err=%v, want ErrRunActive", err)
	}
	list, err := svc.List(ctx, runs.ListOpts{Workspace: "ws", Job: "demo", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("runs=%d, want 1", len(list))
	}
}

func openRuns(t *testing.T) (*runs.Service, context.Context) {
	t.Helper()
	svc, _, ctx := openRunsStore(t)
	return svc, ctx
}

// TestIfRunningPerConversation proves the overlap check is scoped to one
// conversation key (plan E.3): a live run of another chat does not block a
// launch, and the same conversation coalesces against its own live run.
func TestIfRunningPerConversation(t *testing.T) {
	svc, ctx := openRuns(t)
	if _, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "chat", JobName: "chat", Harness: "fake", Reason: runs.ReasonManual,
		ConversationKey: "conv-a",
	}); err != nil {
		t.Fatal(err)
	}
	// A different conversation launches a second run.
	r2, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "chat", JobName: "chat", Harness: "fake", Reason: runs.ReasonTrigger,
			ConversationKey: "conv-b",
		},
		Prompt: "b", IfRunning: "skip",
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "" || r2.Status == runs.StatusSkipped {
		t.Fatalf("outcome=%q status=%s; a second conversation must launch", outcome, r2.Status)
	}
	// A live run of conv-b makes the next conv-b item coalesce.
	live, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "chat", JobName: "chat", Harness: "fake", Reason: runs.ReasonManual,
		ConversationKey: "conv-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	r3, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "chat", JobName: "chat", Harness: "fake", Reason: runs.ReasonTrigger,
			ConversationKey: "conv-b",
		},
		Prompt: "again", Conversation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "coalesce" || r3.ID != live.ID {
		t.Fatalf("outcome=%q id=%s want coalesce on %s", outcome, r3.ID, live.ID)
	}
}

// TestIfRunningEmptyKeyMatchesOnlyUnkeyed proves an unkeyed launch ignores a
// keyed live run of the same job.
func TestIfRunningEmptyKeyMatchesOnlyUnkeyed(t *testing.T) {
	svc, ctx := openRuns(t)
	if _, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "mixed", JobName: "mixed", Harness: "fake", Reason: runs.ReasonManual,
		ConversationKey: "conv-a",
	}); err != nil {
		t.Fatal(err)
	}
	r2, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "mixed", JobName: "mixed", Harness: "fake", Reason: runs.ReasonTrigger,
		},
		Prompt: "unkeyed", IfRunning: "skip",
		FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "" || r2.Status == runs.StatusSkipped {
		t.Fatalf("outcome=%q status=%s; a keyed live run must not block an unkeyed launch", outcome, r2.Status)
	}
}

// TestCoalesceFindsKeyedRunPastTwentyLive proves the overlap filter runs in
// SQL (P1-17) and the coalesce sends the message text (P1-13): the keyed run
// is older than 25 unkeyed live runs and still takes the text.
func TestCoalesceFindsKeyedRunPastTwentyLive(t *testing.T) {
	svc, ctx := openRuns(t)
	chat, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "busy", JobName: "busy", Harness: "fake", Reason: runs.ReasonManual,
		ConversationKey: "chat-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		if _, err := svc.Create(ctx, runs.CreateOpts{
			WorkspaceID: "ws", JobID: "busy", JobName: "busy", Harness: "fake", Reason: runs.ReasonManual,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "busy", JobName: "busy", Harness: "fake", Reason: runs.ReasonTrigger,
			ConversationKey: "chat-1",
		},
		Wake:         &runs.WakeOpts{Message: "second message"},
		CoalesceText: "second message",
		Conversation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "coalesce" || got.ID != chat.ID {
		t.Fatalf("outcome=%q id=%s, want coalesce on %s", outcome, got.ID, chat.ID)
	}
	tl, err := svc.Timeline(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, e := range tl.Events {
		if e.Kind == "coalesce" {
			var d map[string]string
			_ = json.Unmarshal(e.Detail, &d)
			text = d["prompt_preview"]
		}
	}
	if text != "second message" {
		t.Fatalf("coalesce text=%q, want the message", text)
	}
}

// TestActionErrorKeepsLiveRun proves actions resolve before the overlap
// policy (P1-15): a launch that fails on its actions does not replace the
// live run.
func TestActionErrorKeepsLiveRun(t *testing.T) {
	svc, ctx := openRuns(t)
	t.Setenv("TREBI_FAKE_HARNESS", "")
	live, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "acts", JobName: "acts", Harness: "claude", Reason: runs.ReasonManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "acts", JobName: "acts", Harness: "claude", Reason: runs.ReasonTrigger,
		},
		Prompt: "x", IfRunning: "replace", JobConnectors: []string{"ghost"},
	})
	if !errors.Is(err, runs.ErrActionsInvalid) {
		t.Fatalf("err=%v, want ErrActionsInvalid", err)
	}
	got, _ := svc.Get(ctx, live.ID)
	if got.Status != runs.StatusRunning {
		t.Fatalf("live status=%s end=%s, want running", got.Status, got.EndReason)
	}
}

// TestContinueConversation pins session.continue: with no live run, an event
// of a known conversation resumes the session of its last finished run.
func TestContinueConversation(t *testing.T) {
	const day = int64(24 * time.Hour / time.Millisecond)
	cases := []struct {
		name       string
		continueMS int64
		status     runs.Status
		harness    string
		transcript bool
		wantResume bool
		wantEvent  bool
	}{
		{name: "complete run continues", continueMS: day, status: runs.StatusComplete, harness: "fake", transcript: true, wantResume: true},
		{name: "timed out run continues", continueMS: day, status: runs.StatusTimedOut, harness: "fake", transcript: true, wantResume: true},
		{name: "setting off", continueMS: 0, status: runs.StatusComplete, harness: "fake", transcript: true},
		{name: "outside window", continueMS: 1, status: runs.StatusComplete, harness: "fake", transcript: true},
		{name: "failed run starts fresh", continueMS: day, status: runs.StatusFailed, harness: "fake", transcript: true},
		{name: "other harness starts fresh", continueMS: day, status: runs.StatusComplete, harness: "codex", transcript: true},
		{name: "session file missing", continueMS: day, status: runs.StatusComplete, harness: "fake", wantEvent: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx := openRuns(t)
			if tc.transcript {
				dir := home.HarnessDir("fake")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			prev, err := svc.Create(ctx, runs.CreateOpts{
				WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: tc.harness, Reason: runs.ReasonTrigger,
				ConversationKey: "thread-1", SessionID: "sess-1", WorkingDir: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.Terminate(ctx, prev.ID, tc.status, runs.EndResult, nil); err != nil {
				t.Fatal(err)
			}
			time.Sleep(5 * time.Millisecond)
			run, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
				CreateOpts: runs.CreateOpts{
					WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonTrigger,
					ConversationKey: "thread-1", QueueID: "q-2",
				},
				Prompt: "reply", Conversation: true, ContinueMS: tc.continueMS,
				FakeCommand: []string{"sh", "-c", `echo "$TREBI_RUN_COMPLETE"`},
			})
			if err != nil {
				t.Fatal(err)
			}
			if outcome != "" {
				t.Fatalf("outcome=%q: want a launch", outcome)
			}
			reopened := run.ID == prev.ID
			if reopened != tc.wantResume {
				t.Fatalf("run=%s prev=%s, want reopen=%v", run.ID, prev.ID, tc.wantResume)
			}
			if run.Reason != runs.ReasonTrigger {
				t.Fatalf("reason=%s want trigger", run.Reason)
			}
			if reopened {
				turns, err := svc.Turns(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if run.TurnCount != 2 || run.SessionID != "sess-1" || len(turns) != 2 || turns[1].QueueID != "q-2" || turns[1].Reason != runs.ReasonTrigger {
					t.Fatalf("reopened run turns=%d session=%q rows=%+v", run.TurnCount, run.SessionID, turns)
				}
			} else {
				got, _ := svc.Get(ctx, prev.ID)
				if got.Status != tc.status || got.TurnCount != 1 {
					t.Fatalf("previous status=%s turns=%d, want %s and 1", got.Status, got.TurnCount, tc.status)
				}
				if follows := run.PreviousRunID == prev.ID; follows != tc.wantEvent {
					t.Fatalf("previous_run_id=%q, want set=%v", run.PreviousRunID, tc.wantEvent)
				}
			}
			tl, err := svc.Timeline(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			event := slices.ContainsFunc(tl.Events, func(e runs.TimelinePoint) bool { return e.Kind == "continue_fallback" })
			if event != tc.wantEvent {
				t.Fatalf("continue_fallback event=%v want %v", event, tc.wantEvent)
			}
		})
	}
}

// TestContinueConversationCoalesces proves a live run of the conversation
// still takes the message, so two close events never resume one session twice.
func TestContinueConversationCoalesces(t *testing.T) {
	svc, ctx := openRuns(t)
	live, err := svc.Create(ctx, runs.CreateOpts{
		WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", ConversationKey: "thread-1",
		SessionID: "sess-1", TmuxSession: "trebi-live-c",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, outcome, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
		CreateOpts: runs.CreateOpts{
			WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonTrigger,
			ConversationKey: "thread-1",
		},
		Prompt: "reply", Conversation: true, ContinueMS: 3600_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != runs.OverlapCoalesce || run.ID != live.ID {
		t.Fatalf("outcome=%q run=%s: want coalesce into %s", outcome, run.ID, live.ID)
	}
	// A reply to a live turn joins it: no new turn.
	got, err := svc.Get(ctx, live.ID)
	if err != nil || got.TurnCount != 1 {
		t.Fatalf("turns after coalesce %v err %v", got, err)
	}
	tl, err := svc.Timeline(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(tl.Events, func(ev runs.TimelinePoint) bool { return ev.Kind == "coalesce" }) {
		t.Fatal("no coalesce event")
	}
}

// TestContinueFollowUp proves a follow-up continues with no time limit: a
// resume sends the message, and a fresh session gets the fresh prompt.
func TestContinueFollowUp(t *testing.T) {
	cases := []struct {
		name       string
		transcript bool
		wantResume bool
		wantPrompt string
	}{
		{name: "resume sends the message", transcript: true, wantResume: true, wantPrompt: "msg"},
		{name: "session file missing", wantPrompt: "fresh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, ctx := openRunsStore(t)
			if tc.transcript {
				dir := home.HarnessDir("fake")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			prev, err := svc.Create(ctx, runs.CreateOpts{
				WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonTrigger,
				ConversationKey: "r1/e1", SessionID: "sess-1", WorkingDir: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.Terminate(ctx, prev.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
				t.Fatal(err)
			}
			// 60 days ago: a follow-up has no window.
			old := time.Now().Add(-60 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
			if _, err := st.DB().Exec(`UPDATE runs SET ended_at=? WHERE id=?`, old, prev.ID); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "prompt")
			run, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
				CreateOpts: runs.CreateOpts{
					WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", Reason: runs.ReasonTrigger,
					ConversationKey: "r1/e1", QueueID: "q-2",
				},
				Prompt: "job prompt", Conversation: true,
				FollowUp:    &runs.FollowUp{Text: "msg", FreshPrompt: "fresh"},
				FakeCommand: []string{"sh", "-c", `printf %s "$TREBI_FAKE_PROMPT" > ` + out + `; echo "$TREBI_RUN_COMPLETE"`},
			})
			if err != nil {
				t.Fatal(err)
			}
			if reopened := run.ID == prev.ID; reopened != tc.wantResume {
				t.Fatalf("run=%s prev=%s, want reopen=%v", run.ID, prev.ID, tc.wantResume)
			}
			deadline := time.Now().Add(5 * time.Second)
			for b, _ := os.ReadFile(out); len(b) == 0 && time.Now().Before(deadline); b, _ = os.ReadFile(out) {
				time.Sleep(10 * time.Millisecond)
			}
			if b, _ := os.ReadFile(out); string(b) != tc.wantPrompt {
				t.Fatalf("prompt = %q, want %q", b, tc.wantPrompt)
			}
		})
	}
}

// TestContinueSendsMessage proves session.continue: a resume sends the
// message, and a fresh session gets the job prompt or the agent wake.
func TestContinueSendsMessage(t *testing.T) {
	cases := []struct {
		name       string
		kind       string
		transcript bool
		wantPrompt string
	}{
		{name: "resume sends the message", kind: runs.KindJob, transcript: true, wantPrompt: "msg"},
		{name: "session file missing", kind: runs.KindJob, wantPrompt: "job prompt"},
		{name: "agent resume sends the message", kind: runs.KindAgent, transcript: true, wantPrompt: "msg"},
		{name: "agent session file missing", kind: runs.KindAgent, wantPrompt: "agent body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx := openRuns(t)
			if tc.transcript {
				dir := home.HarnessDir("fake")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			prev, err := svc.Create(ctx, runs.CreateOpts{
				WorkspaceID: "ws", JobID: "demo", JobName: "demo", Kind: tc.kind, Harness: "fake", Reason: runs.ReasonTrigger,
				ConversationKey: "room-1", SessionID: "sess-1", WorkingDir: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.Terminate(ctx, prev.ID, runs.StatusComplete, runs.EndKeepAlive, nil); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "prompt")
			opts := runs.LaunchOpts{
				CreateOpts: runs.CreateOpts{
					WorkspaceID: "ws", JobID: "demo", JobName: "demo", Kind: tc.kind, Harness: "fake", Reason: runs.ReasonTrigger,
					ConversationKey: "room-1", QueueID: "q-2",
				},
				Prompt: "job prompt", CoalesceText: "msg", Conversation: true, ContinueMS: 3600_000,
				FakeCommand: []string{"sh", "-c", `printf %s "$TREBI_FAKE_PROMPT" > ` + out + `; echo "$TREBI_RUN_COMPLETE"`},
			}
			if tc.kind == runs.KindAgent {
				opts.Prompt, opts.Wake = "", &runs.WakeOpts{Body: "agent body", Recap: "user: earlier", Message: "msg"}
			}
			run, _, err := svc.CreateAndLaunch(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			if reopened := run.ID == prev.ID; reopened != tc.transcript {
				t.Fatalf("run=%s prev=%s, want reopen=%v", run.ID, prev.ID, tc.transcript)
			}
			deadline := time.Now().Add(5 * time.Second)
			for b, _ := os.ReadFile(out); len(b) == 0 && time.Now().Before(deadline); b, _ = os.ReadFile(out) {
				time.Sleep(10 * time.Millisecond)
			}
			if b, _ := os.ReadFile(out); string(b) != tc.wantPrompt {
				t.Fatalf("prompt = %q, want %q", b, tc.wantPrompt)
			}
		})
	}
}

// TestConversationOpen proves the probe of the follow-up gate (plan A.9).
func TestConversationOpen(t *testing.T) {
	svc, st, ctx := openRunsStore(t)
	open := func(key string, within time.Duration) bool {
		t.Helper()
		ok, err := svc.ConversationOpen(ctx, "ws", "demo", key, within)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if open("r1/e1", 0) {
		t.Fatal("no run is an open conversation")
	}
	live, err := svc.Create(ctx, runs.CreateOpts{WorkspaceID: "ws", JobID: "demo", JobName: "demo", Harness: "fake", ConversationKey: "r1/e1", Trigger: &runs.TriggerCapture{Vars: map[string]string{"text": "first"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !open("r1/e1", time.Hour) || open("r1/e2", 0) || open("r1", 0) {
		t.Fatal("a live run opens only its own key")
	}
	if err := svc.Terminate(ctx, live.ID, runs.StatusComplete, runs.EndResult, nil); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`UPDATE runs SET ended_at=? WHERE id=?`, old, live.ID); err != nil {
		t.Fatal(err)
	}
	if !open("r1/e1", 0) || open("r1/e1", time.Hour) || !open("r1/e1", 3*time.Hour) {
		t.Fatal("an ended run is open with no limit or inside the limit only")
	}
	vars, err := svc.FirstTriggerVars(ctx, "ws", "demo", "r1/e1")
	if err != nil || vars["text"] != "first" {
		t.Fatalf("first vars = %v %v", vars, err)
	}
}

func openRunsStore(t *testing.T) (*runs.Service, *store.Store, context.Context) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return runs.New(st, nil, nil), st, context.Background()
}
