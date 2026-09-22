package config

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"gopkg.in/yaml.v3"
)

// BaseDir returns the trebi home. Delegates to the home package (C3).
func BaseDir() string { return home.Base() }

type Config struct {
	Defaults   Defaults         `yaml:"defaults"`
	API        APIConfig        `yaml:"api,omitempty"`
	Terminal   string           `yaml:"terminal,omitempty"` // macOS: Terminal | iTerm | Ghostty
	Tools      ToolsConfig      `yaml:"tools,omitempty"`
	Harness    HarnessConfig    `yaml:"harness,omitempty"`
	Notify     NotifyConfig     `yaml:"notify,omitempty"`
	Files      FilesConfig      `yaml:"files,omitempty"`
	Workspaces WorkspacesConfig `yaml:"workspaces,omitempty"`
	Assistant  AssistantConfig  `yaml:"assistant,omitempty"`
	Email      EmailConfig      `yaml:"email,omitempty"`
	Cloud      CloudConfig      `yaml:"cloud,omitempty"`
}

// HarnessConfig is the vendor run-runtime policy (plan 2026-09-16 Part J).
// The default runtime is the subprocess CLI wire; `runtime: tui` is the
// incident-only kill switch back to tmux panes.
type HarnessConfig struct {
	// Runtime is "" (default cli) or "tui".
	Runtime string `yaml:"runtime,omitempty"`
}

// APIConfig is the HTTP bind for trebid. Default is loopback + port 7433;
// an explicit api.port of 0 asks for an ephemeral port.
type APIConfig struct {
	Host string `yaml:"host,omitempty"`
	Port *int   `yaml:"port,omitempty"`
	TLS  APITLS `yaml:"tls,omitempty"`
}

// APITLS is the optional PEM pair for HTTPS. Both paths must be set together.
// Insecure allows plaintext on a non-loopback bind (Tailscale/LAN).
type APITLS struct {
	Cert     string `yaml:"cert,omitempty"`
	Key      string `yaml:"key,omitempty"`
	Insecure bool   `yaml:"insecure,omitempty"`
}

const (
	DefaultAPIHost = "127.0.0.1"
	DefaultAPIPort = 7433
)

// HostOrDefault is 127.0.0.1 when host is unset.
func (a APIConfig) HostOrDefault() string {
	h := strings.TrimSpace(a.Host)
	if h == "" {
		return DefaultAPIHost
	}
	return h
}

// PortOrDefault is 7433 when api.port is unset. An explicit 0 stays
// ephemeral (the port is written to api.json after bind).
func (a APIConfig) PortOrDefault() int {
	if a.Port == nil {
		return DefaultAPIPort
	}
	return *a.Port
}

// ListenAddr is host:port for net.Listen (IPv6 hosts are bracketed).
func (a APIConfig) ListenAddr() string {
	return net.JoinHostPort(a.HostOrDefault(), strconv.Itoa(a.PortOrDefault()))
}

// TLSEnabled reports whether both cert and key paths are set.
func (a APIConfig) TLSEnabled() bool {
	return strings.TrimSpace(a.TLS.Cert) != "" && strings.TrimSpace(a.TLS.Key) != ""
}

// Loopback reports whether the bind host is loopback (or "localhost").
func (a APIConfig) Loopback() bool {
	h := a.HostOrDefault()
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// PublicBind reports an unspecified or globally routed address (not RFC1918,
// Tailscale CGNAT, or link-local). Doctor errors tls.insecure here.
func (a APIConfig) PublicBind() bool {
	h := a.HostOrDefault()
	ip := net.ParseIP(h)
	if ip == nil {
		return !strings.EqualFold(h, "localhost")
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return false
	}
	return ip.IsUnspecified() || ip.IsGlobalUnicast()
}

// TLSFiles expands cert/key paths. Both empty is OK; one without the other errors.
func (a APIConfig) TLSFiles() (cert, key string, err error) {
	cert = expandTilde(strings.TrimSpace(a.TLS.Cert))
	key = expandTilde(strings.TrimSpace(a.TLS.Key))
	if cert == "" && key == "" {
		return "", "", nil
	}
	if cert == "" || key == "" {
		return "", "", fmt.Errorf("api.tls.cert and api.tls.key must both be set")
	}
	return cert, key, nil
}

// BindGuard refuses a non-loopback listen without TLS or tls.insecure (B28).
func BindGuard(listenAddr string, a APIConfig) error {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		host = listenAddr
	}
	g := a
	g.Host = host
	if g.Loopback() {
		return nil
	}
	if g.TLSEnabled() {
		return nil
	}
	if g.TLS.Insecure {
		return nil
	}
	return fmt.Errorf("non-loopback bind %s requires api.tls.cert/api.tls.key or api.tls.insecure=true", listenAddr)
}

