//go:build live_cli

package cli

import (
	"context"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/tmux"
)

// TestLiveCursorAuth: the shared cursor-agent login under its isolation env.
func TestLiveCursorAuth(t *testing.T) {
	if !liveEnabled(t, "cursor") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "cursor")
}

// TestLiveCursorACPTurn: `cursor-agent acp` initialize → session/new →
// session/prompt → end_turn, with streamed agent_message_chunk text.
func TestLiveCursorACPTurn(t *testing.T) {
	if !liveEnabled(t, "cursor") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "cursor")

	cmd, env := livePlan(t, "cursor")
	req := StartRequest{
		Tool:        "cursor",
		BaseCommand: cmd,
		WorkingDir:  t.TempDir(),
		Env:         env,
		Persistent:  true,
	}
	s, err := acpStart(context.Background(), req, acpCursor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	if s.ID() == "" {
		t.Fatal("no ACP session id")
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
}

// TestLiveCursorACPLoad: session/load reuses a stored session id.
func TestLiveCursorACPLoad(t *testing.T) {
	if !liveEnabled(t, "cursor") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "cursor")

	cmd, env := livePlan(t, "cursor")
	work := t.TempDir()
	req := StartRequest{Tool: "cursor", BaseCommand: cmd, WorkingDir: work, Env: env, Persistent: true}
	s, err := acpStart(context.Background(), req, acpCursor)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Prompt(context.Background(), Prompt{Text: "Reply with exactly OK.", Kind: "job"}); err != nil {
		t.Fatal(err)
	}
	_, res := assistantText(t, s, 4*time.Minute)
	if res.Result == nil || res.Result.IsError {
		t.Fatalf("first result: %+v", res.Result)
	}
	firstID := s.ID()
	_ = s.Close(context.Background())
	if firstID == "" {
		t.Fatal("no session id")
	}

	tool, _ := tmux.ByName("cursor")
	plan, err := tool.PrepareResume(liveBin("cursor"), firstID)
	if err != nil {
		t.Fatal(err)
	}
	resumeReq := StartRequest{
		Tool: "cursor", BaseCommand: plan.Command, WorkingDir: work,
		Env: plan.Env, SessionID: firstID, Persistent: true,
	}
	s2, err := acpStart(context.Background(), resumeReq, acpCursor)
	if err != nil {
		t.Fatalf("cursor ACP load: %v", err)
	}
	defer func() { _ = s2.Close(context.Background()) }()
	if s2.ID() != firstID {
		t.Fatalf("loaded session id = %q, want %q", s2.ID(), firstID)
	}
}
