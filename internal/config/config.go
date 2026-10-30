package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/notify"
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
	UI         UIConfig         `yaml:"ui,omitempty"`
	Files      FilesConfig      `yaml:"files,omitempty"`
	Uploads    UploadsConfig    `yaml:"uploads,omitempty"`
	Workspaces WorkspacesConfig `yaml:"workspaces,omitempty"`
	Assistant  AssistantConfig  `yaml:"assistant,omitempty"`
	Email      EmailConfig      `yaml:"email,omitempty"`
	Cloud      CloudConfig      `yaml:"cloud,omitempty"`
	Mobile     MobileConfig     `yaml:"mobile,omitempty"`
	Connectors ConnectorsConfig `yaml:"connectors,omitempty"`
	Requests   RequestsConfig   `yaml:"requests,omitempty"`
	Inbox      InboxConfig      `yaml:"inbox,omitempty"`
	Decisions  DecisionsConfig  `yaml:"decisions,omitempty"`
	Artifacts  ArtifactsConfig  `yaml:"artifacts,omitempty"`
}

// HarnessConfig is the vendor run-runtime policy (plan 2026-09-16 Part J).
// The default runtime is the subprocess CLI wire; `runtime: tui` is the
// incident-only kill switch back to panes.
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

// UploadsConfig is the size limit of one image that a person adds to a
// message. 0 is the default, 10 MiB.
type UploadsConfig struct {
	MaxBytes int64 `yaml:"max_bytes,omitempty"`
}

// NotifyConfig is the global alert layer. Route is the global notify route
// (see internal/notify); nil means unset, so the default route applies.
type NotifyConfig struct {
	Route         []notify.Step `yaml:"route,omitempty"`
	WaitingAfter  string        `yaml:"waiting_after,omitempty"`  // empty → 5m; "0"/"off" disables
	ApprovalAfter string        `yaml:"approval_after,omitempty"` // empty → 5m; "0" mails at once; "off" never
	UsageWarnAt   int           `yaml:"usage_warn_at,omitempty"`  // 0 → 80
}

// UIConfig is where a person opens the Trebi UI.
type UIConfig struct {
	// PublicURL is the base of the links in alert mail. Empty uses the
	// bound listener URL from api.json.
	PublicURL string `yaml:"public_url,omitempty"`
}

func (u UIConfig) check(e *ConfigError) {
	if u.PublicURL == "" {
		return
	}
	if p, err := url.Parse(u.PublicURL); err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		e.add("ui.public_url", "want an http or https URL, for example https://trebi.example.com")
	}
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

type Defaults struct {
	Command string `yaml:"command"`
	// Workspace is the system sender for alerts with no workspace: an id or
	// a name.
	Workspace string          `yaml:"workspace,omitempty"`
	Session   SessionDefaults `yaml:"session,omitempty"`
	Memory    MemoryDefaults  `yaml:"memory,omitempty"`
	State     StateDefaults   `yaml:"state,omitempty"`
}

// StateDefaults cap the state store of each job or agent. 0 takes the default.
type StateDefaults struct {
	MaxRows  int   `yaml:"max_rows,omitempty"`  // rows for each owner
	MaxBytes int64 `yaml:"max_bytes,omitempty"` // bytes for each owner
}

// Built-in state limits for each owner.
const (
	DefaultStateMaxRows        = 100000
	DefaultStateMaxBytes int64 = 50 << 20
)

// Resolved returns the limits with defaults for 0.
func (d StateDefaults) Resolved() StateDefaults {
	if d.MaxRows <= 0 {
		d.MaxRows = DefaultStateMaxRows
	}
	if d.MaxBytes <= 0 {
		d.MaxBytes = DefaultStateMaxBytes
	}
	return d
}

// MemoryDefaults are the global structured-memory knobs when job frontmatter
// omits them. Zero values are meaningful (no cap, no freshness check), so the
// fields carry no omitempty and defaultConfig pre-seeds them.
type MemoryDefaults struct {
	MaxLogs       int  `yaml:"max_logs"`       // Log entry cap; 0 = no cap
	RequireUpdate bool `yaml:"require_update"` // a run must rewrite MEMORY.md
	InjectLogs    int  `yaml:"inject_logs"`    // Log entries in the launch prompt
	MaxBytes      int  `yaml:"max_bytes"`      // cap on the memory part of the prompt; 0 = no cap
}

