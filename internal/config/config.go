package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"gopkg.in/yaml.v3"
)

// BaseDir returns the trebi home. Delegates to the home package (C3).
func BaseDir() string { return home.Base() }

type Config struct {
	Defaults Defaults     `yaml:"defaults"`
	Terminal string       `yaml:"terminal,omitempty"` // macOS: Terminal | iTerm | Ghostty
	Tools    ToolsConfig  `yaml:"tools,omitempty"`
	Notify   NotifyConfig `yaml:"notify,omitempty"`
}

// NotifyConfig: shell templates per channel (email → whatsapp → discord).
// Placeholders: {{kind}} {{tool}} {{task_id}} {{job}} {{workspace}} {{message}} {{hint}} {{resume}}.
// Kinds: auth_missing, waiting.
type NotifyConfig struct {
	Email        string `yaml:"email,omitempty"`
	WhatsApp     string `yaml:"whatsapp,omitempty"`
	Discord      string `yaml:"discord,omitempty"`
	WaitingAfter string `yaml:"waiting_after,omitempty"` // empty → 5m; "0"/"off" disables
	UsageWarnAt  int    `yaml:"usage_warn_at,omitempty"` // 0 → 80
}

// DefaultWaitingAfter is used when notify.waiting_after is unset.
const DefaultWaitingAfter = 5 * time.Minute

// WaitingAfterDuration parses WaitingAfter. ok=false means escalate is disabled.
func (n NotifyConfig) WaitingAfterDuration() (d time.Duration, ok bool) {
	s := strings.TrimSpace(n.WaitingAfter)
	if s == "" {
		return DefaultWaitingAfter, true
	}
	if s == "0" || strings.EqualFold(s, "off") {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return DefaultWaitingAfter, true
	}
	return d, true
}

// DefaultUsageWarnAt is used when notify.usage_warn_at is unset.
const DefaultUsageWarnAt = 80

// UsageWarnPercent is the harness usage percent that pages. 0 in YAML → 80.
func (n NotifyConfig) UsageWarnPercent() int {
	if n.UsageWarnAt <= 0 {
		return DefaultUsageWarnAt
	}
	return n.UsageWarnAt
}

// NotifyChannel is one shell template in the notifier chain.
type NotifyChannel struct {
	Name    string
	Command string
}

// Channels returns named non-empty notify commands in try order.
func (n NotifyConfig) Channels() []NotifyChannel {
	var out []NotifyChannel
	for _, ch := range []NotifyChannel{
		{Name: "email", Command: n.Email},
		{Name: "whatsapp", Command: n.WhatsApp},
		{Name: "discord", Command: n.Discord},
	} {
		ch.Command = strings.TrimSpace(ch.Command)
		if ch.Command != "" {
			out = append(out, ch)
		}
	}
	return out
}

// Chain returns non-empty notify commands in try order: email, whatsapp, discord.
func (n NotifyConfig) Chain() []string {
	chs := n.Channels()
	out := make([]string, len(chs))
	for i, ch := range chs {
		out[i] = ch.Command
	}
	return out
}

type Defaults struct {
	Command string          `yaml:"command"`
	Session SessionDefaults `yaml:"session,omitempty"`
}

// SessionDefaults are global reaper/session knobs when job frontmatter omits them.
type SessionDefaults struct {
	Timeout     string `yaml:"timeout,omitempty"`
	MaxDuration string `yaml:"max_duration,omitempty"`
	KeepAlive   string `yaml:"keep_alive,omitempty"`
	Nudge       string `yaml:"nudge,omitempty"`
	IfRunning   string `yaml:"if_running,omitempty"`
}

