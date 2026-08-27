package tmux

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

type claudeTool struct{}

func (claudeTool) Name() string { return Claude }

func (claudeTool) Match(command string) bool {
	// Full command: paths and multi-arg "claude --dangerously-skip-permissions".
	return containsToken(command, Claude)
}

func (t claudeTool) PrepareLaunch(command string) LaunchPlan {
	cmd, sid := injectSessionIDFlag(command, "--session-id")
	cmd = injectClaudeSettings(cmd)
	return planWithSession(cmd, sid, claudeLaunchEnv())
}

func (claudeTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	cmd, err := resumeWithFlag(command, sessionID, []string{"--session-id"}, "--resume", Claude)
	if err != nil {
		return LaunchPlan{}, err
	}
	cmd = injectClaudeSettings(cmd)
	return LaunchPlan{Command: cmd, SessionID: sessionID, Env: claudeLaunchEnv()}, nil
}

// BridgeClaudeAuth symlinks user credentials into harness CLAUDE_CONFIG_DIR when shared.
func BridgeClaudeAuth(mode string) error {
	userHome := harness.UserHome()
	if userHome == "" {
		return nil
	}
	src := filepath.Join(userHome, ".claude", ".credentials.json")
	dst := filepath.Join(config.HarnessDir(Claude), ".credentials.json")
	return harness.SymlinkAuth(src, dst, mode)
}

func (claudeTool) IsReady(screen string) bool {
	return claudeReady(screen)
}

func (claudeTool) HandleStartup(screen string) []Action {
	var actions []Action
	// First-run theme picker (fresh CLAUDE_CONFIG_DIR). Preselect is usually
	// "Dark mode"; Enter accepts. Without this, readiness times out.
	if strings.Contains(screen, "Choose the text style that looks best") ||
		(strings.Contains(screen, "Let's get started") &&
			strings.Contains(screen, "Dark mode") &&
			strings.Contains(screen, "Light mode")) {
		actions = append(actions, Action{Key: "enter"})
	}
	// Trust this folder?
	if strings.Contains(screen, "Is this a project you created or one you trust?") &&
		strings.Contains(screen, "Yes, I trust this folder") {
		actions = append(actions, Action{Key: "enter"})
	}
	// Bypass permissions confirmation ("2" = yes).
	if strings.Contains(screen, "Bypass Permissions mode") &&
		strings.Contains(screen, "Yes, I accept") {
		actions = append(actions, Action{Text: "2"})
	}
	// Resume full session as-is (option 2), not summary.
	if strings.Contains(screen, "Resume full session as-is") &&
		strings.Contains(screen, "Resume from summary") {
		actions = append(actions, Action{Text: "2"})
	}
	// Claude-in-Chrome first-use: headless jobs always pick "keep browser tools off" (#2).
	if strings.Contains(screen, "Claude in Chrome extension detected") ||
		(strings.Contains(screen, "use my browser") &&
			strings.Contains(screen, "keep browser tools off")) {
		actions = append(actions, Action{Text: "2"})
	}
	return actions
}

func (claudeTool) PostReadyDelay() time.Duration { return claudePostReadyDelay }

func (claudeTool) TranscriptPath(home, workingDir, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("no session id recorded")
	}
	slug := ClaudeProjectDir(workingDir)
	rel := filepath.Join("projects", slug, sessionID+".jsonl")

	if configDir := os.Getenv("CLAUDE_CONFIG_DIR"); configDir != "" {
		return filepath.Join(configDir, rel), nil
	}

	// Prefer harness, fall back to user ~/.claude (legacy).
	harnessPath := filepath.Join(config.HarnessDir(Claude), rel)
	if fileExistsNotDir(harnessPath) {
		return harnessPath, nil
	}
	if home == "" {
		return "", fmt.Errorf("home directory required")
	}
	userPath := filepath.Join(home, ".claude", rel)
	if fileExistsNotDir(userPath) {
		return userPath, nil
	}
	return harnessPath, nil
}

func (claudeTool) Lifecycle() LifecycleAdapter { return claudeLifecycle{} }