// DefaultMemoryMaxBytes is defaults.memory.max_bytes when config omits it (16 KiB).
const DefaultMemoryMaxBytes = 16384

// DefaultMemory returns the built-in memory defaults (also the seed of
// defaultConfig), so per-launch resolvers can fall back without a config file.
func DefaultMemory() MemoryDefaults {
	return MemoryDefaults{MaxLogs: 100, RequireUpdate: true, InjectLogs: 20, MaxBytes: DefaultMemoryMaxBytes}
}

// SessionDefaults are global reaper/session knobs when job frontmatter omits them.
type SessionDefaults struct {
	Timeout     string `yaml:"timeout,omitempty"`
	MaxDuration string `yaml:"max_duration,omitempty"`
	KeepAlive   string `yaml:"keep_alive,omitempty"`
	Nudge       string `yaml:"nudge,omitempty"`
	IfRunning   string `yaml:"if_running,omitempty"`
}

// DefaultMaxDuration is defaults.session.max_duration when config omits it.
const DefaultMaxDuration = 24 * time.Hour

// MaxDurationMS resolves max_duration: empty is DefaultMaxDuration, 0 or off is no limit.
func (d SessionDefaults) MaxDurationMS() int64 {
	s := strings.TrimSpace(d.MaxDuration)
	if s == "0" || strings.EqualFold(s, "off") {
		return 0
	}
	v, err := time.ParseDuration(s)
	if err != nil || v <= 0 {
		v = DefaultMaxDuration
	}
	return v.Milliseconds()
}

// ToolsConfig holds the binary pin policy and per-tool settings.
type ToolsConfig struct {
	// Fallback when pin is missing or ensure fails: "require" (fail) or "path" (PATH).
	Fallback string `yaml:"fallback,omitempty"`
	// Bridge is the global inheritance policy for harness resources.
	Bridge    *BridgeConfig `yaml:"bridge,omitempty"`
	Claude    *ToolPin      `yaml:"claude,omitempty"`
	Grok      *ToolPin      `yaml:"grok,omitempty"`
	Codex     *ToolPin      `yaml:"codex,omitempty"`
	OpenCode  *ToolPin      `yaml:"opencode,omitempty"`
	OpenCode2 *ToolPin      `yaml:"opencode2,omitempty"`
	Copilot   *ToolPin      `yaml:"copilot,omitempty"`
	Cursor    *ToolPin      `yaml:"cursor,omitempty"`
	Pi        *ToolPin      `yaml:"pi,omitempty"`
	OMP       *ToolPin      `yaml:"omp,omitempty"`
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
	// Enabled turns the harness on. Nil is off: the user turns on each harness.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Updates is "auto" (empty) or "keep". Keep runs the copy in the bin store.
	Updates string `yaml:"updates,omitempty"`
	// Version is the kept version (keep mode).
	Version string `yaml:"version,omitempty"`
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
	// Approval is "ask" (empty) or "auto". Auto lets a job on the wire use
	// tools without approval. A harness that cannot ask (HarnessAsksApproval)
	// runs only with auto.
	Approval string `yaml:"approval,omitempty"`
	// Bridge overrides global tools.bridge for this tool.
	Bridge *BridgeConfig `yaml:"bridge,omitempty"`
	// Compat holds the Claude file switches of Grok and OpenCode.
	Compat *HarnessCompat `yaml:"compat,omitempty"`
}

// HarnessCompat turns the Claude files that a harness also reads on or off.
// Keys: agents, skills, rules, mcps, hooks (CompatKeys).
type HarnessCompat struct {
	Claude map[string]bool `yaml:"claude,omitempty"`
}

// CompatKeys lists the Claude compat switches of a harness, or nil.
func CompatKeys(tool string) []string {
	switch tool {
	case "grok":
		return []string{"agents", "skills", "rules", "mcps", "hooks"}
	case "opencode", "opencode2":
		return []string{"agents", "skills"}
	}
	return nil
}

