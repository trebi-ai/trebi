//go:build live_cli

package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/tmux"
)

// claudeTuning keeps live Claude turns tiny and cheap: haiku, capped turns,
// capped list-price budget (the operator's subscription still meters usage).
func claudeTuning(req *StartRequest, maxTurns string) {
	req.ExtraArgs = append(req.ExtraArgs,
		"--model", "haiku",
		"--max-turns", maxTurns,
		"--max-budget-usd", "1")
}

// TestLiveClaudeSharedAuth: the probe the daemon uses must pass, and darwin
// must still pin the default Keychain (I.1).
func TestLiveClaudeSharedAuth(t *testing.T) {
	if !liveEnabled(t, "claude") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "claude")

	cmd, env := livePlan(t, "claude")
	if cmd == "" {
		t.Fatal("empty launch command")
	}
	if env["CLAUDE_CONFIG_DIR"] == "" {
		t.Fatalf("isolated config dir missing: %+v", env)
	}
	if runtime.GOOS == "darwin" {
		v, ok := env["CLAUDE_SECURESTORAGE_CONFIG_DIR"]
		if !ok || v != "" {
			t.Fatalf("keychain pin lost: %+v", env)
		}
	}
}

// TestLiveClaudeOneShot: -p returns PONG, exits 0, result success, usage > 0.
func TestLiveClaudeOneShot(t *testing.T) {
	if !liveEnabled(t, "claude") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "claude")

	req := liveStartRequest(t, "claude", true, "Reply with exactly PONG and nothing else.")
	claudeTuning(&req, "1")
	s, err := claudeStart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	text, res := assistantText(t, s, 3*time.Minute)
	if res.Result == nil || res.Result.IsError {
		t.Fatalf("result: %+v", res.Result)
	}
	if !pongMatch(text) {
		t.Fatalf("assistant text = %q, want PONG", text)
	}
	if res.Result.Usage.Output <= 0 {
		t.Fatalf("usage empty: %+v", res.Result.Usage)
	}
	if s.ID() == "" {
		t.Fatalf("session id not captured")
	}
}

// TestLiveClaudePersistent: init session id matches, a second prompt keeps
// context, interrupt, and close preserves the last assistant line.
func TestLiveClaudePersistent(t *testing.T) {
	if !liveEnabled(t, "claude") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "claude")

	req := liveStartRequest(t, "claude", false, "")
	s, err := claudeStart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	if err := s.Prompt(context.Background(), Prompt{Text: "Remember the word BANANA. Reply OK.", Kind: "job"}); err != nil {
		t.Fatal(err)
	}
	text, res := assistantText(t, s, 3*time.Minute)
	if res.Result == nil || res.Result.IsError {
		t.Fatalf("first result: %+v", res.Result)
	}
	if !strings.Contains(strings.ToUpper(text), "OK") {
		t.Fatalf("first reply: %q", text)
	}
	initID := s.ID()
	if initID == "" {
		t.Fatal("no session id after init")
	}
	if res.Type == EventResult && res.Result.SessionID != "" && res.Result.SessionID != initID {
		t.Fatalf("result session id %q != init %q", res.Result.SessionID, initID)
	}

	if err := s.Prompt(context.Background(), Prompt{Text: "What word did I ask you to remember? Answer with just that word.", Kind: "followup"}); err != nil {
		t.Fatal(err)
	}
	text2, res2 := assistantText(t, s, 3*time.Minute)
	if res2.Result == nil || res2.Result.IsError {
		t.Fatalf("second result: %+v", res2.Result)
	}
	if !strings.Contains(strings.ToUpper(text2), "BANANA") {
		t.Fatalf("second turn lost context: %q", text2)
	}
	if s.ID() != initID {
		t.Fatalf("session id changed: %q -> %q", initID, s.ID())
	}

	// Interrupt a long turn: the wire must terminate it, not hang (Part L.2).
	if err := s.Prompt(context.Background(), Prompt{Text: "Count from 1 to 200, one number per line.", Kind: "followup"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	interrupted := false
	deadline := time.After(3 * time.Minute)
	for !interrupted {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("session ended after interrupt")
			}
			if ev.Type == EventResult {
				interrupted = true
			}
		case <-deadline:
			t.Fatal("interrupt did not end the turn")
		}
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("close after interrupt: %v", err)
	}
}

// TestLiveClaudeAskUserQuestion verifies G.4 explicitly: AskUserQuestion
// surfaces on the wire as a control request the approval inbox can answer.
func TestLiveClaudeAskUserQuestion(t *testing.T) {
	if !liveEnabled(t, "claude") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "claude")

	req := liveStartRequest(t, "claude", false, "")
	claudeTuning(&req, "4")
	req.ExtraArgs = append(req.ExtraArgs, "--permission-mode", "default")
	s, err := claudeStart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	if err := s.Prompt(context.Background(), Prompt{
		Text: "Use the AskUserQuestion tool to ask me to pick a color, with options red and blue. Then stop.",
		Kind: "job",
	}); err != nil {
		t.Fatal(err)
	}
	answerer, ok := s.(Answerer)
	if !ok {
		t.Fatal("session does not answer permissions")
	}
	sawQuestion := false
	deadline := time.After(5 * time.Minute)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("session ended before a result")
			}
			switch ev.Type {
			case EventPermission:
				if ev.Permission != nil && ev.Permission.Tool == "AskUserQuestion" {
					sawQuestion = true
				}
				if ev.Permission != nil {
					_ = answerer.AnswerPermission(context.Background(), ev.Permission.ID, Decision{Allow: true})
				}
			case EventResult:
				if !sawQuestion {
					t.Fatal("AskUserQuestion never surfaced as a control request")
				}
				return
			}
		case <-deadline:
			t.Fatalf("timeout (AskUserQuestion seen=%v)", sawQuestion)
		}
	}
}

