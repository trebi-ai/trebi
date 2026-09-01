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

type grokTool struct{ harnessHome }

func (grokTool) Name() string { return Grok }

func (grokTool) Match(command string) bool {
	// Official Grok Build binary is "grok". Avoid matching "grok-cli".
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	base := fields[0]
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return base == Grok || base == "grok.exe"
}

func (t grokTool) PrepareLaunch(command string) LaunchPlan {
	// Grok Build: -s / --session-id names a new session with a UUID you supply.
	cmd, sid := injectSessionIDFlag(command, "--session-id")
	return planWithSession(cmd, sid, t.launchEnv())
}

func (t grokTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	cmd, err := resumeWithFlag(command, sessionID, []string{"--session-id", "-s"}, "--resume", Grok)
	if err != nil {
		return LaunchPlan{}, err
	}
	return LaunchPlan{Command: cmd, SessionID: sessionID, Env: t.launchEnv()}, nil
}

func (grokTool) IsReady(screen string) bool {
	// The resume picker overlay still shows the input-box edge — never paste
	// into it (goldens: testdata/screens/grok/resume-picker).
	if strings.Contains(screen, "─ Resume session ─") {
		return false
	}
	// Project-dir / permission pickers do not block here on purpose: some
	// permission UIs still show "│ ❯" and accept typing.
	// Bordered input row "│ ❯" accepts typing as soon as it renders
	// (goldens: testdata/screens/grok/startup).
	return strings.Contains(screen, "│ ❯")
}

// ClassifyScreen provides a safety net when hooks fail (pre-promotion).
// Uses bottom chrome only (same spoof resistance as other classifiers).
func (grokTool) ClassifyScreen(screen string) ScreenClass {
	return classifyChrome(screen,
		[]string{"Thinking", "Running", "esc to interrupt"},
		[]string{"│ ❯"},
	)
}

// HandleStartup: no first-run dialogs observed in captures under shared home.
// Under a fresh GROK_HOME, folder trust may appear — accept preselected option.
func (grokTool) HandleStartup(screen string) []Action {
	if strings.Contains(screen, "trust") && strings.Contains(screen, "Yes") {
		return []Action{{Key: "enter"}}
	}
	return nil
}

func (grokTool) PostReadyDelay() time.Duration { return defaultPostReadyDelay }

func (grokTool) ParseTranscript(r io.Reader) ([]Turn, error) { return unsupportedParse(r) }

func (grokTool) TranscriptPath(home, workingDir, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("no session id recorded")
	}
	// Prefer isolated GROK_HOME when set.
	if gh := os.Getenv("GROK_HOME"); gh != "" {
		return filepath.Join(gh, "sessions", sessionID+".jsonl"), nil
	}
	if home == "" {
		return "", fmt.Errorf("home directory required")
	}
	return filepath.Join(home, ".grok", "sessions", sessionID+".jsonl"), nil
}

func (grokTool) Lifecycle() LifecycleAdapter { return grokLifecycle{} }

func (t grokTool) launchEnv() map[string]string {
	t.tool = Grok
	cfg, dir := t.prepare()
	mode := harness.AuthModeShared
	if cfg != nil {
		mode = harness.AuthModeFromConfig(cfg, Grok)
	}
	_ = t.bridgeAuth(filepath.Join(".grok", "auth.json"), mode)
	return map[string]string{"GROK_HOME": dir}
}

//go:embed lifecycle/grok-jobi.json
var grokHooksJSON []byte

// Minimal jobi-seeded config for isolated GROK_HOME.
const grokConfigTOML = `# jobi-seeded defaults for isolated GROK_HOME (do not copy interactive config)
[compat.claude]
hooks = false

[telemetry]
trace_upload = false

[harness]
disable_codebase_upload = true
`

const grokHookFileName = "jobi.json"

type grokLifecycle struct{}

func (g grokLifecycle) Install(trebiBase, binDir string) error {
	dir := filepath.Join(trebiBase, "harness", Grok)
	if err := g.materialize(dir); err != nil {
		return err
	}
	// Remove legacy global hooks pollution if present.
	if home := harness.UserHome(); home != "" {
		legacy := filepath.Join(home, ".grok", "hooks", grokHookFileName)
		_ = os.Remove(legacy)
	}
	return nil
}

func (grokLifecycle) Uninstall(trebiBase, binDir string) error {
	// Remove jobi harness hooks (leave auth symlink / sessions).
	dir := filepath.Join(trebiBase, "harness", Grok)
	path := filepath.Join(dir, "hooks", grokHookFileName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing grok hooks: %w", err)
	}
	// Legacy user path.
	if home := harness.UserHome(); home != "" {
		legacy := filepath.Join(home, ".grok", "hooks", grokHookFileName)
		if err := os.Remove(legacy); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing legacy grok hooks: %w", err)
		}
	}
	return nil
}

func (grokLifecycle) materialize(dir string) error {
	if err := harness.EnsureDir(filepath.Join(dir, "hooks")); err != nil {
		return err
	}
	if err := harness.WriteFileIfChanged(filepath.Join(dir, "hooks", grokHookFileName), grokHooksJSON, 0644); err != nil {
		return fmt.Errorf("writing grok hooks: %w", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := harness.WriteFileIfChanged(cfgPath, []byte(grokConfigTOML), 0644); err != nil {
			return err
		}
	}
	return harness.WriteSchemaStamp(dir)
}
