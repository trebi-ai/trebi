//go:build live

package runs_test

import (
	"cmp"
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/files"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// Live resume (scripts/test.resume.sh): each real harness keeps its context
// across two turns of one run. Turn 3 removes the session file and expects a
// new run. Real prompts spend a small amount of quota.
//
// Env:
//
//	TREBI_LIVE_HARNESSES    comma list (default: every installed harness)
//	TREBI_LIVE_RUNTIME      cli (default) or tui
//	TREBI_LIVE_SKIP_NOAUTH  1 skips a harness that is not logged in
//	TREBI_LIVE_MODEL_<H>    model for harness H (else liveResumeModels)
//	TREBI_LIVE_OUT          result file (default build/live-resume.txt)
func TestLiveResume(t *testing.T) {
	runtime := cmp.Or(os.Getenv("TREBI_LIVE_RUNTIME"), "cli")
	names := strings.Split(cmp.Or(os.Getenv("TREBI_LIVE_HARNESSES"), "claude,codex,pi,cursor,opencode,copilot,gemini"), ",")
	out := cmp.Or(os.Getenv("TREBI_LIVE_OUT"), filepath.Join("..", "..", "build", "live-resume.txt"))
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		t.Run(name+"/"+runtime, func(t *testing.T) {
			result := "FAIL"
			t.Cleanup(func() {
				if t.Skipped() {
					result = "SKIP"
				} else if !t.Failed() {
					result = "PASS"
				}
				line := fmt.Sprintf("%s %s %s %s\n", time.Now().UTC().Format(time.RFC3339), name, runtime, result)
				f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err == nil {
					_, _ = f.WriteString(line)
					_ = f.Close()
				}
			})
			liveResume(t, name, runtime)
		})
	}
}

// liveResumeModels is the cheapest model of each harness.
var liveResumeModels = map[string]string{ //nolint:gochecknoglobals // test table
	tmux.Claude: "haiku",
	tmux.Codex:  "gpt-5-mini",
	tmux.Gemini: "gemini-2.5-flash",
}

func liveResume(t *testing.T, name, runtime string) {
	tool, ok := tmux.ByName(name)
	if !ok {
		t.Fatalf("unknown harness %s", name)
	}
	bin := name
	if name == tmux.Cursor {
		bin = "cursor-agent"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not on PATH", bin)
	}
	ctx := context.Background()
	if st := tool.ProbeAuth(ctx); st.Status != "ok" && st.Status != "unknown" {
		if os.Getenv("TREBI_LIVE_SKIP_NOAUTH") == "1" {
			t.Skipf("%s auth %s: %s", name, st.Status, st.Hint)
		}
		t.Fatalf("%s auth %s: %s", name, st.Status, st.Hint)
	}

	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := runs.New(st, nil, files.New(files.DefaultMaxBytes))
	work := t.TempDir()
	model := cmp.Or(os.Getenv("TREBI_LIVE_MODEL_"+strings.ToUpper(name)), liveResumeModels[name])

	launch := func(prompt string) *runs.Run {
		run, _, err := svc.CreateAndLaunch(ctx, runs.LaunchOpts{
			CreateOpts: runs.CreateOpts{
				WorkspaceID: "ws", Job: "live", Kind: runs.KindJob, Harness: name, Runtime: runtime,
				Reason: runs.ReasonTrigger, ConversationKey: "live-" + name, WorkingDir: work,
			},
			Prompt: prompt, Model: model, Conversation: true, ContinueMS: int64(time.Hour / time.Millisecond),
		})
		if err != nil {
			t.Fatal(err)
		}
		return liveWait(t, svc, run.ID)
	}

	word := liveWord(t)
	first := launch("Remember the code word " + word + ". Reply with OK only.")
	second := launch("What is the code word? Reply with the word only.")
	if second.ID != first.ID || second.TurnCount != 2 {
		t.Fatalf("turn 2 run %s turns %d, want %s and 2", second.ID, second.TurnCount, first.ID)
	}
	turns, err := svc.Turns(ctx, first.ID)
	if err != nil || len(turns) != 2 {
		t.Fatalf("turns %+v err %v", turns, err)
	}
	if turns[0].SessionID == "" || turns[1].SessionID == "" {
		t.Fatalf("turn sessions %q %q", turns[0].SessionID, turns[1].SessionID)
	}
	if turns[1].SessionID != turns[0].SessionID {
		t.Logf("%s forked the session on resume: %s -> %s", name, turns[0].SessionID, turns[1].SessionID)
	}
	if name == tmux.Claude || name == tmux.Codex {
		for _, tr := range turns {
			if tr.Tokens.Input == 0 {
				t.Errorf("turn %d records no input tokens", tr.N)
			}
		}
	}

	path, pathErr := tool.TranscriptPath(home.HarnessDir(name), tmux.TranscriptWorkingDir(name, second.WorkingDir, second.StartedAt), second.SessionID)
	entries := liveEntries(ctx, t, svc, tool, second, path, pathErr)
	reply := ""
	prompts := 0
	for _, e := range entries {
		switch e.Role {
		case "assistant":
			reply = e.Text
		case "user":
			prompts++
		}
	}
	if !strings.Contains(strings.ToLower(reply), strings.ToLower(word)) {
		t.Fatalf("turn 2 reply %q lacks the code word %q", reply, word)
	}
	if pathErr == nil && prompts < 2 {
		t.Errorf("session file %s holds %d prompts, want 2", path, prompts)
	}

	// Turn 3: the session file is gone, so the follow-up starts a new run.
	if pathErr != nil {
		t.Logf("%s: no session file (%v); skip turn 3", name, pathErr)
		return
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	third := launch("Reply with OK only.")
	if third.ID == first.ID || third.PreviousRunID != first.ID {
		t.Fatalf("turn 3 run %s previous %q, want a new run after %s", third.ID, third.PreviousRunID, first.ID)
	}
}

// liveEntries reads the run conversation from the wire events, or from the
// session file for a pane run.
func liveEntries(ctx context.Context, t *testing.T, svc *runs.Service, tool tmux.Tool, run *runs.Run, path string, pathErr error) []tmux.Turn {
	t.Helper()
	if run.Runtime == "cli" {
		entries, err := svc.CLITurns(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) > 0 {
			return entries
		}
	}
	if pathErr != nil {
		t.Fatalf("no transcript: %v", pathErr)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := tool.ParseTranscript(f)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// liveWait waits for the turn to end; MaxWait caps one WaitFor call.
func liveWait(t *testing.T, svc *runs.Service, id string) *runs.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		run, err := svc.WaitFor(context.Background(), id, runs.MaxWait)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status.IsTerminal() {
			return run
		}
	}
	t.Fatalf("run %s did not end in 5m", id)
	return nil
}

func liveWord(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(26))
		if err != nil {
			t.Fatal(err)
		}
		b[i] = byte('a' + n.Int64())
	}
	return string(b)
}
