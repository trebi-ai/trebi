package tmux

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

type piTool struct{ harnessHome }

func (piTool) Name() string { return Pi }

func (piTool) Match(command string) bool {
	// Exact binary basename "pi" only — avoid matching paths like "pixel".
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	base := fields[0]
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".exe")
	return base == Pi
}

func (t piTool) PrepareLaunch(command string) LaunchPlan {
	// --session-id <uuid> creates a known-upfront project session id.
	cmd, sid := injectSessionIDFlag(command, "--session-id")
	cmd = injectPiFlags(cmd)
	return planWithSession(cmd, sid, t.launchEnv())
}

func (t piTool) PrepareResume(command, sessionID string) (LaunchPlan, error) {
	if sessionID == "" {
		return LaunchPlan{}, fmt.Errorf("no session ID available for pi resume")
	}
	base := StripFlags(command, "--session-id", "--session", "--resume", "-r", "--continue", "-c")
	base = injectPiFlags(base)
	// --session <id> reopens an existing session by id (partial UUID ok).
	cmd := AppendFlags(base, []string{"--session", sessionID})
	return LaunchPlan{Command: cmd, SessionID: sessionID, Env: t.launchEnv()}, nil
}

// injectPiFlags adds --approve so unattended launches skip project-trust prompts.
func injectPiFlags(command string) string {
	if !strings.Contains(command, "--approve") && !strings.Contains(command, " -a ") &&
		!strings.HasSuffix(command, " -a") && !strings.Contains(command, "--no-approve") {
		command = AppendFlags(command, []string{"--approve"})
	}
	return command
}

func (piTool) IsReady(screen string) bool {
	if IsAuthBlocker(screen) || IsLimitBlocker(screen) {
		return false
	}
	// Project trust dialog blocks paste (goldens: startup/01-trust-folder).
	if strings.Contains(screen, "Trust project folder?") {
		return false
	}
	// Footer: cost/usage chrome + model • thinking-level (goldens: startup/02-ready).
	if !strings.Contains(screen, "•") {
		return false
	}
	return strings.Contains(screen, "$") || strings.Contains(screen, "%/")
}

func (piTool) HandleStartup(screen string) []Action {
	// Preselected option is "→ Trust" (or "Trust"); Enter accepts.
	if strings.Contains(screen, "Trust project folder?") {
		return []Action{{Key: "enter"}}
	}
	return nil
}

func (piTool) PostReadyDelay() time.Duration { return defaultPostReadyDelay }

func (piTool) ParseTranscript(r io.Reader) ([]Turn, error) { return unsupportedParse(r) }

func (piTool) TranscriptPath(home, workingDir, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("no session id recorded")
	}
	// Sessions: <agentDir>/sessions/<cwd-slug>/<timestamp>_<sessionId>.jsonl
	bases := []string{}
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		bases = append(bases, d)
	}
	bases = append(bases, config.HarnessDir(Pi))
	if home != "" {
		bases = append(bases, filepath.Join(home, ".pi", "agent"))
	}
	for _, base := range bases {
		sessions := filepath.Join(base, "sessions")
		if found := findPiSessionFile(sessions, sessionID); found != "" {
			return found, nil
		}
	}
	return "", ErrTranscriptUnsupported
}

func findPiSessionFile(sessionsDir, sessionID string) string {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return ""
	}
	suffix := "_" + sessionID + ".jsonl"
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(sessionsDir, e.Name())
		files, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if strings.HasSuffix(name, suffix) || name == sessionID+".jsonl" {
				return filepath.Join(sub, name)
			}
		}
	}
	return ""
}

func (piTool) Lifecycle() LifecycleAdapter {
	return newPiLifecycle()
}

// ClassifyScreen: busy while "Working..."; idle when model footer shows.
func (piTool) ClassifyScreen(screen string) ScreenClass {
	return classifyChrome(screen,
		[]string{"Working...", "Working"},
		[]string{"•"},
	)
}

func (t piTool) launchEnv() map[string]string {
	t.tool = Pi
	cfg, dir := t.prepare()
	mode := harness.AuthModeShared
	if cfg != nil {
		mode = harness.AuthModeFromConfig(cfg, Pi)
	}
	_ = t.bridgeAuth(filepath.Join(".pi", "agent", "auth.json"), mode)
	// MCP config after bridges: trebi owns harness mcp.json (merge user + host discovery).
	inheritMCP := true
	if cfg != nil {
		inheritMCP = config.ResolveResourceInherit(Pi, "mcp", cfg)
	}
	_ = ensurePiMcpConfig(dir, inheritMCP)
	return map[string]string{
		// Relocates ~/.pi/agent → harness (settings, auth, sessions, extensions).
		"PI_CODING_AGENT_DIR":   dir,
		"PI_SKIP_VERSION_CHECK": "1",
		"PI_TELEMETRY":          "0",
	}
}

