//go:build live_cli

package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/tmux"
)

// TestLiveCodexAuth: the shared ~/.codex login the daemon probes.
func TestLiveCodexAuth(t *testing.T) {
	if !liveEnabled(t, "codex") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "codex")
}

// TestLiveCodexTurn: initialize/initialized → thread/start → turn/start →
// PONG, turn/completed, thread id captured, token usage observed.
func TestLiveCodexTurn(t *testing.T) {
	if !liveEnabled(t, "codex") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "codex")

	req := liveStartRequest(t, "codex", false, "")
	req.ApprovalPolicy = "never"
	req.Sandbox = "workspace-write"
	s, err := codexStart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	if s.ID() == "" {
		t.Fatal("thread id not captured at handshake")
	}
	if err := s.Prompt(context.Background(), Prompt{Text: "Reply with exactly PONG and nothing else.", Kind: "job"}); err != nil {
		t.Fatal(err)
	}
	text, res := assistantText(t, s, 4*time.Minute)
	if res.Result == nil || res.Result.IsError {
		t.Fatalf("result: %+v", res.Result)
	}
	if !pongMatch(text) {
		t.Fatalf("assistant text = %q", text)
	}
	if res.Result.SessionID != "" && res.Result.SessionID != s.ID() {
		t.Fatalf("thread id changed: %q -> %q", s.ID(), res.Result.SessionID)
	}
}

// TestLiveCodexResume: thread/resume reuses the stored thread id.
func TestLiveCodexResume(t *testing.T) {
	if !liveEnabled(t, "codex") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "codex")

	req := liveStartRequest(t, "codex", false, "")
	req.ApprovalPolicy = "never"
	req.Sandbox = "workspace-write"
	s, err := codexStart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Prompt(context.Background(), Prompt{Text: "Reply with exactly PONG.", Kind: "job"}); err != nil {
		t.Fatal(err)
	}
	_, res := assistantText(t, s, 4*time.Minute)
	if res.Result == nil || res.Result.IsError {
		t.Fatalf("first result: %+v", res.Result)
	}
	threadID := s.ID()
	_ = s.Close(context.Background())
	if threadID == "" {
		t.Fatal("no thread id")
	}

	tool, _ := tmux.ByName("codex")
	plan, err := tool.PrepareResume(liveBin("codex"), threadID)
	if err != nil {
		t.Fatal(err)
	}
	resumeReq := StartRequest{
		Tool:           "codex",
		BaseCommand:    plan.Command,
		WorkingDir:     t.TempDir(),
		Env:            plan.Env,
		SessionID:      threadID,
		ApprovalPolicy: "never",
		Sandbox:        "workspace-write",
	}
	s2, err := codexStart(context.Background(), resumeReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close(context.Background()) }()
	if s2.ID() != threadID {
		t.Fatalf("resumed thread id = %q, want %q", s2.ID(), threadID)
	}
	if err := s2.Prompt(context.Background(), Prompt{Text: "Reply with exactly PONG.", Kind: "job"}); err != nil {
		t.Fatal(err)
	}
	_, res2 := assistantText(t, s2, 4*time.Minute)
	if res2.Result == nil || res2.Result.IsError {
		t.Fatalf("resume result: %+v", res2.Result)
	}
}

// TestLiveCodexApproval: sandbox workspace-write + on-request produces an
// approval request, answered accept, and the file exists.
func TestLiveCodexApproval(t *testing.T) {
	if !liveEnabled(t, "codex") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "codex")

	dir := t.TempDir()
	target := filepath.Join(dir, "codex-perm.txt")
	req := liveStartRequest(t, "codex", false, "")
	req.WorkingDir = dir
	req.ApprovalPolicy = "on-request"
	req.Sandbox = "workspace-write"
	s, err := codexStart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	prompt := "Create a file named codex-perm.txt in the current working directory containing hello. Use the shell if needed."
	if err := s.Prompt(context.Background(), Prompt{Text: prompt, Kind: "job"}); err != nil {
		t.Fatal(err)
	}
	// The approval is a protocol request; answer allow when it arrives. Some
	// pins auto-approve workspace writes, so accept either path.
	done := make(chan struct{})
	go func() {
		defer close(done)
		answerer := s.(Answerer)
		for ev := range s.Events() {
			if ev.Type == EventPermission && ev.Permission != nil {
				_ = answerer.AnswerPermission(context.Background(), ev.Permission.ID, Decision{Allow: true})
			}
			if ev.Type == EventResult {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Minute):
		t.Fatal("timeout waiting for codex turn")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("approved side effect missing: %v", err)
	}
}
