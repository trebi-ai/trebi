package tmux

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/store"
)

type claudeTool struct{ harnessHome }

func (claudeTool) Name() string { return Claude }

func (claudeTool) Match(command string) bool {
	// Full command: paths and multi-arg "claude --dangerously-skip-permissions".
	return strings.Contains(command, Claude)
}

func (t claudeTool) PrepareLaunch(command string) LaunchPlan {
	cmd, sid := injectSessionIDFlag(command, "--session-id")
	cmd = injectClaudeSettings(cmd)
	return planWithSession(cmd, sid, t.launchEnv())
}

func (t claudeTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	cmd, err := resumeWithFlag(command, sessionID, []string{"--session-id"}, "--resume", Claude)
	if err != nil {
		return LaunchPlan{}, err
	}
	cmd = injectClaudeSettings(cmd)
	return LaunchPlan{Command: cmd, SessionID: sessionID, Env: t.launchEnv()}, nil
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
	// Trust this folder? Numbered → "1"; unnumbered with No highlighted → down+enter.
	if strings.Contains(screen, "Is this a project you created or one you trust?") &&
		strings.Contains(screen, "Yes, I trust this folder") {
		actions = append(actions, TrustYesActions(screen)...)
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

func (claudeTool) ParseTranscript(r io.Reader) ([]Turn, error) {
	return parseClaudeTranscript(r)
}

func (claudeTool) TranscriptPath(home, workingDir, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("no session id recorded")
	}
	name := sessionID + ".jsonl"
	slug := ClaudeProjectDir(workingDir)
	var roots []string
	if configDir := os.Getenv("CLAUDE_CONFIG_DIR"); configDir != "" {
		roots = append(roots, configDir)
	}
	roots = append(roots, config.HarnessDir(Claude))
	if home != "" {
		roots = append(roots, filepath.Join(home, ".claude"))
	}
	var first string
	for _, root := range roots {
		if root == "" {
			continue
		}
		candidates := []string{filepath.Join(root, "projects", name)}
		if slug != "" {
			candidates = append([]string{filepath.Join(root, "projects", slug, name)}, candidates...)
		}
		for _, p := range candidates {
			if first == "" {
				first = p
			}
			if fileExistsNotDir(p) {
				return p, nil
			}
		}
		if found := findClaudeSessionJSONL(filepath.Join(root, "projects"), name); found != "" {
			return found, nil
		}
	}
	if first == "" {
		return "", fmt.Errorf("transcript not found")
	}
	return first, nil
}

func findClaudeSessionJSONL(projects, name string) string {
	var found string
	_ = filepath.WalkDir(projects, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Name() == name {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func (claudeTool) Lifecycle() LifecycleAdapter { return claudeLifecycle{} }

func (t claudeTool) launchEnv() map[string]string {
	t.tool = Claude
	cfg, dir := t.prepare()
	mode := harness.AuthModeShared
	if cfg != nil {
		mode = harness.AuthModeFromConfig(cfg, Claude)
	}
	_ = EnsureSharedAuth(Claude, mode)
	_ = seedClaudeOnboarding(dir)
	env := claudeIsolationEnvMap(dir, mode)
	env["DISABLE_AUTOUPDATER"] = "1"
	env["DISABLE_UPDATES"] = "1"
	return env
}

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

// seedClaudeOnboarding marks harness past first-run (skip theme wizard) and disables Claude-in-Chrome.
// Best-effort; merges a few keys from ~/.claude.json when present.
// Differs from eval.seedIsolatedClaudeHome, which writes a fixed
// {"hasCompletedOnboarding": true} into a scratch home so trials stay
// isolated from operator config.
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

// injectClaudeSettings appends --settings with trebi lifecycle hooks unless already present.
func injectClaudeSettings(command string) string {
	if strings.Contains(command, "--settings") {
		return command
	}
	// Prefer settings file under harness when available; else inline JSON.
	settingsPath := filepath.Join(config.HarnessDir(Claude), "trebi-settings.json")
	if st, err := os.Stat(settingsPath); err == nil && !st.IsDir() {
		return AppendFlags(command, []string{"--settings", settingsPath})
	}
	// The inline payload is multi-line JSON; shell-quote it so both the pane
	// export and argv splitting keep it as one value.
	return AppendFlags(command, []string{"--settings", shellSingleQuote(claudeSettingsJSON)})
}

// shellSingleQuote quotes a value for a POSIX shell / argv field splitter.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func fileExistsNotDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// claudeReady needs ❯ plus idle footer. Trust/bypass dialogs also have ❯.
// Login / limit chrome can match both — exclude via blockers.
func claudeReady(screen string) bool {
	if IsAuthBlocker(screen) || IsLimitBlocker(screen) {
		return false
	}
	if strings.Contains(screen, "Yes, I trust this folder") ||
		strings.Contains(screen, "Bypass Permissions mode") {
		return false
	}
	if !strings.Contains(screen, "❯") {
		return false
	}
	return strings.Contains(screen, " min | ") ||
		strings.Contains(screen, "auto mode on")
}

// claudeSettingsJSON is the Claude lifecycle-hooks --settings payload.
// waiting = real human attention only; idle_prompt Notification omitted (that's idle).
//
//go:embed lifecycle/claude-trebi-settings.json
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
	// Leave harness/claude sessions; only drop trebi-settings.
	settings := filepath.Join(trebiBase, "harness", Claude, "trebi-settings.json")
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
		filepath.Join(dir, "trebi-settings.json"),
		[]byte(payload),
		0644,
	); err != nil {
		return fmt.Errorf("writing claude settings: %w", err)
	}
	return harness.WriteSchemaStamp(dir)
}

type claudeLine struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type claudeBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
}

func parseClaudeTranscript(r io.Reader) ([]Turn, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var turns []Turn
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		t, ok := parseClaudeLine(line)
		if ok {
			turns = append(turns, t)
		}
	}
	if turns == nil {
		turns = []Turn{}
	}
	return turns, sc.Err()
}

func parseClaudeLine(line string) (Turn, bool) {
	var raw claudeLine
	if json.Unmarshal([]byte(line), &raw) != nil {
		return Turn{}, false
	}
	switch raw.Type {
	case "user", "assistant":
	default:
		return Turn{}, false
	}
	msg := decodeClaudeMessage(raw.Message)
	turn := Turn{Role: raw.Type, At: parseClaudeTime(raw.Timestamp)}
	text, tools := claudeContent(msg.Content)
	turn.Text = text
	turn.Tools = tools
	if turn.Role == "user" && len(tools) > 0 && text == "" {
		turn.Role = "tools"
	}
	if turn.Text == "" && len(turn.Tools) == 0 {
		return Turn{}, false
	}
	return turn, true
}

func decodeClaudeMessage(raw json.RawMessage) claudeMessage {
	var msg claudeMessage
	if len(raw) == 0 {
		return msg
	}
	_ = json.Unmarshal(raw, &msg)
	return msg
}

func claudeContent(raw json.RawMessage) (string, []ToolCall) {
	if len(raw) == 0 {
		return "", nil
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return asString, nil
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", nil
	}
	var texts []string
	var tools []ToolCall
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "tool_use":
			tools = append(tools, ToolCall{Name: b.Name, Input: string(b.Input)})
		case "tool_result":
			tools = append(tools, ToolCall{Name: "tool_result", Output: claudeToolResult(b.Content)})
		}
	}
	return strings.Join(texts, "\n"), tools
}

func claudeToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func parseClaudeTime(s string) time.Time { return store.ParseTS(s) }
