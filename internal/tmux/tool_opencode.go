package tmux

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

type opencodeTool struct{}

func (opencodeTool) Name() string { return OpenCode }

func (opencodeTool) Match(command string) bool {
	return binaryMatch(command, OpenCode)
}

func (opencodeTool) PrepareLaunch(command string) LaunchPlan {
	// Session ID is assigned by OpenCode; capture via lifecycle plugin.
	return LaunchPlan{Command: command, Env: opencodeLaunchEnv()}
}

func (opencodeTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	if sessionID == "" {
		return LaunchPlan{}, fmt.Errorf("no session ID available for opencode resume")
	}
	base := StripFlags(command, "--session", "-s")
	cmd := AppendFlags(base, []string{"--session", sessionID})
	return LaunchPlan{Command: cmd, SessionID: sessionID, Env: opencodeLaunchEnv()}, nil
}

func opencodeLaunchEnv() map[string]string {
	dir := config.HarnessDir(OpenCode)
	_ = (opencodeLifecycle{}).materialize(dir)
	var cfg *config.Config
	if c, err := config.Load(); err == nil {
		cfg = c
	}
	_ = harness.EnsureBridges(OpenCode, cfg)
	cfgPath := filepath.Join(dir, "opencode.json")
	return map[string]string{
		// Merge plugin with user config; does not relocate auth (XDG data).
		"OPENCODE_CONFIG": cfgPath,
	}
}

// Session id: never on the TUI; the lifecycle plugin delivers it via hook
// payloads and the daemon persists it (service/ipc.go).

func (opencodeTool) IsReady(screen string) bool {
	// Focused empty input: "┃" gutter with the idle placeholder — safe to
	// paste (goldens: testdata/screens/opencode/startup).
	return strings.Contains(screen, "┃") && strings.Contains(screen, "Ask anything")
}

func (opencodeTool) HandleStartup(screen string) []Action { return nil }

func (opencodeTool) PostReadyDelay() time.Duration { return defaultPostReadyDelay }

func (opencodeTool) TranscriptPath(home, workingDir, sessionID string) (string, error) {
	// Prefer opencode export; no stable single-file path in all versions.
	return "", ErrTranscriptUnsupported
}

func (opencodeTool) Lifecycle() LifecycleAdapter { return opencodeLifecycle{} }

//go:embed lifecycle/opencode-jobi.js
var opencodePluginJS []byte

const opencodePluginFileName = "jobi-lifecycle.js"

type opencodeLifecycle struct{}

func (o opencodeLifecycle) Install(trebiBase, binDir string) error {
	dir := filepath.Join(trebiBase, "harness", OpenCode)
	if err := o.materialize(dir); err != nil {
		return err
	}
	// Remove legacy user-config pollution.
	if home := harness.UserHome(); home != "" {
		legacy := filepath.Join(home, ".config", "opencode", "plugins", opencodePluginFileName)
		_ = os.Remove(legacy)
	}
	return nil
}

func (opencodeLifecycle) Uninstall(trebiBase, binDir string) error {
	dir := filepath.Join(trebiBase, "harness", OpenCode)
	path := filepath.Join(dir, "plugins", opencodePluginFileName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing opencode plugin: %w", err)
	}
	_ = os.Remove(filepath.Join(dir, "opencode.json"))
	// Legacy user path.
	if home := harness.UserHome(); home != "" {
		legacy := filepath.Join(home, ".config", "opencode", "plugins", opencodePluginFileName)
		if err := os.Remove(legacy); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing legacy opencode plugin: %w", err)
		}
	}
	return nil
}

func (opencodeLifecycle) materialize(dir string) error {
	pluginDir := filepath.Join(dir, "plugins")
	if err := harness.EnsureDir(pluginDir); err != nil {
		return err
	}
	pluginPath := filepath.Join(pluginDir, opencodePluginFileName)
	if err := harness.WriteFileIfChanged(pluginPath, opencodePluginJS, 0644); err != nil {
		return fmt.Errorf("writing opencode plugin: %w", err)
	}
	// file:// absolute URL so OpenCode resolves the plugin.
	abs, err := filepath.Abs(pluginPath)
	if err != nil {
		abs = pluginPath
	}
	// JSON with file URL (space-safe paths encoded lightly).
	fileURL := "file://" + filepath.ToSlash(abs)
	ocJSON := fmt.Sprintf(`{
  "$schema": "https://opencode.ai/config.json",
  "plugin": [%q]
}
`, fileURL)
	if err := harness.WriteFileIfChanged(filepath.Join(dir, "opencode.json"), []byte(ocJSON), 0644); err != nil {
		return err
	}
	return harness.WriteSchemaStamp(dir)
}
