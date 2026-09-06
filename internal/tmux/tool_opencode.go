package tmux

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/harness"
)

type opencodeTool struct{ harnessHome }

func (opencodeTool) Name() string { return OpenCode }

func (opencodeTool) Match(command string) bool {
	return binaryMatch(command, OpenCode)
}

func (t opencodeTool) PrepareLaunch(command string) LaunchPlan {
	// Session ID is assigned by OpenCode; capture via lifecycle plugin.
	return LaunchPlan{Command: command, Env: t.launchEnv()}
}

func (t opencodeTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	if sessionID == "" {
		return LaunchPlan{}, fmt.Errorf("no session ID available for opencode resume")
	}
	base := StripFlags(command, "--session", "-s")
	cmd := AppendFlags(base, []string{"--session", sessionID})
	return LaunchPlan{Command: cmd, SessionID: sessionID, Env: t.launchEnv()}, nil
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

func (opencodeTool) ParseTranscript(r io.Reader) ([]Turn, error) { return unsupportedParse(r) }

func (opencodeTool) Lifecycle() LifecycleAdapter { return opencodeLifecycle{} }

func (t opencodeTool) launchEnv() map[string]string {
	t.tool = OpenCode
	_, dir := t.prepare()
	cfgPath := filepath.Join(dir, "opencode.json")
	return map[string]string{
		// Merge plugin with user config; does not relocate auth (XDG data).
		"OPENCODE_CONFIG": cfgPath,
	}
}

//go:embed lifecycle/opencode-trebi.js
var opencodePluginJS []byte

const opencodePluginFileName = "trebi-lifecycle.js"

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