// TestLiveClaudeResume: resume keeps the same vendor session id.
func TestLiveClaudeResume(t *testing.T) {
	if !liveEnabled(t, "claude") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "claude")

	req := liveStartRequest(t, "claude", true, "Reply with exactly PONG.")
	claudeTuning(&req, "1")
	s, err := claudeStart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	_, res := assistantText(t, s, 3*time.Minute)
	if res.Result == nil || res.Result.IsError {
		t.Fatalf("first result: %+v", res.Result)
	}
	firstID := s.ID()
	if firstID == "" {
		t.Fatal("no session id")
	}
	_ = s.Close(context.Background())

	tool, _ := tmux.ByName("claude")
	plan, err := tool.PrepareResume(liveBin("claude"), firstID)
	if err != nil {
		t.Fatal(err)
	}
	resumeReq := StartRequest{
		Tool:          "claude",
		BaseCommand:   plan.Command,
		WorkingDir:    t.TempDir(),
		Env:           plan.Env,
		SessionID:     firstID,
		OneShot:       true,
		OneShotPrompt: "Reply with exactly PONG.",
		Persistent:    false,
	}
	claudeTuning(&resumeReq, "1")
	s2, err := claudeStart(context.Background(), resumeReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close(context.Background()) }()
	_, res2 := assistantText(t, s2, 3*time.Minute)
	if res2.Result == nil || res2.Result.IsError {
		t.Fatalf("resume result: %+v", res2.Result)
	}
	if s2.ID() != firstID {
		t.Fatalf("resume session id = %q, want %q", s2.ID(), firstID)
	}
}

// TestLiveClaudePermission: a Bash prompt under default permissions produces
// a can_use_tool control frame; allow runs the side effect, deny completes.
func TestLiveClaudePermission(t *testing.T) {
	if !liveEnabled(t, "claude") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "claude")

	dir := t.TempDir()
	target := filepath.Join(dir, "trebi-perm.txt")
	req := liveStartRequest(t, "claude", false, "")
	req.WorkingDir = dir
	claudeTuning(&req, "4")
	req.ExtraArgs = append(req.ExtraArgs, "--permission-mode", "default")
	s, err := claudeStart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	prompt := "Use the Bash tool to run exactly this command: printf hello > " + target + "\nThen reply DONE."
	if err := s.Prompt(context.Background(), Prompt{Text: prompt, Kind: "job"}); err != nil {
		t.Fatal(err)
	}
	perm := waitEvent(t, s, EventPermission, 3*time.Minute)
	if perm.Permission == nil || perm.Permission.Tool == "" {
		t.Fatalf("permission: %+v", perm.Permission)
	}
	answerer, ok := s.(Answerer)
	if !ok {
		t.Fatal("session does not answer permissions")
	}
	if err := answerer.AnswerPermission(context.Background(), perm.Permission.ID, Decision{Allow: true}); err != nil {
		t.Fatal(err)
	}
	_, res := assistantText(t, s, 3*time.Minute)
	if res.Result == nil || res.Result.IsError {
		t.Fatalf("allow result: %+v", res.Result)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("allowed side effect missing: %v", err)
	}
}

// TestLiveClaudePermissionDenied: denying completes the turn without the side
// effect. This round-trip is the one the tmux path could never prove.
func TestLiveClaudePermissionDenied(t *testing.T) {
	if !liveEnabled(t, "claude") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "claude")

	dir := t.TempDir()
	target := filepath.Join(dir, "trebi-denied.txt")
	req := liveStartRequest(t, "claude", false, "")
	req.WorkingDir = dir
	claudeTuning(&req, "4")
	req.ExtraArgs = append(req.ExtraArgs, "--permission-mode", "default")
	s, err := claudeStart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	prompt := "Use the Bash tool to run exactly this command: printf nope > " + target + "\nThen reply DONE."
	if err := s.Prompt(context.Background(), Prompt{Text: prompt, Kind: "job"}); err != nil {
		t.Fatal(err)
	}
	perm := waitEvent(t, s, EventPermission, 3*time.Minute)
	answerer := s.(Answerer)
	if err := answerer.AnswerPermission(context.Background(), perm.Permission.ID, Decision{Allow: false, Message: "operator denied in the live test"}); err != nil {
		t.Fatal(err)
	}
	_, res := assistantText(t, s, 3*time.Minute)
	if res.Result == nil || res.Result.IsError {
		t.Fatalf("denied turn must still complete: %+v", res.Result)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("denied side effect was written")
	}
}