// ClaudeCompat reports whether tool reads the Claude files of key. All keys
// are on by default except hooks.
func (c *Config) ClaudeCompat(tool, key string) bool {
	if c != nil {
		if p := c.Tools.Pin(tool); p != nil && p.Compat != nil {
			if v, ok := p.Compat.Claude[key]; ok {
				return v
			}
		}
	}
	return key != "hooks"
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
		return []string{"low", "medium", "high", "xhigh"}
	case "omp":
		return []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}
	case "codex":
		return []string{"none", "minimal", "low", "medium", "high", "xhigh"}
	case "copilot":
		return []string{"low", "medium", "high", "xhigh", "max"}
	case "pi":
		return []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}
	case "opencode", "opencode2":
		return nil // variant name, model-specific
	case "cursor", "gemini", "fake":
		return []string{}
	}
	return []string{}
}

// HarnessEffortFreeText reports whether tool's effort is a free-form variant
// name (opencode) rather than a fixed level list.
func HarnessEffortFreeText(tool string) bool {
	return tool == "opencode" || tool == "opencode2"
}

// HarnessAsksApproval reports whether the wire of tool sends a permission
// request before a tool call. grok 1.0.70 does not, so it cannot run in ask
// mode.
func HarnessAsksApproval(tool string) bool {
	return tool != "grok"
}

// HarnessApproval is the approval setting of tool: "auto" or "ask".
func (c *Config) HarnessApproval(tool string) string {
	if c != nil {
		if p := c.Tools.Pin(tool); p != nil && strings.EqualFold(strings.TrimSpace(p.Approval), "auto") {
			return "auto"
		}
	}
	return "ask"
}

