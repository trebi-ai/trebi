//go:build live_cli

package cli

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/tmux"
)

// liveTier is the L.2 acceptance tier: real binaries, real shared auth, no
// mocks. scripts/test.live.sh sets TREBI_LIVE_HARNESSES.

// liveEnabled gates one harness: unset selection skips with the hint;
// an explicitly named harness that is absent fails; "all" skips absent ones.
func liveEnabled(t *testing.T, name string) bool {
	t.Helper()
	sel := strings.TrimSpace(os.Getenv("TREBI_LIVE_HARNESSES"))
	if sel == "" {
		t.Skipf("live tier disabled; run scripts/test.live.sh %s or set TREBI_LIVE_HARNESSES=%s", name, name)
		return false
	}
	selected := sel == "all"
	for _, n := range strings.Split(sel, ",") {
		if strings.TrimSpace(n) == name {
			selected = true
		}
	}
	if !selected {
		t.Skipf("harness %s not selected", name)
		return false
	}
	if liveBin(name) == "" {
		if sel == "all" {
			t.Skipf("%s binary not installed", name)
			return false
		}
		t.Fatalf("%s requested but its binary is not installed", name)
	}
	return true
}

// liveBin resolves the harness binary the way the daemon does.
func liveBin(name string) string {
	bin := name
	if name == "cursor" {
		bin = "cursor-agent"
	}
	if p, err := exec.LookPath(bin); err == nil {
		return p
	}
	return ""
}

// liveHome points TREBI_HOME at a temp dir so transcripts/config written by
// the test never touch the real daemon home. Shared auth still resolves
// through the user home / Keychain.
func liveHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
}

// liveAuthGate checks auth exactly as the daemon does and asserts shared
// mode is really shared.
func liveAuthGate(t *testing.T, name string) {
	t.Helper()
	tool, ok := tmux.ByName(name)
	if !ok {
		t.Fatalf("unknown harness %s", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st := tool.ProbeAuth(ctx)
	skipNoAuth := os.Getenv("TREBI_LIVE_SKIP_NOAUTH") == "1"
	if !st.OK() {
		if skipNoAuth {
			t.Skipf("%s not authenticated: %s", name, st.Hint)
		}
		t.Fatalf("%s not authenticated: status=%s hint=%s", name, st.Status, st.Hint)
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY"} {
		if os.Getenv(key) != "" {
			t.Fatalf("%s must be tested with the shared login, but %s is set", name, key)
		}
	}
}

// livePlan returns the prepared command + isolation env the daemon would use.
// It installs lifecycle assets first, exactly like the daemon boot does.
func livePlan(t *testing.T, name string) (string, map[string]string) {
	t.Helper()
	tool, _ := tmux.ByName(name)
	if life := tool.Lifecycle(); life != nil {
		if err := life.Install(config.BaseDir(), config.BinDir()); err != nil {
			t.Fatalf("install lifecycle %s: %v", name, err)
		}
	}
	plan := tool.PrepareLaunch(liveBin(name))
	return plan.Command, plan.Env
}

// liveStartRequest builds a StartRequest with the harness policy applied.
func liveStartRequest(t *testing.T, name string, oneShot bool, prompt string) StartRequest {
	t.Helper()
	cmd, env := livePlan(t, name)
	req := StartRequest{
		Tool:           name,
		BaseCommand:    cmd,
		WorkingDir:     t.TempDir(),
		Env:            env,
		OneShot:        oneShot,
		OneShotPrompt:  prompt,
		Persistent:     !oneShot,
		PermissionMode: "",
	}
	if oneShot {
		req.SessionID = newLiveSessionID()
	}
	return req
}

func newLiveSessionID() string {
	// The vendor requires a UUID.
	return uuid.NewString()
}

// waitEvent reads until a matching event or timeout.
func waitEvent(t *testing.T, s Session, typ EventType, timeout time.Duration) Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("session ended before %s event", typ)
			}
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s", typ)
		}
	}
}

// waitResult reads until the terminal result event.
func waitResult(t *testing.T, s Session, timeout time.Duration) Event {
	t.Helper()
	return waitEvent(t, s, EventResult, timeout)
}

// assistantText collects assistant text seen while waiting for result.
func assistantText(t *testing.T, s Session, timeout time.Duration) (string, Event) {
	t.Helper()
	var b strings.Builder
	lastErr := ""
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("session ended before a result (text=%q err=%q)", b.String(), lastErr)
			}
			switch ev.Type {
			case EventAssistant:
				b.WriteString(ev.Text)
			case EventError:
				lastErr = ev.Error
			case EventResult:
				return b.String(), ev
			}
		case <-deadline:
			t.Fatalf("timeout waiting for result (text=%q err=%q)", b.String(), lastErr)
		}
	}
}

func pongMatch(text string) bool {
	up := strings.ToUpper(text)
	up = strings.NewReplacer(".", "", "!", "", ",", "", "`", "").Replace(up)
	return strings.Contains(up, "PONG")
}

var _ = context.Background