// ClaudeProjectDir maps a working dir to ~/.claude/projects/<slug>
// (non-alphanumerics → "-"; trailing slash stripped).
func ClaudeProjectDir(wd string) string {
	wd = strings.TrimRight(wd, "/")
	var b strings.Builder
	b.Grow(len(wd))
	for _, r := range wd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func claudeLaunchEnv() map[string]string {
	dir := config.HarnessDir(Claude)
	_ = (claudeLifecycle{}).materialize(dir)
	mode := harness.AuthModeShared
	var cfg *config.Config
	if c, err := config.Load(); err == nil {
		cfg = c
		mode = harness.AuthModeFromConfig(cfg, Claude)
	}
	_ = BridgeClaudeAuth(mode)
	_ = seedClaudeOnboarding(dir)
	// Resource inheritance (skills, MCP, user_memory, …) — soft on failure.
	_ = harness.EnsureBridges(Claude, cfg)
	return map[string]string{
		"CLAUDE_CONFIG_DIR":   dir,
		"DISABLE_AUTOUPDATER": "1",
		"DISABLE_UPDATES":     "1",
	}
}

// seedClaudeOnboarding marks harness past first-run (skip theme wizard) and disables Claude-in-Chrome.
// Best-effort; merges a few keys from ~/.claude.json when present.
func seedClaudeOnboarding(configDir string) error {
	if configDir == "" {
		return nil
	}
	path := filepath.Join(configDir, ".claude.json")
	m := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
		if m == nil {
			m = map[string]any{}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
			var u map[string]any
			if json.Unmarshal(b, &u) == nil {
				for _, k := range []string{
					"lastOnboardingVersion",
					"theme", "userTheme", "preferredTheme",
				} {
					if _, ok := m[k]; !ok {
						if v, ok := u[k]; ok {
							m[k] = v
						}
					}
				}
			}
		}
	}
	m["hasCompletedOnboarding"] = true
	if _, ok := m["lastOnboardingVersion"]; !ok {
		m["lastOnboardingVersion"] = "1.0.9"
	}
	// Unattended jobs: never default to browser tools (interactive opt-in dialog).
	m["claudeInChromeDefaultEnabled"] = false
	m["hasCompletedClaudeInChromeOnboarding"] = true

	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o600)
}

// injectClaudeSettings appends --settings with jobi lifecycle hooks unless already present.
func injectClaudeSettings(command string) string {
	if strings.Contains(command, "--settings") {
		return command
	}
	// Prefer settings file under harness when available; else inline JSON.
	settingsPath := filepath.Join(config.HarnessDir(Claude), "jobi-settings.json")
	if st, err := os.Stat(settingsPath); err == nil && !st.IsDir() {
		return AppendFlags(command, []string{"--settings", settingsPath})
	}
	return AppendFlags(command, []string{"--settings", claudeSettingsJSON})
}

func fileExistsNotDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// claudeReady needs ❯ plus the status bar (" min | "); trust dialogs have ❯ only.
// Login / limit chrome also matches both — exclude via blockers.
func claudeReady(screen string) bool {
	if IsAuthBlocker(screen) || IsLimitBlocker(screen) {
		return false
	}
	return strings.Contains(screen, "❯") && strings.Contains(screen, " min | ")
}

// claudeSettingsJSON is the Claude lifecycle-hooks --settings payload.
// waiting = real human attention only; idle_prompt Notification omitted (that's idle).
//
//go:embed lifecycle/claude-jobi-settings.json
var claudeSettingsJSON string

type claudeLifecycle struct{}

func (c claudeLifecycle) Install(trebiBase, binDir string) error {
	// Remove legacy PATH shim (was a file at bin/claude; version store needs a dir).
	legacyShim := filepath.Join(binDir, Claude)
	if fi, err := os.Lstat(legacyShim); err == nil && !fi.IsDir() {
		_ = os.Remove(legacyShim)
	}
	dir := filepath.Join(trebiBase, "harness", Claude)
	return c.materialize(dir)
}

func (claudeLifecycle) Uninstall(trebiBase, binDir string) error {
	// Remove legacy shim if still present.
	legacyShim := filepath.Join(binDir, Claude)
	if fi, err := os.Lstat(legacyShim); err == nil && !fi.IsDir() {
		_ = os.Remove(legacyShim)
	}
	// Leave harness/claude sessions; only drop jobi-settings.
	settings := filepath.Join(trebiBase, "harness", Claude, "jobi-settings.json")
	if err := os.Remove(settings); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (claudeLifecycle) materialize(dir string) error {
	if err := harness.EnsureDir(dir); err != nil {
		return err
	}
	// Trailing newline for stable WriteFileIfChanged / editors.
	payload := claudeSettingsJSON
	if !strings.HasSuffix(payload, "\n") {
		payload += "\n"
	}
	if err := harness.WriteFileIfChanged(
		filepath.Join(dir, "jobi-settings.json"),
		[]byte(payload),
		0644,
	); err != nil {
		return fmt.Errorf("writing claude settings: %w", err)
	}
	return harness.WriteSchemaStamp(dir)
}
