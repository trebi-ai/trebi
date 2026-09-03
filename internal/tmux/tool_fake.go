package tmux

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

// fakeTool drives the `fake` binary (cmd/fake). It exists so eval trials can
// exercise login and logout without touching a vendor account.
type fakeTool struct{}

func (fakeTool) Name() string { return Fake }

func (fakeTool) Match(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	return filepath.Base(fields[0]) == Fake
}

func (fakeTool) PrepareLaunch(command string) LaunchPlan { return LaunchPlan{Command: command} }

func (fakeTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	return LaunchPlan{}, ErrResumeUnsupported
}

func (fakeTool) IsReady(screen string) bool           { return true }
func (fakeTool) HandleStartup(screen string) []Action { return nil }
func (fakeTool) PostReadyDelay() time.Duration        { return 0 }

func (fakeTool) TranscriptPath(home, workingDir, sessionID string) (string, error) {
	return "", ErrTranscriptUnsupported
}

func (fakeTool) ParseTranscript(r io.Reader) ([]Turn, error) { return unsupportedParse(r) }

func (fakeTool) Lifecycle() LifecycleAdapter { return nil }

func (fakeTool) AuthCapabilities() harness.AuthCaps {
	return harness.AuthCaps{
		LoginModes: []string{harness.LoginRelay},
		LocalCmd:   "fake login",
		CredFile:   fakeCredFile(),
	}
}

func (fakeTool) ProbeAuth(context.Context) harness.AuthStatus {
	st := authBase(Fake)
	if fileExists(fakeCredFile()) {
		st.Status = harness.AuthOK
		return st
	}
	st.Status = harness.AuthMissing
	st.Hint = loginHint(Fake)
	return st
}

func fakeCredFile() string { return filepath.Join(config.HarnessDir(Fake), "auth.json") }
