package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

// ompTool is Oh My Pi, the pi fork from can1357. Trebi drives it on the wire
// through `omp acp`; the terminal policy is a fallback for the incident kill
// switch. omp keeps its login in its agent directory, so shared mode leaves
// PI_CODING_AGENT_DIR unset and separate mode points it at the Trebi folder.
type ompTool struct{ harnessHome }

func (ompTool) Name() string { return OMP }

func (ompTool) Match(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	base := filepath.Base(fields[0])
	return base == OMP || base == "omp.exe"
}

func (t ompTool) PrepareLaunch(command string) LaunchPlan {
	// omp names its own sessions; the server id arrives on the wire. The
	// fallback pane has no known session id, so it starts fresh.
	return LaunchPlan{Command: command, Env: t.launchEnv()}
}

func (t ompTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	if sessionID == "" {
		return LaunchPlan{}, fmt.Errorf("no session ID available for omp resume")
	}
	base := StripFlags(command, "--resume", "-r", "--continue", "-c")
	cmd := AppendFlags(base, []string{"--resume", sessionID})
	return LaunchPlan{Command: cmd, SessionID: sessionID, Env: t.launchEnv()}, nil
}

// IsReady follows the pi screen protocol: omp is a pi fork. The wire is the
// normal runtime; this only guards the kill-switch pane.
func (ompTool) IsReady(screen string) bool { return piTool{}.IsReady(screen) }

func (ompTool) HandleStartup(screen string) []Action { return piTool{}.HandleStartup(screen) }

func (ompTool) PostReadyDelay() time.Duration { return defaultPostReadyDelay }

func (ompTool) ParseTranscript(r io.Reader) ([]Turn, error) { return unsupportedParse(r) }

func (ompTool) TranscriptPath(home, workingDir, sessionID string) (string, error) {
	return "", ErrTranscriptUnsupported
}

func (ompTool) Lifecycle() LifecycleAdapter { return nil }

func (t ompTool) launchEnv() map[string]string {
	t.tool = OMP
	cfg, dir := t.prepare()
	mode := harness.AuthModeShared
	if cfg != nil {
		mode = harness.AuthModeFromConfig(cfg, OMP)
	}
	if mode == harness.AuthModeSeparate {
		// omp reads the variable as its agent directory. The Trebi folder is
		// that directory; the user signs in there once.
		return map[string]string{"PI_CODING_AGENT_DIR": dir}
	}
	// Shared mode uses ~/.omp directly, where the existing login lives.
	return nil
}

func (ompTool) AuthCapabilities() harness.AuthCaps {
	return harness.AuthCaps{
		AttachOnly: true,
		LocalCmd:   "omp  (sign in with a provider)",
		CredFile:   filepath.Join(harness.UserHome(), ".omp"),
	}
}

// ProbeAuth asks the vendor for the configured provider models. An empty list
// means no provider is logged in. The probe never writes to the home.
func (ompTool) ProbeAuth(ctx context.Context) harness.AuthStatus {
	st := authBase(OMP)
	bin := lookPathTool(OMP)
	if bin == "" {
		st.Status = harness.AuthMissing
		st.Hint = "omp is not installed"
		return st
	}
	env := []string{}
	if st.Mode == harness.AuthModeSeparate {
		env = append(env, "PI_CODING_AGENT_DIR="+config.HarnessDir(OMP))
	}
	out, err := runVendor(ctx, bin, []string{"models", "--json"}, env)
	var payload struct {
		Models []json.RawMessage `json:"models"`
	}
	if json.Unmarshal([]byte(out), &payload) == nil {
		if len(payload.Models) > 0 {
			st.Status = harness.AuthOK
			st.Detail = fmt.Sprintf("%d models", len(payload.Models))
			return st
		}
		st.Status = harness.AuthMissing
		st.Hint = "Sign in to Oh My Pi on this computer, then choose Use existing login"
		return st
	}
	st.Status = harness.AuthUnknown
	st.Detail = strings.TrimSpace(out)
	if err != nil && st.Detail == "" {
		st.Detail = err.Error()
	}
	return st
}
