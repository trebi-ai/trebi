package visor_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/hometest"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/visor"
)

// TestMain lets the test binary run as the holder of a visor session.
func TestMain(m *testing.M) {
	if code, ok := visor.Hold(os.Args); ok {
		os.Exit(code)
	}
	hometest.Main(m)
}

// The wire holder ends after each turn, and the session is suspended. The
// events of the next turn still arrive on the same Session.
func TestSessionFollowsAfterSuspend(t *testing.T) {
	t.Setenv(agentwire.FakeScriptEnv, `while IFS= read -r line; do
echo '{"type":"init","session_id":"s1"}'
echo '{"type":"result","subtype":"success","text":"done"}'
done`)
	home, err := os.MkdirTemp("", "tv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) }) // a test folder
	o := agentvisor.Options{Home: home, Client: "test"}
	visor.Configure(&o)
	v, err := agentvisor.New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close(context.Background())
	s, err := visor.Start(t.Context(), v, agentvisor.StartSpec{Kind: agentvisor.KindWire, Harness: "fake", WorkingDir: t.TempDir(), Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	waitResult(t, s)
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := v.Session(t.Context(), s.ID())
		if err != nil {
			t.Fatal(err)
		}
		if got.State == agentvisor.StateSuspended {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state %s, want suspended", got.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := s.Send(t.Context(), agentwire.Prompt{Text: "two"}); err != nil {
		t.Fatal(err)
	}
	waitResult(t, s)
}

func waitResult(t *testing.T, s *visor.Session) {
	t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatalf("the events ended at %s", s.End())
			}
			if e.Type == agentwire.EventResult {
				return
			}
		case <-timeout:
			t.Fatal("no result")
		}
	}
}