//go:embed lifecycle/pi-trebi-lifecycle.ts
var piLifecycleTS []byte

// Minimal trebi-seeded settings for isolated PI_CODING_AGENT_DIR.
const piSettingsJSON = `{
  "lastChangelogVersion": "0.84.1",
  "enableInstallTelemetry": false,
  "quietStartup": true,
  "defaultProjectTrust": "always",
  "packages": [
    "npm:pi-mcp-adapter"
  ]
}
`

const piExtensionFileName = "trebi-lifecycle.ts"

// Default pi-mcp-adapter package (npm: source in settings; version pin for install).
const (
	piMcpAdapterSource  = "npm:pi-mcp-adapter"
	piMcpAdapterNpmSpec = "pi-mcp-adapter@2.22.0"
)

// piMcpHostImports returns host MCP configs that pi-mcp-adapter can import for discovery.
func piMcpHostImports() []string {
	return []string{
		"cursor", "claude-code", "claude-desktop", "opencode", "vscode", "codex", "windsurf",
	}
}

type piLifecycle struct {
	log *slog.Logger
}

func newPiLifecycle() piLifecycle {
	return piLifecycle{log: slog.Default().With("source", "pi_lifecycle")}
}

func (p piLifecycle) Install(trebiBase, binDir string) error {
	dir := filepath.Join(trebiBase, "harness", Pi)
	return p.materialize(dir)
}

func (piLifecycle) Uninstall(trebiBase, binDir string) error {
	dir := filepath.Join(trebiBase, "harness", Pi)
	path := filepath.Join(dir, "extensions", piExtensionFileName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing pi lifecycle extension: %w", err)
	}
	return nil
}

func (p piLifecycle) materialize(dir string) error {
	if err := harness.EnsureDir(dir); err != nil {
		return err
	}
	extDir := filepath.Join(dir, "extensions")
	if err := harness.EnsureDir(extDir); err != nil {
		return err
	}
	if err := harness.WriteFileIfChanged(
		filepath.Join(extDir, piExtensionFileName),
		piLifecycleTS,
		0644,
	); err != nil {
		return fmt.Errorf("writing pi lifecycle extension: %w", err)
	}
	// Seed settings only when missing so bridged/user prefs survive.
	cfgPath := filepath.Join(dir, "settings.json")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := harness.WriteFileIfChanged(cfgPath, []byte(piSettingsJSON), 0644); err != nil {
			return err
		}
	}
	// Always ensure packages entry + pi-mcp-adapter on disk (best-effort install).
	if err := ensurePiMcpAdapter(dir); err != nil {
		p.log.Warn("pi-mcp-adapter ensure", "error", err)
	}
	return harness.WriteSchemaStamp(dir)
}

// ensurePiMcpAdapter installs the package under agentDir/npm and pins it in settings.
func ensurePiMcpAdapter(agentDir string) error {
	if err := installPiMcpAdapterPkg(agentDir); err != nil {
		// Still register the package source so a later install / pi update can fill it.
		_ = ensurePiPackageInSettings(filepath.Join(agentDir, "settings.json"))
		return err
	}
	return ensurePiPackageInSettings(filepath.Join(agentDir, "settings.json"))
}

func piMcpAdapterInstalled(agentDir string) bool {
	// package.json present means npm install succeeded.
	p := filepath.Join(agentDir, "npm", "node_modules", "pi-mcp-adapter", "package.json")
	_, err := os.Stat(p)
	return err == nil
}

func installPiMcpAdapterPkg(agentDir string) error {
	if piMcpAdapterInstalled(agentDir) {
		return nil
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		return fmt.Errorf("npm not on PATH: %w", err)
	}
	npmDir := filepath.Join(agentDir, "npm")
	if err := os.MkdirAll(npmDir, 0755); err != nil {
		return err
	}
	// Match `pi install npm:pi-mcp-adapter` layout: <agentDir>/npm/node_modules/...
	cmd := exec.Command(npm, "install", "--prefix", npmDir, "--no-fund", "--no-audit",
		"--loglevel=error", piMcpAdapterNpmSpec)
	cmd.Env = append(os.Environ(), "npm_config_update_notifier=false")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("npm install %s: %w (%s)", piMcpAdapterNpmSpec, err, truncateRunes(string(out), 240))
	}
	if !piMcpAdapterInstalled(agentDir) {
		return fmt.Errorf("pi-mcp-adapter missing after npm install under %s", npmDir)
	}
	return nil
}

