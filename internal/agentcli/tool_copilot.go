package agentcli

import (
	"fmt"
	"io"
	"strings"
	"time"
)

type copilotTool struct{}

func (copilotTool) Name() string { return Copilot }

func (copilotTool) Match(command string) bool {
	// "copilot", "copilot-cli", paths containing copilot.
	return binaryMatch(command, Copilot)
}

func (copilotTool) PrepareLaunch(command string) LaunchPlan {
	// Screen-only; no COPILOT_HOME isolation until event mode exists.
	return LaunchPlan{Command: command}
}

func (copilotTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	if sessionID == "" {
		return LaunchPlan{}, fmt.Errorf("no session ID captured for copilot resume")
	}
	return LaunchPlan{Command: AppendFlags(command, []string{"--resume", sessionID}), SessionID: sessionID}, nil
}

func (copilotTool) IsReady(screen string) bool {
	// Composer ("❯" through 1.0.27, "┃" gutter from 1.0.71) plus the "? help"
	// footer; startup dialogs show a selection cursor but never the footer
	// (goldens: testdata/screens/copilot/startup, keybinding-setup).
	if !strings.Contains(screen, "? help") {
		return false
	}
	return strings.Contains(screen, "❯") || strings.Contains(screen, "┃")
}

// HandleStartup: folder-trust dialog has "1. Yes" preselected. The VS Code
// multi-line keybinding offer is declined — trebi must not write into the
// user's terminal config (goldens: testdata/screens/copilot/keybinding-setup).
func (copilotTool) HandleStartup(screen string) []Action {
	if strings.Contains(screen, "Do you trust the files in this folder") {
		return []Action{{Text: "1"}}
	}
	if strings.Contains(screen, "Would you like to add this key binding to your terminal configuration?") {
		return []Action{{Key: "escape"}}
	}
	return nil
}
func (copilotTool) PostReadyDelay() time.Duration { return defaultPostReadyDelay }

func (copilotTool) TranscriptPath(home, workingDir, sessionID string) (string, error) {
	return "", ErrTranscriptUnsupported
}

func (copilotTool) ParseTranscript(r io.Reader) ([]Turn, error) { return unsupportedParse(r) }

func (copilotTool) Lifecycle() LifecycleAdapter { return nil }
