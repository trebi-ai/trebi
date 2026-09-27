package tmux

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/harness"
)

// opencodeTool is the policy of the OpenCode family: 1.x and the 2.x beta are
// the same terminal application, so one struct carries both. name is the
// harness (OpenCode or OpenCode2) and the vendor binary.
type opencodeTool struct {
	harnessHome
	name string
}

func (t opencodeTool) Name() string { return t.name }

func (t opencodeTool) Match(command string) bool {
	return binaryMatch(command, t.name)
}

func (t opencodeTool) PrepareLaunch(command string) LaunchPlan {
	// Session ID is assigned by OpenCode; capture via lifecycle plugin.
	return LaunchPlan{Command: command, Env: t.launchEnv()}
}

// InjectMCP sets OPENCODE_CONFIG_CONTENT to the fragment that carries the
// injected servers. OpenCode merges it with the user's own config.
func (t opencodeTool) InjectMCP(plan *LaunchPlan, servers []agentwire.MCPServer, _ RunFiles) error {
	content, err := agentwire.OpenCodeConfigContent(servers)
	if err != nil {
		return err
	}
	if plan.Env == nil {
		plan.Env = map[string]string{}
	}
	plan.Env["OPENCODE_CONFIG_CONTENT"] = content
	return nil
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

// ParseOpenCodeMessages maps the `GET /session/:id/message` payload (an array
// of {info, parts}) into the shared Turn shape. The CLI runtime uses it for
// the transcript fallback when the SSE stream was missed (Part H).
func ParseOpenCodeMessages(data []byte) ([]Turn, error) {
	var msgs []struct {
		Info struct {
			Role string `json:"role"`
			Time struct {
				Created int64 `json:"created"`
			} `json:"time"`
		} `json:"info"`
		Parts []struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Tool  string `json:"tool"`
			State struct {
				Status string          `json:"status"`
				Input  json.RawMessage `json:"input"`
				Output string          `json:"output"`
			} `json:"state"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("opencode transcript: %w", err)
	}
	turns := []Turn{}
	for _, m := range msgs {
		turn := Turn{At: time.UnixMilli(m.Info.Time.Created)}
		var texts []string
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			case "tool":
				input := ""
				if len(p.State.Input) > 0 {
					input = string(p.State.Input)
				}
				turn.Tools = append(turn.Tools, ToolCall{
					Name: p.Tool, Input: input, Output: p.State.Output,
				})
			}
		}
		turn.Text = strings.Join(texts, "\n")
		switch {
		case m.Info.Role == "user":
			turn.Role = "user"
		case turn.Text == "" && len(turn.Tools) > 0:
			turn.Role = "tools"
		default:
			turn.Role = "assistant"
		}
		if turn.Text == "" && len(turn.Tools) == 0 {
			continue
		}
		turns = append(turns, turn)
	}
	return turns, nil
}

// Lifecycle: the session-id plugin is written against the 1.x plugin API.
// The 2.x builds tested on 2026-09-22 never load a plugin declared in an
// injected config dir, so opencode2 runs screen-only (EventMode default)
// until the V2 plugin API is proven. The cli wire carries the id anyway.
func (t opencodeTool) Lifecycle() LifecycleAdapter {
	if t.name == OpenCode2 {
		return nil
	}
	return opencodeLifecycle{}
}

func (t opencodeTool) launchEnv() map[string]string {
	t.tool = t.name
	_, dir := t.prepare()
	if t.name == OpenCode2 {
		// V2 reads the operator's own config directories. The 1.x variables are
		// not honored in the builds tested on 2026-09-22: `debug config` lists
		// only the global directories, and a plugin declared in an injected
		// directory never loads. So the trebi home stays out of a V2 launch and
		// the bridge assets written under it do not reach V2 — the operator's
		// own skills and config do. Re-check when the V2 config surface settles.
		return nil
	}
	cfgPath := filepath.Join(dir, "opencode.json")
	return map[string]string{
		// OPENCODE_CONFIG_DIR adds the trebi home as a config directory, so
		// OpenCode scans the skills and plugins the bridge injects there and
		// reads opencode.json from it. It adds to the user's config: the real
		// XDG config dir stays in the list. It does not move auth — auth.json
		// lives under XDG data (Global.Path.data), which trebi leaves alone.
		"OPENCODE_CONFIG_DIR": dir,
		// Kept for OpenCode builds without OPENCODE_CONFIG_DIR: the same file
		// then merges as an explicit config.
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