// ToolsConfig holds the binary pin policy and per-tool settings.
type ToolsConfig struct {
	// Fallback when pin is missing or ensure fails: "require" (fail) or "path" (PATH).
	Fallback string `yaml:"fallback,omitempty"`
	// Bridge is the global inheritance policy for harness resources.
	Bridge   *BridgeConfig `yaml:"bridge,omitempty"`
	Claude   *ToolPin      `yaml:"claude,omitempty"`
	Grok     *ToolPin      `yaml:"grok,omitempty"`
	Codex    *ToolPin      `yaml:"codex,omitempty"`
	OpenCode *ToolPin      `yaml:"opencode,omitempty"`
	Copilot  *ToolPin      `yaml:"copilot,omitempty"`
	Cursor   *ToolPin      `yaml:"cursor,omitempty"`
	Pi       *ToolPin      `yaml:"pi,omitempty"`
	// Extra allows future tools without schema churn.
	extra map[string]*ToolPin `yaml:"-"`
}

// BridgeConfig is inherit-by-default policy for harness capability resources.
type BridgeConfig struct {
	// Inherit defaults all resources for this scope (nil = true at global).
	Inherit *bool `yaml:"inherit,omitempty"`
	// Only, if non-empty, allowlists resource keys that may inherit.
	Only []string `yaml:"only,omitempty"`
	// Never force-disables listed resource keys.
	Never []string `yaml:"never,omitempty"`
	// Resources is per-key mode: inherit|true|link|false|off|none.
	Resources ResourceModeMap `yaml:"resources,omitempty"`
}

// ResourceModeMap maps resource keys to normalized mode strings.
type ResourceModeMap map[string]string

// UnmarshalYAML accepts bool or string modes for each resource key.
func (m *ResourceModeMap) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Kind == 0 {
		return nil
	}
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("want map of resource modes")
	}
	out := make(map[string]string)
	for i := 0; i+1 < len(value.Content); i += 2 {
		k := value.Content[i].Value
		vNode := value.Content[i+1]
		switch vNode.ShortTag() {
		case "!!bool":
			if vNode.Value == "true" {
				out[k] = "inherit"
			} else {
				out[k] = "false"
			}
		case "!!str", "!!int", "!!float":
			out[k] = strings.TrimSpace(vNode.Value)
		case "!!null":
			// treat as unset — skip
		default:
			out[k] = strings.TrimSpace(vNode.Value)
		}
	}
	*m = out
	return nil
}

// ToolPin is one harness binary pin + auth mode + optional bridge override.
type ToolPin struct {
	Version string `yaml:"version,omitempty"` // exact x.y.z after ensure target
	Channel string `yaml:"channel,omitempty"` // last channel target (stable/latest)
	// Auth: "shared" (default, bridge user login) or "separate" (jobi-only creds).
	Auth string `yaml:"auth,omitempty"`
	// Path overrides the store binary (skip download; still version-check if possible).
	Path string `yaml:"path,omitempty"`
	// Bridge overrides global tools.bridge for this tool.
	Bridge *BridgeConfig `yaml:"bridge,omitempty"`
}

// FirstClassTools returns the tools trebi pins / ensures (Gemini excluded).
func FirstClassTools() []string {
	return []string{"claude", "grok", "codex", "opencode", "copilot", "cursor", "pi"}
}

// KnownBridgeResources returns the valid bridge resource keys per tool (for Validate).
func KnownBridgeResources() map[string][]string {
	return map[string][]string{
		"claude": {
			"skills", "mcp", "plugins", "plugin_enablement", "user_memory",
			"settings_prefs", "settings_local", "commands", "agents", "rules",
			"keybindings", "status_line",
		},
		"grok": {
			"skills", "claude_skills_compat", "config_prefs", "marketplace", "plugins",
		},
		"codex": {
			"config_prefs", "skills", "prompts", "mcp",
		},
		"opencode": {
			"plugins", "config", "skills",
		},
		"copilot": {
			"settings", "skills",
		},
		"cursor": {
			"mcp", "rules", "skills", "cli_config_prefs",
		},
		"pi": {
			"skills", "prompts", "themes", "mcp", "models", "settings_prefs",
		},
	}
}