// WorkspacesConfig is the pathless-create root for remote workspace bootstrap.
type WorkspacesConfig struct {
	Root string `yaml:"root,omitempty"`
}

// DefaultWorkspacesRoot is used when workspaces.root is unset.
const DefaultWorkspacesRoot = "~/trebi"

// RootOrDefault expands workspaces.root (default ~/trebi). Empty if ~ cannot resolve.
func (w WorkspacesConfig) RootOrDefault() string {
	raw := strings.TrimSpace(w.Root)
	if raw == "" {
		raw = DefaultWorkspacesRoot
	}
	return expandTilde(raw)
}

// FilesConfig is the workspace file-plane cap.
type FilesConfig struct {
	MaxBytes int64 `yaml:"max_bytes,omitempty"`
}

// DefaultFilesMaxBytes is used when files.max_bytes is unset (10 MiB).
const DefaultFilesMaxBytes int64 = 10 << 20

// MaxBytesOrDefault is the write/read cap injected at construction.
func (f FilesConfig) MaxBytesOrDefault() int64 {
	if f.MaxBytes <= 0 {
		return DefaultFilesMaxBytes
	}
	return f.MaxBytes
}

// NotifyConfig: shell templates per channel (email → whatsapp → discord).
// Values are passed as TREBI_ALERT_* environment variables. Existing
// {{kind}} {{tool}} {{task_id}} {{job}} {{workspace}} {{message}} {{hint}}
// {{body}} {{resume}} {{connector}} placeholders are rewritten to
// "$TREBI_ALERT_*" so untrusted text is never interpolated into the shell string.
type NotifyConfig struct {
	Email         string `yaml:"email,omitempty"`
	WhatsApp      string `yaml:"whatsapp,omitempty"`
	Discord       string `yaml:"discord,omitempty"`
	WaitingAfter  string `yaml:"waiting_after,omitempty"`  // empty → 5m; "0"/"off" disables
	ApprovalAfter string `yaml:"approval_after,omitempty"` // empty → 5m; "0" mails at once; "off" never
	UsageWarnAt   int    `yaml:"usage_warn_at,omitempty"`  // 0 → 80
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

// DefaultApprovalAfter is used when notify.approval_after is unset.
const DefaultApprovalAfter = 5 * time.Minute

// ApprovalAfterDuration parses ApprovalAfter (plan M5). ok=false means the
// approval escalation is off: tray only, no mail.
func (n NotifyConfig) ApprovalAfterDuration() (d time.Duration, ok bool) {
	s := strings.TrimSpace(n.ApprovalAfter)
	if s == "" {
		return DefaultApprovalAfter, true
	}
	if strings.EqualFold(s, "off") {
		return 0, false
	}
	if s == "0" {
		return 0, true
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return DefaultApprovalAfter, true
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

type Defaults struct {
	Command string          `yaml:"command"`
	Session SessionDefaults `yaml:"session,omitempty"`
	Memory  MemoryDefaults  `yaml:"memory,omitempty"`
}

// MemoryDefaults are the global structured-memory knobs when job frontmatter
// omits them. Zero values are meaningful (no cap, no freshness check), so the
// fields carry no omitempty and defaultConfig pre-seeds them.
type MemoryDefaults struct {
	MaxLogs       int  `yaml:"max_logs"`       // Log entry cap; 0 = no cap
	RequireUpdate bool `yaml:"require_update"` // a run must rewrite MEMORY.md
	InjectLogs    int  `yaml:"inject_logs"`    // Log entries in the launch prompt
}

// DefaultMemory returns the built-in memory defaults (also the seed of
// defaultConfig), so per-launch resolvers can fall back without a config file.
func DefaultMemory() MemoryDefaults {
	return MemoryDefaults{MaxLogs: 100, RequireUpdate: true, InjectLogs: 20}
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
	// Auth: "shared" (default, bridge user login) or "separate" (trebi-only creds).
	Auth string `yaml:"auth,omitempty"`
	// Path overrides the store binary (skip download; still version-check if possible).
	Path string `yaml:"path,omitempty"`
	// Model is the session default model passed as a launch flag (never a
	// vendor config file: a shared-mode user's interactive CLI must not see it).
	Model string `yaml:"model,omitempty"`
	// Effort is the session default reasoning effort passed as a launch flag.
	// Empty = the vendor default. Levels are per-tool (HarnessEffortLevels).
	Effort string `yaml:"effort,omitempty"`
	// Env is the harness-process environment for every trebi launch of this
	// tool. Job env (envset + inline env) overrides it; isolation still wins.
	Env map[string]string `yaml:"env,omitempty"`
	// Runtime forces the run runtime for this tool: "tui" (pane) or "" (the
	// default cli wire, subject to the global harness.runtime kill switch).
	Runtime string `yaml:"runtime,omitempty"`
	// Bridge overrides global tools.bridge for this tool.
	Bridge *BridgeConfig `yaml:"bridge,omitempty"`
}

// HarnessEffortLevels returns the effort levels tool accepts on a launch flag,
// in increasing depth. A nil non-empty result means free text (opencode
// variants are model-specific). An empty result means the tool has no effort
// knob (cursor, gemini, fake).
func HarnessEffortLevels(tool string) []string {
	switch tool {
	case "claude":
		return []string{"low", "medium", "high", "xhigh", "max"}
	case "grok":
		return []string{"low", "medium", "high"}
	case "codex":
		return []string{"none", "minimal", "low", "medium", "high", "xhigh"}
	case "copilot":
		return []string{"low", "medium", "high", "xhigh", "max"}
	case "pi":
		return []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}
	case "opencode":
		return nil // variant name, model-specific
	case "cursor", "gemini", "fake":
		return []string{}
	}
	return []string{}
}

// HarnessEffortFreeText reports whether tool's effort is a free-form variant
// name (opencode) rather than a fixed level list.
func HarnessEffortFreeText(tool string) bool {
	return tool == "opencode"
}

// HarnessSupportsModel reports whether tool takes a --model launch flag.
func HarnessSupportsModel(tool string) bool {
	switch tool {
	case "claude", "grok", "codex", "opencode", "copilot", "cursor", "pi", "gemini":
		return true
	}
	return false
}

// ValidEnvKey reports whether s is a portable environment variable name.
func ValidEnvKey(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// FirstClassTools is the documented filter of tmux.AllTools(): every
// registered harness except gemini and fake. Kept as this list because
// config cannot import tmux; TestFirstClassToolsMatchesAllToolsFilter pins it.
func FirstClassTools() []string {
	return []string{"claude", "grok", "codex", "opencode", "copilot", "cursor", "pi"}
}

// IsFirstClass reports whether name has a dedicated tools.<name> field in config.yaml.
func IsFirstClass(name string) bool {
	for _, t := range FirstClassTools() {
		if t == name {
			return true
		}
	}
	return false
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

// HarnessRuntimeFor resolves the run runtime for one tool (Part J): the
// per-tool tools.<name>.runtime wins, then the global harness.runtime, else
// "" (the cli wire default).
func (c *Config) HarnessRuntimeFor(tool string) string {
	if c == nil {
		return ""
	}
	if p := c.Tools.Pin(tool); p != nil && strings.TrimSpace(p.Runtime) != "" {
		return strings.ToLower(strings.TrimSpace(p.Runtime))
	}
	return strings.ToLower(strings.TrimSpace(c.Harness.Runtime))
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
func BinDir() string { return home.BinDir() }

// LibexecDir is ~/.trebi/libexec — trebi helpers (not versioned harness CLIs).
func LibexecDir() string { return home.LibexecDir() }

// HarnessDir is ~/.trebi/harness/<tool> — trebi-owned config/session tree for a harness.
func HarnessDir(tool string) string { return home.HarnessDir(tool) }

// ToolStoreDir is ~/.trebi/bin/<tool>.
func ToolStoreDir(tool string) string {
	return filepath.Join(BinDir(), tool)
}

// ToolCurrentLink is ~/.trebi/bin/<tool>/current (symlink to a version dir).
func ToolCurrentLink(tool string) string {
	return filepath.Join(ToolStoreDir(tool), "current")
}

// SecretsDir is ~/.trebi/secrets (mode 0700) for separate-mode env key files.
func SecretsDir() string { return home.SecretsDir() }

// SecretEnvPath is ~/.trebi/secrets/<tool>.env.
func SecretEnvPath(tool string) string {
	return filepath.Join(SecretsDir(), tool+".env")
}

// EnsureDirs creates the home tree via home.EnsureDirs (which also fixes the
// secrets dir to 0700), then the config extras.
func EnsureDirs() error {
	return home.EnsureDirs()
}

// ConfigPath is ~/.trebi/config.yaml (or under BaseDir in tests).
func ConfigPath() string { return home.ConfigPath() }

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
			cfg.applyDefaults()
			return cfg, nil
		}
		return nil, err
	}

	if err := decodeConfig(data, cfg); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := cfg.migrateAssistantKey(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// seedTemplate documents every tunable key. Active lines are the values
// applyDefaults would supply anyway; the rest are commented with the
// default the loader uses, so the file shows what exists without pinning it.
const seedTemplate = `# trebi config. Written once, on first daemon start.
# Commented keys show the built-in default; uncomment to pin one.
# Apply changes with: trebi config set <dotted.key> <value>

defaults:
  command: claude
  # session:
  #   timeout: ""          # idle timeout; empty means no idle reap
  #   max_duration: ""     # hard wall-clock cap per run
  #   keep_alive: ""
  #   nudge: ""
  #   if_running: ""       # skip | replace | coalesce | allow
  # memory:                # structured MEMORY.md defaults
  #   max_logs: 100        # Log entry cap; 0 = no cap
  #   require_update: true # a run must rewrite MEMORY.md
  #   inject_logs: 20      # Log entries in the launch prompt

# api:
#   host: 127.0.0.1        # 0.0.0.0 or a LAN/Tailscale IP opens remote access
#   port: 7433             # default; 0 = ephemeral; written to api.json after bind
#   tls:
#     cert: ""             # PEM cert; required with key for non-loopback HTTPS
#     key: ""
#     insecure: false      # allow plaintext on non-loopback (Tailscale/LAN)

terminal: Terminal         # macOS: Terminal | iTerm | Ghostty

tools:
  fallback: path           # path (use PATH) | require (fail when unpinned)
  # bridge:                # per-harness resource inheritance
  #   never: []

# notify:
#   email: ""              # shell template; text arrives as $TREBI_ALERT_*
#   whatsapp: ""
#   discord: ""
#   waiting_after: 5m      # "0" or "off" disables the waiting escalation
#   approval_after: 5m     # tray first, mail after; "0" mails at once; "off" never
#   usage_warn_at: 80      # percent

# files:
#   max_bytes: 10485760    # workspace file PUT/GET cap (10 MiB)

# workspaces:
#   root: ~/trebi          # pathless workspace add --create --name lands here

# assistant:                 # in-process workspace assistant (API key only)
#   model: ""                # empty = native off; set a model to enable
#   api_format: openai       # openai | anthropic
#   base_url: ""             # default api.openai.com / api.anthropic.com (no /v1)
#   api_key_env: ""          # prefer this over api_key
#   max_tokens: 8192
#   context_tokens: 0        # 0 = no compaction
#   system_extra: ""
#   web_search: true         # anthropic server tool only; ignored for openai

# email:                     # mail channel through the trebi-mail Worker
#   worker: ""               # Worker URL; present enables the channel
#   token: ""                # secret:<name> holding the shared Worker token
#   domain: ""               # bare hostname the addresses live on
#   operators: []            # alert recipients and default inbound allowlist
#   poll_interval: 30s       # 5s to 10m
#   require_dmarc: true
#   from_name: Trebi

# cloud:                     # hosted account link (plan 2026-09-22)
#   api_url: ""              # hosted endpoint (default https://staging-api.trebi.ai)
#   enabled: true            # false keeps a linked device offline
`

// Seed writes config.yaml with the documented defaults when it does not exist.
// It reports whether it created the file. A present file is never rewritten.
func Seed() (bool, error) {
	if err := EnsureDirs(); err != nil {
		return false, err
	}
	if _, err := os.Stat(ConfigPath()); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.WriteFile(ConfigPath(), []byte(seedTemplate), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// Save validates then writes config.yaml. assistant.api_key is never marshaled.
func (c *Config) Save() error {
	if err := EnsureDirs(); err != nil {
		return err
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return err
	}
	if err := c.Assistant.flushKey(); err != nil {
		return err
	}
	c.Assistant.APIKey = ""
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(), data, 0644)
}

// migrateAssistantKey moves a YAML api_key into the secrets store.
func (c *Config) migrateAssistantKey() error {
	if strings.TrimSpace(c.Assistant.APIKey) == "" {
		return nil
	}
	c.Assistant.pendingKey = c.Assistant.APIKey
	c.Assistant.APIKey = ""
	return c.Save()
}

func expandTilde(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		if p == "~" {
			return h
		}
		return filepath.Join(h, p[2:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func defaultConfig() *Config {
	return &Config{
		Defaults: Defaults{
			Command: "claude",
			Memory:  DefaultMemory(),
		},
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

// ConfigError is one or more field-level validation problems.
type ConfigError struct {
	Path   string // config file basename or absolute
	Errors []FieldError
}

// FieldError names a field path and reason.
type FieldError struct {
	Field string
	Msg   string
}

func (e *ConfigError) Error() string {
	if e == nil || len(e.Errors) == 0 {
		return "invalid config"
	}
	path := e.Path
	if path == "" {
		path = "config.yaml"
	}
	var b strings.Builder
	for i, fe := range e.Errors {
		if i > 0 {
			b.WriteByte('\n')
		}
		if fe.Field != "" {
			fmt.Fprintf(&b, "%s: %s: %s", path, fe.Field, fe.Msg)
		} else {
			fmt.Fprintf(&b, "%s: %s", path, fe.Msg)
		}
	}
	return b.String()
}

func (e *ConfigError) add(field, msg string) {
	e.Errors = append(e.Errors, FieldError{Field: field, Msg: msg})
}

// durationRe matches job frontmatter: s/m/h only.
var durationRe = regexp.MustCompile(`^(\d+(?:h|m|s))+$`)

// Validate checks enums, durations, and bridge resource keys/modes.
// Returns *ConfigError listing all problems when possible.
func (c *Config) Validate() error {
	ce := &ConfigError{Path: "config.yaml"}
	if base := ConfigPath(); base != "" {
		ce.Path = base
		// Prefer basename for stable tests when under BaseDir.
		ce.Path = "config.yaml"
	}

	// Terminal
	if c.Terminal != "" && !validTerminal(c.Terminal) {
		ce.add("terminal", fmt.Sprintf("%q not in [Terminal, iTerm, Ghostty]", c.Terminal))
	}

	host := strings.TrimSpace(c.API.Host)
	if host != "" && !validAPIHost(host) {
		ce.add("api.host", fmt.Sprintf("invalid host %q", c.API.Host))
	}
	if c.API.Port != nil && (*c.API.Port < 0 || *c.API.Port > 65535) {
		ce.add("api.port", "must be 0-65535 (0 = ephemeral)")
	}
	cert := strings.TrimSpace(c.API.TLS.Cert)
	key := strings.TrimSpace(c.API.TLS.Key)
	if (cert == "") != (key == "") {
		ce.add("api.tls", "cert and key must both be set, or both empty")
	}

	// defaults.session
	s := c.Defaults.Session
	ce.checkDuration("defaults.session.timeout", s.Timeout, true)
	ce.checkDuration("defaults.session.max_duration", s.MaxDuration, true)
	ce.checkDuration("defaults.session.keep_alive", s.KeepAlive, true)
	ce.checkNudge("defaults.session.nudge", s.Nudge)
	if s.IfRunning != "" && !validIfRunningMode(strings.ToLower(strings.TrimSpace(s.IfRunning))) {
		ce.add("defaults.session.if_running",
			fmt.Sprintf("%q not in [skip, replace, coalesce, allow]", s.IfRunning))
	}

	// notify.waiting_after: empty → default 5m; 0/off disables escalate
	ce.checkNudge("notify.waiting_after", c.Notify.WaitingAfter)
	ce.checkNudge("notify.approval_after", c.Notify.ApprovalAfter)

	if c.Files.MaxBytes < 0 {
		ce.add("files.max_bytes", "must be >= 0")
	}

	// tools.fallback
	if c.Tools.Fallback != "" && !validFallbackMode(c.Tools.Fallback) {
		ce.add("tools.fallback", fmt.Sprintf("%q not in [require, path]", c.Tools.Fallback))
	}

	// harness.runtime: "" = cli default, "tui" = incident kill switch.
	if rt := strings.ToLower(strings.TrimSpace(c.Harness.Runtime)); rt != "" && rt != "cli" && rt != "tui" {
		ce.add("harness.runtime", fmt.Sprintf("%q not in [cli, tui]", c.Harness.Runtime))
	}
	for _, tool := range []string{"claude", "grok", "codex", "opencode", "copilot", "cursor", "pi"} {
		pin := c.Tools.Pin(tool)
		if pin == nil {
			continue
		}
		if rt := strings.ToLower(strings.TrimSpace(pin.Runtime)); rt != "" && rt != "cli" && rt != "tui" {
			ce.add("tools."+tool+".runtime", fmt.Sprintf("%q not in [cli, tui]", pin.Runtime))
		}
	}

	// global bridge
	ce.checkBridge("tools.bridge", c.Tools.Bridge, "")

	for _, name := range FirstClassTools() {
		p := c.Tools.Pin(name)
		if p == nil {
			continue
		}
		prefix := "tools." + name
		if p.Auth != "" && p.Auth != "shared" && p.Auth != "separate" {
			ce.add(prefix+".auth", fmt.Sprintf("%q not in [shared, separate]", p.Auth))
		}
		if err := validateEffort(name, p.Effort); err != nil {
			ce.add(prefix+".effort", strings.TrimPrefix(err.Error(), "tools."+name+".effort: "))
		}
		for k := range p.Env {
			if !ValidEnvKey(k) {
				ce.add(prefix+".env."+k, "invalid env key")
			}
		}
		ce.checkBridge(prefix+".bridge", p.Bridge, name)
	}

	ce.checkAssistant(c.Assistant)
	ce.checkEmail(*c)
	ce.checkCloud(*c)

	if len(ce.Errors) == 0 {
		return nil
	}
	return ce
}

func (c *Config) applyDefaults() {
	if c.Defaults.Command == "" {
		c.Defaults.Command = "claude"
	}
	if c.Terminal == "" {
		c.Terminal = "Terminal"
	}
	if c.Tools.Fallback == "" {
		c.Tools.Fallback = "path"
	}
}

// NormalizeBridgeMode maps aliases to inherit|false. Invalid returns empty and false.
func NormalizeBridgeMode(raw string) (mode string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "inherit", "link":
		return "inherit", true
	case "false", "off", "none":
		return "false", true
	default:
		return "", false
	}
}

// IsInherit reports whether a normalized or raw mode means inherit.
func IsInherit(raw string) bool {
	m, ok := NormalizeBridgeMode(raw)
	return ok && m == "inherit"
}

func (e *ConfigError) checkBridge(prefix string, b *BridgeConfig, tool string) {
	if b == nil {
		return
	}
	for _, k := range b.Only {
		if tool != "" {
			if !knownResource(tool, k) {
				e.add(prefix+".only", fmt.Sprintf("unknown resource %q (known: %s)", k, strings.Join(KnownBridgeResources()[tool], ", ")))
			}
		} else if !knownResourceAny(k) {
			e.add(prefix+".only", fmt.Sprintf("unknown resource %q", k))
		}
	}
	for _, k := range b.Never {
		if tool != "" {
			if !knownResource(tool, k) {
				e.add(prefix+".never", fmt.Sprintf("unknown resource %q (known: %s)", k, strings.Join(KnownBridgeResources()[tool], ", ")))
			}
		} else if !knownResourceAny(k) {
			e.add(prefix+".never", fmt.Sprintf("unknown resource %q", k))
		}
	}
	for k, mode := range b.Resources {
		field := prefix + ".resources." + k
		if tool != "" {
			if !knownResource(tool, k) {
				e.add(field, fmt.Sprintf("unknown resource (known: %s)", strings.Join(KnownBridgeResources()[tool], ", ")))
				continue
			}
		} else if !knownResourceAny(k) {
			e.add(field, "unknown resource")
			continue
		}
		if !validBridgeModeValue(mode) {
			e.add(field, fmt.Sprintf("%q not in [true, false, inherit, link, off, none]", mode))
		}
	}
}

func knownResource(tool, key string) bool {
	for _, k := range KnownBridgeResources()[tool] {
		if k == key {
			return true
		}
	}
	return false
}

func knownResourceAny(key string) bool {
	for _, keys := range KnownBridgeResources() {
		for _, k := range keys {
			if k == key {
				return true
			}
		}
	}
	return false
}

func (e *ConfigError) checkDuration(field, raw string, allowZeroOff bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return
	}
	if allowZeroOff && (s == "0" || strings.EqualFold(s, "off")) {
		return
	}
	if !durationRe.MatchString(s) {
		e.add(field, fmt.Sprintf("invalid duration %q (use s/m/h, e.g. 30m, 1h)", s))
		return
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		e.add(field, fmt.Sprintf("invalid duration %q: %v", s, err))
		return
	}
	if d <= 0 {
		e.add(field, fmt.Sprintf("duration %q must be positive", s))
	}
}

func (e *ConfigError) checkNudge(field, raw string) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "0" || strings.EqualFold(s, "off") {
		return
	}
	e.checkDuration(field, s, false)
}

func validTerminal(v string) bool {
	switch v {
	case "Terminal", "iTerm", "Ghostty":
		return true
	}
	return false
}

// validAPIHost accepts an IP or a DNS hostname. Empty is the default.
func validAPIHost(h string) bool {
	if h == "" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return true
	}
	if len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, c := range label {
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
			if !ok && c == '-' && i > 0 && i < len(label)-1 {
				ok = true
			}
			if !ok {
				return false
			}
		}
	}
	return true
}

func validFallbackMode(v string) bool {
	return v == "require" || v == "path"
}

// validIfRunningMode accepts empty (default policy).
func validIfRunningMode(v string) bool {
	switch v {
	case "", "skip", "replace", "coalesce", "allow":
		return true
	}
	return false
}

// validBridgeModeValue accepts a bridge mode alias or empty.
func validBridgeModeValue(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	_, ok := NormalizeBridgeMode(raw)
	return ok
}
