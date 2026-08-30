package tmux

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

type codexTool struct{}

func (codexTool) Name() string { return Codex }

func (codexTool) Match(command string) bool {
	return binaryMatch(command, Codex)
}

func (codexTool) PrepareLaunch(command string) LaunchPlan {
	// Soft-event only (agent-turn-complete → stop) so screen still drives busy.
	// Quote the TOML array so bash does not strip the quotes and Codex 0.149+
	// does not deserialize notify as a string.
	notifyBin := filepath.Join(config.LibexecDir(), "trebi-codex-notify")
	cmd := command
	if _, err := os.Stat(notifyBin); err == nil {
		cmd = AppendFlags(cmd, []string{
			"-c", fmt.Sprintf(`'notify=["%s"]'`, notifyBin),
		})
	}
	var cfg *config.Config
	if c, err := config.Load(); err == nil {
		cfg = c
	}
	dir := config.HarnessDir(Codex)
	_ = harness.EnsureDir(dir)
	_ = harness.EnsureBridges(Codex, cfg)
	return LaunchPlan{Command: cmd, Env: map[string]string{"CODEX_HOME": dir}}
}

func (codexTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	if sessionID == "" {
		return LaunchPlan{}, fmt.Errorf("no session ID captured for codex resume")
	}
	return LaunchPlan{Command: AppendFlags(command, []string{"--resume", sessionID}), SessionID: sessionID}, nil
}

// codexTrustDialog marks the first-run folder-trust dialog; its text starts
// with "> You are in ..." so it must be checked before the composer glyph.
const codexTrustDialog = "Do you trust the contents of this directory"

func (codexTool) IsReady(screen string) bool {
	// Composer glyph "› " (goldens: testdata/screens/codex/startup). Renders
	// while "model: loading" — typing is buffered, so that still counts.
	if strings.Contains(screen, codexTrustDialog) {
		return false
	}
	return strings.Contains(screen, "› ")
}

// HandleStartup: trust dialog has "1. Yes, continue" preselected.
func (codexTool) HandleStartup(screen string) []Action {
	if strings.Contains(screen, codexTrustDialog) {
		return []Action{{Key: "enter"}}
	}
	return nil
}
func (codexTool) PostReadyDelay() time.Duration { return defaultPostReadyDelay }

func (codexTool) TranscriptPath(home, workingDir, sessionID string) (string, error) {
	return "", ErrTranscriptUnsupported
}

func (codexTool) ParseTranscript(r io.Reader) ([]Turn, error) { return unsupportedParse(r) }

func (codexTool) Lifecycle() LifecycleAdapter { return codexLifecycle{} }

//go:embed lifecycle/jobi-codex-notify.sh
var codexNotifyScript []byte

type codexLifecycle struct{}

func (codexLifecycle) Install(trebiBase, binDir string) error {
	// Helpers live under libexec (not bin/) so they never collide with
	// versioned stores at bin/<tool>/<ver>/.
	libexec := filepath.Join(trebiBase, "libexec")
	if err := os.MkdirAll(libexec, 0755); err != nil {
		return err
	}
	path := filepath.Join(libexec, "trebi-codex-notify")
	if err := os.WriteFile(path, codexNotifyScript, 0755); err != nil {
		return fmt.Errorf("writing codex notify helper: %w", err)
	}
	// Remove legacy flat install under bin/ if present.
	legacy := filepath.Join(binDir, "trebi-codex-notify")
	_ = os.Remove(legacy)
	// Optional isolated home for strategy B (auth bridge prepared on demand).
	dir := filepath.Join(trebiBase, "harness", Codex)
	if err := harness.EnsureDir(dir); err != nil {
		return err
	}
	return harness.WriteSchemaStamp(dir)
}

func (codexLifecycle) Uninstall(trebiBase, binDir string) error {
	for _, path := range []string{
		filepath.Join(trebiBase, "libexec", "trebi-codex-notify"),
		filepath.Join(binDir, "trebi-codex-notify"), // legacy
	} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// BridgeCodexAuth symlinks user auth into harness CODEX_HOME when mode is shared.
func BridgeCodexAuth(mode string) error {
	userHome := harness.UserHome()
	if userHome == "" {
		return nil
	}
	src := filepath.Join(userHome, ".codex", "auth.json")
	dst := filepath.Join(config.HarnessDir(Codex), "auth.json")
	return harness.SymlinkAuth(src, dst, mode)
}
