package tmux

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Gemini is legacy screen-only (consumer CLI sunset 2026-06-18 → Antigravity).
// No harness isolation or event-mode work.

type geminiTool struct{}

func (geminiTool) Name() string { return Gemini }

func (geminiTool) Match(command string) bool {
	return binaryMatch(command, Gemini)
}

func (geminiTool) PrepareLaunch(command string) LaunchPlan {
	return LaunchPlan{Command: command}
}

func (geminiTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	if sessionID == "" {
		return LaunchPlan{}, fmt.Errorf("no session ID captured for gemini resume")
	}
	return LaunchPlan{Command: AppendFlags(command, []string{"--resume", sessionID}), SessionID: sessionID}, nil
}

func (geminiTool) IsReady(screen string) bool {
	// Input-box placeholder; absent during boot and the "Waiting for auth..."
	// box (goldens: testdata/screens/gemini/startup).
	return strings.Contains(screen, "Type your message or @path/to/file")
}

func (geminiTool) HandleStartup(screen string) []Action {
	// 0.51+ trust dialogs — "files in this folder" then "folders being added
	// to this workspace"; option 1 (trust) is always preselected
	// (goldens: testdata/screens/gemini/startup-trust).
	if strings.Contains(screen, "Do you trust the") {
		return []Action{{Text: "1"}}
	}
	return nil
}
func (geminiTool) PostReadyDelay() time.Duration { return defaultPostReadyDelay }

func (geminiTool) TranscriptPath(home, workingDir, sessionID string) (string, error) {
	return "", ErrTranscriptUnsupported
}

func (geminiTool) ParseTranscript(r io.Reader) ([]Turn, error) { return unsupportedParse(r) }

func (geminiTool) Lifecycle() LifecycleAdapter { return nil }