// HarnessSupportsModel reports whether tool takes a --model launch flag.
func HarnessSupportsModel(tool string) bool {
	switch tool {
	case "claude", "grok", "omp", "codex", "opencode", "opencode2", "copilot", "cursor", "pi", "gemini":
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

// FirstClassTools is the documented filter of agentcli.AllTools(): every
// registered harness except gemini and fake. Kept as this list because
// config cannot import agentcli; TestFirstClassToolsMatchesAllToolsFilter pins it.
func FirstClassTools() []string {
	return []string{"claude", "grok", "omp", "codex", "opencode", "opencode2", "copilot", "cursor", "pi"}
}

// HarnessEnabled reports whether the harness is on. A harness without a
// tools.<name> field (gemini, fake) is always on: it is hidden in the UI.
func (c *Config) HarnessEnabled(name string) bool {
	if !IsFirstClass(name) {
		return true
	}
	if c == nil {
		return false
	}
	p := c.Tools.Pin(name)
	return p != nil && p.Enabled != nil && *p.Enabled
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
			"user_memory",
		},
		"codex": {
			"config_prefs", "skills", "prompts", "mcp", "user_memory",
		},
		"opencode": {
			"plugins", "config", "skills", "user_memory",
		},
		"opencode2": {
			"plugins", "config", "skills", "user_memory",
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
	case "opencode2":
		return tc.OpenCode2
	case "copilot":
		return tc.Copilot
	case "cursor":
		return tc.Cursor
	case "pi":
		return tc.Pi
	case "omp":
		return tc.OMP
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
	case "opencode2":
		tc.OpenCode2 = pin
	case "copilot":
		tc.Copilot = pin
	case "cursor":
		tc.Cursor = pin
	case "pi":
		tc.Pi = pin
	case "omp":
		tc.OMP = pin
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
  #   max_duration: 24h    # hard wall-clock cap per run; off = no limit
  #   keep_alive: ""
  #   nudge: ""
  #   if_running: ""       # queue (default) | skip | replace
  # memory:                # structured MEMORY.md defaults
  #   max_logs: 100        # Log entry cap; 0 = no cap
  #   require_update: true # a run must rewrite MEMORY.md
  #   inject_logs: 20      # Log entries in the launch prompt
  #   max_bytes: 16384     # memory bytes in the launch prompt; 0 = no cap
  # state:                 # trebi state limits for each job or agent
  #   max_rows: 100000
  #   max_bytes: 52428800  # 50 MiB

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
  # claude:
  #   enabled: true        # turn on a harness; each one is off until you turn it on
  #   updates: auto        # auto | keep (Trebi runs keep the copy in ~/.trebi/bin)
  # bridge:                # per-harness resource inheritance
  #   never: []

# notify:
#   route:                 # tried in order; the first sent message stops it
#     - {via: whatsapp, to: "+15551234567"}   # a connector with a channel block
#     - {via: email, to: contact}             # contact | admins | an address
#   waiting_after: 5m      # "0" or "off" disables the waiting escalation
#   approval_after: 5m     # tray first, mail after; "0" mails at once; "off" never
#   usage_warn_at: 80      # percent

# ui:
#   public_url: https://trebi.example.com   # base of the links in alert mail

# files:
#   max_bytes: 10485760    # workspace file PUT/GET cap (10 MiB)

# requests:                # a job asks a person, then continues
#   expire_after: 168h     # a request with no answer expires after this
#   retain: 720h           # keep the report of a closed request this long

# inbox:
#   done_retention: 720h   # delete a Done item this long after it closes

# decisions:               # the safety check on a reply from a new sender
#   decider: auto          # auto | harness | native | off
#   timeout: 20s
#   model: ""              # empty = the native default
#   samples: 1
#   kinds:
#     email.reply_risk: {decider: "", samples: 0, hold_below: 0}

# artifacts:
#   max_bytes: 26214400    # 25 MiB for each version
#   keep_versions: 30
#   keep_snapshots: 2160h

# workspaces:
#   root: ~/trebi          # pathless workspace add --create --name lands here

# assistant:                 # workspace assistant settings (Part B)
#   harness: ""              # "" = auto; native or an agent-wire harness (claude, codex, ...)
#   model: ""                # passed to the harness; empty = harness default
#   effort: ""               # low | medium | high
#   envset: ""               # env set for CLI harnesses (for example claude-zai)
#   mode: auto_edit          # ask | auto_edit | auto
#   skills: [trebi]          # skill names under ~/.trebi/skills
#   idle_close: 15m          # close an idle session process after this
#   permission_timeout: 10m  # deny a headless permission after this
#   start_timeout: 30s       # fail a turn that shows no output after this
#   max_live: 4              # live agent processes across all workspaces
#   native:                  # the in-process loop (no installed CLI)
#     api_format: openai     # openai | anthropic
#     model: ""
#     base_url: ""           # default api.openai.com / api.anthropic.com (no /v1)
#     api_key_env: ""        # prefer this over the stored api_key secret
#     max_tokens: 8192
#     context_tokens: 0      # 0 = no compaction
#     web_search: true       # anthropic server tool only; ignored for openai

# email:                     # mail channel through the trebi-mail Worker
#   worker: ""               # Worker URL; present enables the channel
#   token: ""                # secret:<name> holding the shared Worker token
#   domain: ""               # bare hostname the addresses live on
#   admins: []               # alert contact and default inbound allowlist
#   poll_interval: 30s       # 5s to 10m
#   require_dmarc: true
#   from_name: Trebi

# cloud:                     # hosted account link (plan 2026-09-22)
#   api_url: ""              # hosted endpoint (default https://staging-api.trebi.ai)
#   fallback_urls: []        # used when this network blocks api_url (default: the workers.dev address)
#   enabled: true            # false keeps a linked device offline

# connectors:
#   registries:              # MCP registries the catalog reads, in order
#     - { name: official, url: https://registry.modelcontextprotocol.io }
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

// migrateAssistantKey drains a legacy flat api_key into the secrets store and
// mirrors the present flag onto the native block.
func (c *Config) migrateAssistantKey() error {
	if strings.TrimSpace(c.Assistant.APIKey) != "" {
		c.Assistant.pendingKey = c.Assistant.APIKey
		c.Assistant.APIKey = ""
	}
	if c.Assistant.APIKeyPresent {
		c.Assistant.Native.APIKeyPresent = true
	}
	if strings.TrimSpace(c.Assistant.pendingKey) == "" {
		return nil
	}
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
	if gone := removedKeys(data); len(gone) > 0 {
		return fmt.Errorf("%s: %s", filepath.Base(ConfigPath()), strings.Join(gone, "; "))
	}
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
			fmt.Sprintf("%q not in [queue, skip, replace]", s.IfRunning))
	}

	// notify.waiting_after: empty → default 5m; 0/off disables escalate
	ce.checkNudge("notify.waiting_after", c.Notify.WaitingAfter)
	ce.checkNudge("notify.approval_after", c.Notify.ApprovalAfter)

	if c.Files.MaxBytes < 0 {
		ce.add("files.max_bytes", "must be >= 0")
	}
	if c.Uploads.MaxBytes < 0 {
		ce.add("uploads.max_bytes", "must be >= 0")
	}
	if c.Defaults.Memory.MaxBytes < 0 {
		ce.add("defaults.memory.max_bytes", "must be >= 0")
	}
	if c.Defaults.State.MaxRows < 0 {
		ce.add("defaults.state.max_rows", "must be >= 0")
	}
	if c.Defaults.State.MaxBytes < 0 {
		ce.add("defaults.state.max_bytes", "must be >= 0")
	}
	ce.checkRequests(c.Requests)
	ce.checkInbox(c.Inbox)
	ce.checkDecisions(c.Decisions, c.Artifacts)
	c.UI.check(ce)

	// tools.fallback
	if c.Tools.Fallback != "" && !validFallbackMode(c.Tools.Fallback) {
		ce.add("tools.fallback", fmt.Sprintf("%q not in [require, path]", c.Tools.Fallback))
	}

	// harness.runtime: "" = cli default, "tui" = incident kill switch.
	if rt := strings.ToLower(strings.TrimSpace(c.Harness.Runtime)); rt != "" && rt != "cli" && rt != "tui" {
		ce.add("harness.runtime", fmt.Sprintf("%q not in [cli, tui]", c.Harness.Runtime))
	}
	for _, tool := range []string{"claude", "grok", "omp", "codex", "opencode", "opencode2", "copilot", "cursor", "pi"} {
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
		if p.Approval != "" && p.Approval != "ask" && p.Approval != "auto" {
			ce.add(prefix+".approval", fmt.Sprintf("%q not in [ask, auto]", p.Approval))
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
	ce.checkMobile(c.Mobile)
	ce.checkConnectors(*c)

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
	if len(c.Connectors.Registries) == 0 {
		c.Connectors.Registries = DefaultRegistries()
	}
}

// NormalizeBridgeMode maps aliases to inherit|false|own. Invalid returns empty and false.
func NormalizeBridgeMode(raw string) (mode string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "inherit", "link":
		return "inherit", true
	case "own":
		return "own", true
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
			e.add(field, fmt.Sprintf("%q not in [true, false, inherit, link, off, none, own]", mode))
		} else if m, _ := NormalizeBridgeMode(mode); m == "own" && !Ownable(tool, k) {
			e.add(field, "own is only for a config file of a harness")
		}
	}
}

// OwnableResources lists the config files of each harness that can be in
// "own" mode: Trebi keeps its own copy and the bridge leaves it alone.
func OwnableResources() map[string][]string {
	oc := []string{"config", "user_memory"}
	return map[string][]string{
		"claude":    {"settings_prefs", "user_memory", "settings_local", "mcp"},
		"codex":     {"config_prefs", "user_memory"},
		"grok":      {"config_prefs", "user_memory"},
		"opencode":  oc,
		"opencode2": oc,
	}
}

// Ownable reports whether key of tool can be in "own" mode.
func Ownable(tool, key string) bool {
	return slices.Contains(OwnableResources()[tool], key)
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
	case "", "queue", "skip", "replace":
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

// removedKeys names each removed config key with its replacement. The strict
// decode alone gives only "field not found".
func removedKeys(data []byte) []string {
	var raw struct {
		Notify map[string]any `yaml:"notify"`
		Email  map[string]any `yaml:"email"`
	}
	if yaml.Unmarshal(data, &raw) != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"email", "whatsapp", "discord"} {
		if _, ok := raw.Notify[k]; ok {
			out = append(out, "notify."+k+": removed; add a connector with a channel block and list it in notify.route")
		}
	}
	if _, ok := raw.Email["operators"]; ok {
		out = append(out, "email.operators: renamed to email.admins")
	}
	return out
}

// setUIKey handles ui.* keys; handled=false when path is not one.
func (c *Config) setUIKey(path, value string) (handled bool, err error) {
	if path != "ui.public_url" {
		return false, nil
	}
	c.UI.PublicURL = strings.TrimRight(strings.TrimSpace(value), "/")
	ce := &ConfigError{}
	if c.UI.check(ce); len(ce.Errors) > 0 {
		return true, fmt.Errorf("ui.public_url: %s", ce.Errors[0].Msg)
	}
	return true, nil
}
