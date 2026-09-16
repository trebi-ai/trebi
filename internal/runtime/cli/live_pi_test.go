//go:build live_cli

package cli

import (
	"context"
	"testing"
	"time"
)

// TestLivePiAuth: pi's own provider check under the harness env.
func TestLivePiAuth(t *testing.T) {
	if !liveEnabled(t, "pi") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "pi")
}

// TestLivePiTurn: `pi --mode rpc` prompt → agent_end, with message_end text.
// Set TREBI_LIVE_PI_MODEL to pick a provider/model (e.g. "anthropic/sonnet").
func TestLivePiTurn(t *testing.T) {
	if !liveEnabled(t, "pi") {
		return
	}
	liveHome(t)
	liveAuthGate(t, "pi")

	cmd, env := livePlan(t, "pi")
	req := StartRequest{
		Tool:        "pi",
		BaseCommand: cmd,
		WorkingDir:  t.TempDir(),
		Env:         env,
		Persistent:  true,
	}
	s, err := piStart(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
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