func (tc *ToolsConfig) Pin(name string) *ToolPin {
	if tc == nil {
		return nil
	}
	switch name {
	case "claude":
		return tc.Claude
	case "grok":
		return tc.Grok
	case "codex":
		return tc.Codex
	case "opencode":
		return tc.OpenCode
	case "copilot":
		return tc.Copilot
	case "cursor":
		return tc.Cursor
	case "pi":
		return tc.Pi
	default:
		if tc.extra != nil {
			return tc.extra[name]
		}
		return nil
	}
}

func (tc *ToolsConfig) SetPin(name string, pin *ToolPin) {
	if tc == nil {
		return
	}
	switch name {
	case "claude":
		tc.Claude = pin
	case "grok":
		tc.Grok = pin
	case "codex":
		tc.Codex = pin
	case "opencode":
		tc.OpenCode = pin
	case "copilot":
		tc.Copilot = pin
	case "cursor":
		tc.Cursor = pin
	case "pi":
		tc.Pi = pin
	default:
		if tc.extra == nil {
			tc.extra = map[string]*ToolPin{}
		}
		tc.extra[name] = pin
	}
}

// AuthMode returns "shared" or "separate" for name (default shared).
func (tc *ToolsConfig) AuthMode(name string) string {
	p := tc.Pin(name)
	if p != nil && p.Auth == "separate" {
		return "separate"
	}
	return "shared"
}

// ToolBridge returns tools.<name>.bridge if set.
func (tc *ToolsConfig) ToolBridge(name string) *BridgeConfig {
	p := tc.Pin(name)
	if p == nil {
		return nil
	}
	return p.Bridge
}

// BinDir is ~/.trebi/bin — versioned tool binaries live under BinDir/<tool>/<ver>/.
func BinDir() string {
	return filepath.Join(BaseDir(), "bin")
}

// LibexecDir is ~/.trebi/libexec — jobi helpers (not versioned harness CLIs).
func LibexecDir() string {
	return filepath.Join(BaseDir(), "libexec")
}

// HarnessDir is ~/.trebi/harness/<tool> — jobi-owned config/session tree for a harness.
func HarnessDir(tool string) string {
	return filepath.Join(BaseDir(), "harness", tool)
}

// ToolsRoot is ~/.trebi/bin — root of versioned harness binaries (alias of BinDir).
func ToolsRoot() string {
	return BinDir()
}

// ToolStoreDir is ~/.trebi/bin/<tool>.
func ToolStoreDir(tool string) string {
	return filepath.Join(BinDir(), tool)
}

// ToolVersionDir is ~/.trebi/bin/<tool>/<version>.
func ToolVersionDir(tool, version string) string {
	return filepath.Join(ToolStoreDir(tool), version)
}

// ToolCurrentLink is ~/.trebi/bin/<tool>/current (symlink to a version dir).
func ToolCurrentLink(tool string) string {
	return filepath.Join(ToolStoreDir(tool), "current")
}

// SecretsDir is ~/.trebi/secrets (mode 0700) for separate-mode env key files.
func SecretsDir() string {
	return filepath.Join(BaseDir(), "secrets")
}

// SecretEnvPath is ~/.trebi/secrets/<tool>.env.
func SecretEnvPath(tool string) string {
	return filepath.Join(SecretsDir(), tool+".env")
}

// ToolVersionsCachePath caches remote latest resolutions.
func ToolVersionsCachePath() string {
	return filepath.Join(BaseDir(), "cache", "tool-versions.json")
}

// EnsureDirs creates the home tree via home.EnsureDirs (which also fixes the
// secrets dir to 0700), then the config extras.
func EnsureDirs() error {
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(BaseDir(), "cache"), 0o755); err != nil {
		return fmt.Errorf("creating cache dir: %w", err)
	}
	return nil
}