// ensurePiPackageInSettings merges packages: ["npm:pi-mcp-adapter"] into settings.json.
func ensurePiPackageInSettings(settingsPath string) error {
	m := map[string]any{}
	if b, err := os.ReadFile(settingsPath); err == nil {
		_ = json.Unmarshal(b, &m)
		if m == nil {
			m = map[string]any{}
		}
	}
	pkgs, _ := m["packages"].([]any)
	found := false
	for _, p := range pkgs {
		s, _ := p.(string)
		// Accept bare, versioned, or object form source.
		if s == piMcpAdapterSource || strings.HasPrefix(s, piMcpAdapterSource+"@") {
			found = true
			break
		}
		if obj, ok := p.(map[string]any); ok {
			if src, _ := obj["source"].(string); src == piMcpAdapterSource ||
				strings.HasPrefix(src, piMcpAdapterSource+"@") {
				found = true
				break
			}
		}
	}
	if !found {
		pkgs = append(pkgs, piMcpAdapterSource)
		m["packages"] = pkgs
	}
	// Defaults that make unattended harness runs quiet.
	if _, ok := m["quietStartup"]; !ok {
		m["quietStartup"] = true
	}
	if _, ok := m["defaultProjectTrust"]; !ok {
		m["defaultProjectTrust"] = "always"
	}
	if _, ok := m["enableInstallTelemetry"]; !ok {
		m["enableInstallTelemetry"] = false
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return harness.WriteFileIfChanged(settingsPath, b, 0644)
}

// ensurePiMcpConfig writes harness mcp.json so pi-mcp-adapter sees user + host servers.
// When inherit is true, merges ~/.pi/agent/mcp.json servers. Always enables host discovery
// imports so Cursor/Claude/Codex MCP configs are visible without a pure symlink overwrite.
func ensurePiMcpConfig(agentDir string, inherit bool) error {
	path := filepath.Join(agentDir, "mcp.json")
	// Never rewrite a live symlink (legacy bridge); leave user ownership.
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil
	}

	out := map[string]any{
		"settings": map[string]any{
			// Opt in to host-config fallback so Cursor/Claude/etc. servers are detected.
			"hostConfigDiscovery": "on",
		},
		"imports":    piMcpHostImports(),
		"mcpServers": map[string]any{},
	}

	// Merge any existing harness file first (preserve prior servers).
	mergeMCPFile(out, path)

	if inherit {
		if home := harness.UserHome(); home != "" {
			mergeMCPFile(out, filepath.Join(home, ".pi", "agent", "mcp.json"))
		}
	}

	// Ensure discovery knobs survived merge.
	settings, _ := out["settings"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
		out["settings"] = settings
	}
	if _, ok := settings["hostConfigDiscovery"]; !ok {
		settings["hostConfigDiscovery"] = "on"
	}
	out["imports"] = mergeStringList(out["imports"], piMcpHostImports())

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return harness.WriteFileIfChanged(path, b, 0644)
}

func mergeMCPFile(dst map[string]any, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var src map[string]any
	if json.Unmarshal(b, &src) != nil || src == nil {
		return
	}
	// mcpServers
	dstServers, _ := dst["mcpServers"].(map[string]any)
	if dstServers == nil {
		dstServers = map[string]any{}
		dst["mcpServers"] = dstServers
	}
	if srcServers, ok := src["mcpServers"].(map[string]any); ok {
		for k, v := range srcServers {
			if _, exists := dstServers[k]; !exists {
				dstServers[k] = v
			}
		}
	}
	// imports
	dst["imports"] = mergeStringList(dst["imports"], src["imports"])
	// settings (shallow; do not clobber hostConfigDiscovery once set)
	if srcSettings, ok := src["settings"].(map[string]any); ok {
		dstSettings, _ := dst["settings"].(map[string]any)
		if dstSettings == nil {
			dstSettings = map[string]any{}
			dst["settings"] = dstSettings
		}
		for k, v := range srcSettings {
			if _, exists := dstSettings[k]; !exists {
				dstSettings[k] = v
			}
		}
	}
}

func mergeStringList(existing any, add any) []string {
	seen := map[string]bool{}
	var out []string
	appendOne := func(v any) {
		switch t := v.(type) {
		case string:
			if t != "" && !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		case []any:
			for _, x := range t {
				if s, ok := x.(string); ok && s != "" && !seen[s] {
					seen[s] = true
					out = append(out, s)
				}
			}
		case []string:
			for _, s := range t {
				if s != "" && !seen[s] {
					seen[s] = true
					out = append(out, s)
				}
			}
		}
	}
	appendOne(existing)
	appendOne(add)
	return out
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if n <= 0 {
		return ""
	}
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