// ConfigPath is ~/.trebi/config.yaml (or under BaseDir in tests).
func ConfigPath() string {
	return filepath.Join(BaseDir(), "config.yaml")
}

// Load reads and validates config.yaml. Missing file yields defaults (valid).
// Bad format or schema returns a path-prefixed error.
func Load() (*Config, error) {
	if err := EnsureDirs(); err != nil {
		return nil, err
	}

	cfg := defaultConfig()

	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			applyLoadDefaults(cfg)
			return cfg, nil
		}
		return nil, err
	}

	if err := decodeConfig(data, cfg); err != nil {
		return nil, err
	}
	applyLoadDefaults(cfg)
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ParseConfigBytes decodes and validates config YAML without touching the filesystem
// (aside from no I/O). Used by tests and validate-before-write helpers.
func ParseConfigBytes(data []byte) (*Config, error) {
	cfg := defaultConfig()
	if len(bytes.TrimSpace(data)) == 0 {
		applyLoadDefaults(cfg)
		return cfg, Validate(cfg)
	}
	if err := decodeConfig(data, cfg); err != nil {
		return nil, err
	}
	applyLoadDefaults(cfg)
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save validates then writes config.yaml.
func (c *Config) Save() error {
	if err := EnsureDirs(); err != nil {
		return err
	}
	applyLoadDefaults(c)
	if err := Validate(c); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(), data, 0644)
}

// ParsedSession is defaults.session with durations resolved.
type ParsedSession struct {
	Timeout     time.Duration
	MaxDuration time.Duration
	KeepAlive   time.Duration
	Nudge       time.Duration
	NudgeSet    bool // true when nudge key was present (incl. off)
	IfRunning   string
}

// ParseSessionDefaults resolves defaults.session strings (call after Validate).
func ParseSessionDefaults(s SessionDefaults) (ParsedSession, error) {
	var out ParsedSession
	var err error
	out.Timeout, err = parseDurationOrOff(s.Timeout)
	if err != nil {
		return out, fmt.Errorf("defaults.session.timeout: %w", err)
	}
	out.MaxDuration, err = parseDurationOrOff(s.MaxDuration)
	if err != nil {
		return out, fmt.Errorf("defaults.session.max_duration: %w", err)
	}
	out.KeepAlive, err = parseDurationOrOff(s.KeepAlive)
	if err != nil {
		return out, fmt.Errorf("defaults.session.keep_alive: %w", err)
	}
	if strings.TrimSpace(s.Nudge) != "" {
		out.NudgeSet = true
		if s.Nudge == "0" || strings.EqualFold(strings.TrimSpace(s.Nudge), "off") {
			out.Nudge = 0
		} else {
			out.Nudge, err = parseDurationOrOff(s.Nudge)
			if err != nil {
				return out, fmt.Errorf("defaults.session.nudge: %w", err)
			}
		}
	}
	ir := strings.ToLower(strings.TrimSpace(s.IfRunning))
	if ir != "" {
		out.IfRunning = ir
	}
	return out, nil
}

func parseDurationOrOff(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "0" || strings.EqualFold(s, "off") {
		return 0, nil
	}
	if !durationRe.MatchString(s) {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return d, nil
}

func defaultConfig() *Config {
	return &Config{
		Defaults: Defaults{Command: "claude"},
	}
}

func decodeConfig(data []byte, cfg *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		// EOF on empty is ok
		if err.Error() == "EOF" {
			return nil
		}
		return fmt.Errorf("%s: %w", filepath.Base(ConfigPath()), err)
	}
	return nil
}

func applyLoadDefaults(cfg *Config) {
	if cfg.Defaults.Command == "" {
		cfg.Defaults.Command = "claude"
	}
	if cfg.Terminal == "" {
		cfg.Terminal = "Terminal"
	}
	if cfg.Tools.Fallback == "" {
		cfg.Tools.Fallback = "path"
	}
}
