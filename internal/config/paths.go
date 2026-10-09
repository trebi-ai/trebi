package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/notify"
	"gopkg.in/yaml.v3"
)

// Path source labels for config show (whether declared in config.yaml).
const (
	PathSourceFile    = "config"  // present in config.yaml
	PathSourceDefault = "default" // built-in when omitted
)

// PathEntry is one row for config show / set (dotted config key).
type PathEntry struct {
	Key         string `json:"key" yaml:"key"`
	Value       string `json:"value" yaml:"value"`
	Source      string `json:"source" yaml:"source"` // config | default
	Default     string `json:"default" yaml:"default"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// pathEntryList collects PathEntry rows against a declared-path set.
type pathEntryList struct {
	declared map[string]bool
	entries  []PathEntry
}

// PathKeyDescription returns a short explanation of a dotted config key.
func PathKeyDescription(key string) string {
	if d, ok := pathKeyDescriptions()[key]; ok {
		return d
	}
	if strings.HasPrefix(key, "assistant.") {
		switch key {
		case "assistant.harness":
			return "Harness for new sessions: empty = auto, native, or an agent-wire harness name"
		case "assistant.model":
			return "Model passed to the harness (empty = harness default)"
		case "assistant.effort":
			return "Reasoning effort: low|medium|high (empty = harness default)"
		case "assistant.envset":
			return "Env set for CLI harnesses (for example claude-zai)"
		case "assistant.mode":
			return "Permission mode: ask|auto_edit|auto"
		case "assistant.idle_close":
			return "Close an idle session process after this (for example 15m)"
		case "assistant.permission_timeout":
			return "Deny a headless permission after this (for example 10m)"
		case "assistant.max_live":
			return "Live agent processes across all workspaces"
		case "assistant.native.model":
			return "Native loop model (empty = native off)"
		case "assistant.native.api_format":
			return "Native API format: openai | anthropic"
		case "assistant.native.base_url":
			return "Native API origin (empty = api.openai.com / api.anthropic.com, no /v1)"
		case "assistant.native.api_key_env":
			return "Env var holding the native API key, preferred over the stored secret"
		case "assistant.native.max_tokens":
			return "Native per-response token cap (8192 when unset)"
		case "assistant.native.context_tokens":
			return "Native context window for compaction (0 = no compaction)"
		case "assistant.native.web_search":
			return "Anthropic server web search tool (openai ignores it)"
		case "assistant.native.limits.daily_tokens":
			return "Native daily token cap (0 = unlimited)"
		case "assistant.native.limits.max_turns":
			return "Native tool-loop turn cap (0 = loop default)"
		case "assistant.native.api_key_source":
			return "Live native key source label (read-only)"
		}
		if key == "assistant.api_key" {
			return "Stored native API key secret (write-only)"
		}
	}
	// tools.<name>.…
	parts := strings.Split(key, ".")
	if len(parts) >= 3 && parts[0] == "tools" {
		tool, rest := parts[1], strings.Join(parts[2:], ".")
		switch rest {
		case "auth":
			return fmt.Sprintf("%s auth mode: shared (user login) or separate", tool)
		case "version":
			return fmt.Sprintf("Pinned %s version (x.y.z after ensure)", tool)
		case "channel":
			return fmt.Sprintf("Last ensure channel for %s (latest|stable|…)", tool)
		case "path":
			return fmt.Sprintf("Override binary path for %s (skip store download)", tool)
		case "runtime":
			return fmt.Sprintf("%s run runtime: cli (subprocess wire) or tui (pane)", tool)
		case "model":
			return fmt.Sprintf("Default %s model, passed as a launch flag (empty = vendor default)", tool)
		case "effort":
			levels := HarnessEffortLevels(tool)
			if len(levels) > 0 {
				return fmt.Sprintf("Default %s reasoning effort (%s)", tool, strings.Join(levels, "|"))
			}
			if HarnessEffortFreeText(tool) {
				return fmt.Sprintf("Default %s variant name (model-specific, empty = vendor default)", tool)
			}
			return fmt.Sprintf("%s has no effort flag; ignore", tool)
		case "bridge.inherit":
			return fmt.Sprintf("%s-only inherit default (empty = use global)", tool)
		}
		if strings.HasPrefix(rest, "env.") {
			return fmt.Sprintf("Env for the %s process: %s", tool, strings.TrimPrefix(rest, "env."))
		}
		if strings.HasPrefix(rest, "bridge.resources.") {
			rkey := strings.TrimPrefix(rest, "bridge.resources.")
			if d, ok := bridgeResourceDescriptions()[rkey]; ok {
				return d
			}
			return fmt.Sprintf("Bridge resource %q for %s (inherit|false)", rkey, tool)
		}
	}
	return ""
}

// ResourceDefaultInherit is the built-in inherit default for a catalog key
// (matches harness catalogs; kept here to avoid import cycles).
func ResourceDefaultInherit(tool, key string) bool {
	// Defaults false:
	switch tool + "/" + key {
	case "claude/keybindings", "claude/status_line",
		"grok/marketplace",
		"opencode/config", "opencode2/config":
		return false
	}
	// Known keys default true; unknown false.
	for _, k := range KnownBridgeResources()[tool] {
		if k == key {
			return true
		}
	}
	return false
}

// ResolveResourceInherit applies tools.bridge / tools.<t>.bridge policy for a resource key.
func ResolveResourceInherit(tool, key string, cfg *Config) bool {
	def := ResourceDefaultInherit(tool, key)
	if cfg == nil {
		return def
	}
	// 1. tools.<tool>.bridge.resources.<key>
	if pin := cfg.Tools.Pin(tool); pin != nil && pin.Bridge != nil && pin.Bridge.Resources != nil {
		if raw, ok := pin.Bridge.Resources[key]; ok {
			return IsInherit(raw)
		}
	}
	// 2. tools.<tool>.bridge.inherit → then global never/only
	if pin := cfg.Tools.Pin(tool); pin != nil && pin.Bridge != nil && pin.Bridge.Inherit != nil {
		return applyGlobalBridgeFilters(key, *pin.Bridge.Inherit, cfg)
	}
	// 3–5 global
	if cfg.Tools.Bridge != nil {
		b := cfg.Tools.Bridge
		if containsStr(b.Never, key) {
			return false
		}
		if len(b.Only) > 0 && !containsStr(b.Only, key) {
			return false
		}
		if raw, ok := b.Resources[key]; ok {
			return IsInherit(raw)
		}
		if b.Inherit != nil {
			return *b.Inherit
		}
	}
	return def
}

// PathEntries returns all known config paths with effective value and source.
// declared is optional set of dotted paths present in the YAML file.
func (c *Config) PathEntries(declared map[string]bool) []PathEntry {
	if c == nil {
		c = defaultConfig()
		c.applyDefaults()
	}
	if declared == nil {
		declared = map[string]bool{}
	}
	l := &pathEntryList{declared: declared}
	l.addCore(c)
	l.addGlobalBridge(c)
	for _, tool := range FirstClassTools() {
		l.addTool(c, tool)
	}
	return l.entries
}

// DeclaredPaths returns dotted paths present in config.yaml bytes (mapping keys only).
func DeclaredPaths(data []byte) (map[string]bool, error) {
	out := map[string]bool{}
	if len(bytes.TrimSpace(data)) == 0 {
		return out, nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	// Document node → content[0] is the map
	n := &root
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	walkYAMLPaths(n, "", out)
	return out, nil
}

// LoadWithDeclared loads config and the set of paths declared in the file.
func LoadWithDeclared() (*Config, map[string]bool, error) {
	if err := EnsureDirs(); err != nil {
		return nil, nil, err
	}
	cfg := defaultConfig()
	declared := map[string]bool{}
	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			cfg.applyDefaults()
			return cfg, declared, nil
		}
		return nil, nil, err
	}
	declared, err = DeclaredPaths(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path.Base(ConfigPath()), err)
	}
	if err := decodeConfig(data, cfg); err != nil {
		return nil, nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	if err := cfg.migrateAssistantKey(); err != nil {
		return nil, nil, err
	}
	return cfg, declared, nil
}

// SetPath sets a known dotted config key on c. Does not Save.
func (c *Config) SetPath(path, value string) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	path = strings.TrimSpace(path)
	value = strings.TrimSpace(value)
	if path == "" {
		return fmt.Errorf("empty key")
	}
	if path == "terminal" {
		return setTerminal(&c.Terminal, value)
	}
	if handled, err := c.setDefaultsKey(path, value); handled {
		return err
	}
	if handled, err := c.setAPIKey(path, value); handled {
		return err
	}
	if handled, err := c.setNotifyKey(path, value); handled {
		return err
	}
	if handled, err := c.setToolsKey(path, value); handled {
		return err
	}
	if handled, err := c.setHarnessKey(path, value); handled {
		return err
	}
	if handled, err := c.setFilesKey(path, value); handled {
		return err
	}
	if handled, err := c.setWorkspacesKey(path, value); handled {
		return err
	}
	if handled, err := c.setAssistantKey(path, value); handled {
		return err
	}
	if handled, err := c.setEmailKey(path, value); handled {
		return err
	}
	if handled, err := c.setCloudKey(path, value); handled {
		return err
	}
	if handled, err := c.setConnectorsKey(path, value); handled {
		return err
	}
	if handled, err := c.setRequestsKey(path, value); handled {
		return err
	}
	if handled, err := c.setUIKey(path, value); handled {
		return err
	}

	// tools.<name>.…
	parts := strings.Split(path, ".")
	if len(parts) >= 3 && parts[0] == "tools" {
		tool := parts[1]
		if !IsFirstClass(tool) {
			return fmt.Errorf("unknown tool %q", tool)
		}
		rest := strings.Join(parts[2:], ".")
		return c.setToolKey(tool, rest, value)
	}
	return fmt.Errorf("unknown key %q (see trebi config show)", path)
}

// EntriesToTreeMap nests PathEntries for --tree / nested YAML/JSON.
func EntriesToTreeMap(entries []PathEntry) map[string]any {
	root := map[string]any{}
	for _, e := range entries {
		leaf := map[string]any{
			"value":   e.Value,
			"source":  e.Source,
			"default": e.Default,
		}
		if e.Description != "" {
			leaf["description"] = e.Description
		}
		setNested(root, strings.Split(e.Key, "."), leaf)
	}
	return root
}

// SortedPaths returns known config keys for completion/docs.
func SortedPaths() []string {
	cfg := defaultConfig()
	cfg.applyDefaults()
	ents := cfg.PathEntries(nil)
	paths := make([]string, len(ents))
	for i, e := range ents {
		paths[i] = e.Key
	}
	sort.Strings(paths)
	return paths
}

// FilterPathEntries keeps entries matching any pattern. Non-glob patterns are
// prefix filters (tools.grok → tools.grok and tools.grok.*); globs use path.Match.
func FilterPathEntries(entries []PathEntry, patterns []string) ([]PathEntry, error) {
	if len(patterns) == 0 {
		return entries, nil
	}
	clean := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if isKeyGlob(p) {
			// path.Match validates the pattern even with an empty name.
			if _, err := path.Match(p, ""); err != nil {
				return nil, fmt.Errorf("invalid --key pattern %q: %w", p, err)
			}
		}
		clean = append(clean, p)
	}
	if len(clean) == 0 {
		return entries, nil
	}
	var out []PathEntry
	for _, e := range entries {
		for _, p := range clean {
			ok, err := matchConfigKey(p, e.Key)
			if err != nil {
				return nil, fmt.Errorf("invalid --key pattern %q: %w", p, err)
			}
			if ok {
				out = append(out, e)
				break
			}
		}
	}
	return out, nil
}

func (tc *ToolsConfig) BridgeOnly() []string {
	if tc == nil || tc.Bridge == nil {
		return nil
	}
	return tc.Bridge.Only
}

func (tc *ToolsConfig) BridgeNever() []string {
	if tc == nil || tc.Bridge == nil {
		return nil
	}
	return tc.Bridge.Never
}

func (l *pathEntryList) add(path, value, def string) {
	src := PathSourceDefault
	if l.declared[path] {
		src = PathSourceFile
	}
	if value == "" {
		value = "-"
	}
	if def == "" {
		def = "-"
	}
	l.entries = append(l.entries, PathEntry{
		Key: path, Value: value, Source: src, Default: def,
		Description: PathKeyDescription(path),
	})
}

// addCore adds the top-level, defaults, and notify rows.
func (l *pathEntryList) addCore(cfg *Config) {
	l.add("defaults.command", cfg.Defaults.Command, "claude")
	l.add("defaults.workspace", cfg.Defaults.Workspace, "")
	l.add("defaults.session.timeout", cfg.Defaults.Session.Timeout, "")
	l.add("defaults.session.max_duration", cfg.Defaults.Session.MaxDuration, "")
	l.add("defaults.session.keep_alive", cfg.Defaults.Session.KeepAlive, "")
	l.add("defaults.session.nudge", cfg.Defaults.Session.Nudge, "")
	l.add("defaults.session.if_running", cfg.Defaults.Session.IfRunning, "")
	l.add("defaults.memory.max_bytes", strconv.Itoa(cfg.Defaults.Memory.MaxBytes), strconv.Itoa(DefaultMemoryMaxBytes))
	st := cfg.Defaults.State.Resolved()
	l.add("defaults.state.max_rows", strconv.Itoa(st.MaxRows), strconv.Itoa(DefaultStateMaxRows))
	l.add("defaults.state.max_bytes", strconv.FormatInt(st.MaxBytes, 10), strconv.FormatInt(DefaultStateMaxBytes, 10))
	l.add("api.host", cfg.API.HostOrDefault(), DefaultAPIHost)
	l.add("api.port", strconv.Itoa(cfg.API.PortOrDefault()), strconv.Itoa(DefaultAPIPort))
	l.add("api.tls.cert", cfg.API.TLS.Cert, "")
	l.add("api.tls.key", cfg.API.TLS.Key, "")
	insecure := "false"
	if cfg.API.TLS.Insecure {
		insecure = "true"
	}
	l.add("api.tls.insecure", insecure, "false")
	l.add("terminal", cfg.Terminal, "Terminal")
	l.add("tools.fallback", cfg.Tools.Fallback, "path")
	harnessRuntime := strings.TrimSpace(cfg.Harness.Runtime)
	if harnessRuntime == "" {
		harnessRuntime = "cli"
	}
	l.add("harness.runtime", harnessRuntime, "cli")
	route := ""
	if cfg.Notify.Route != nil {
		b, _ := json.Marshal(cfg.Notify.Route) // []notify.Step always marshals
		route = string(b)
	}
	l.add("notify.route", route, "")
	// Effective value: unset → 5m (WaitingAfterDuration); keep source=default when not in YAML.
	waShow := strings.TrimSpace(cfg.Notify.WaitingAfter)
	if waShow == "" {
		waShow = "5m"
	}
	l.add("notify.waiting_after", waShow, "5m")
	aaShow := strings.TrimSpace(cfg.Notify.ApprovalAfter)
	if aaShow == "" {
		aaShow = "5m"
	}
	l.add("notify.approval_after", aaShow, "5m")
	warnShow := "80"
	if cfg.Notify.UsageWarnAt > 0 {
		warnShow = strconv.Itoa(cfg.Notify.UsageWarnAt)
	}
	l.add("notify.usage_warn_at", warnShow, "80")
	mb := cfg.Files.MaxBytes
	if mb <= 0 {
		mb = DefaultFilesMaxBytes
	}
	l.add("files.max_bytes", strconv.FormatInt(mb, 10), strconv.FormatInt(DefaultFilesMaxBytes, 10))
	rootShow := strings.TrimSpace(cfg.Workspaces.Root)
	if rootShow == "" {
		rootShow = DefaultWorkspacesRoot
	}
	l.add("workspaces.root", rootShow, DefaultWorkspacesRoot)
	l.addAssistant(cfg)
	l.addEmail(cfg)
	l.addCloud(cfg)
	l.addConnectors(cfg)
	l.addRequests(cfg)
	l.add("ui.public_url", cfg.UI.PublicURL, "")
}

// addCloud adds the cloud block rows.
func (l *pathEntryList) addCloud(cfg *Config) {
	c := cfg.Cloud
	l.add("cloud.api_url", c.APIURLOrDefault(), DefaultCloudAPIURL)
	l.add("cloud.enabled", strconv.FormatBool(c.Active()), "true")
	l.add("cloud.remote_mcp", strconv.FormatBool(c.RemoteMCPOn()), "true")
	l.add("cloud.mobile", strconv.FormatBool(c.MobileOn()), "true")
}

// addEmail adds the email block rows.
func (l *pathEntryList) addEmail(cfg *Config) {
	e := cfg.Email
	l.add("email.worker", e.Worker, "")
	l.add("email.token", e.Token, "")
	l.add("email.domain", e.Domain, "")
	l.add("email.admins", joinCSV(e.Admins), "")
	pi := strings.TrimSpace(e.PollInterval)
	if pi == "" {
		pi = DefaultEmailPollInterval.String()
	}
	l.add("email.poll_interval", pi, DefaultEmailPollInterval.String())
	l.add("email.require_dmarc", strconv.FormatBool(e.DMARCRequired()), "true")
	l.add("email.from_name", e.FromSenderName(), "Trebi")
	l.add("email.content_days", strconv.Itoa(e.ContentDaysOrDefault()), strconv.Itoa(DefaultEmailContentDays))
}

func (l *pathEntryList) addAssistant(cfg *Config) {
	a := cfg.Assistant
	l.add("assistant.harness", a.Harness, "")
	l.add("assistant.model", a.Model, "")
	l.add("assistant.effort", a.Effort, "")
	l.add("assistant.envset", a.Envset, "")
	l.add("assistant.mode", a.ModeOrDefault(), DefaultAssistantMode)
	l.add("assistant.idle_close", a.IdleCloseDuration().String(), DefaultAssistantIdleClose.String())
	l.add("assistant.permission_timeout", a.PermissionTimeoutDuration().String(), DefaultAssistantPermissionTimeout.String())
	l.add("assistant.max_live", strconv.Itoa(a.MaxLiveOrDefault()), strconv.Itoa(DefaultAssistantMaxLive))
	n := a.Native
	l.add("assistant.native.model", n.Model, "")
	format := n.Format()
	l.add("assistant.native.api_format", format, "openai")
	base := strings.TrimSpace(n.BaseURL)
	if base == "" {
		base = n.DefaultBaseURL()
	}
	l.add("assistant.native.base_url", base, n.DefaultBaseURL())
	keyVal := "-"
	if a.APIKeyPresent || strings.TrimSpace(a.pendingKey) != "" {
		keyVal = "********"
	}
	l.add("assistant.api_key", keyVal, "")
	l.add("assistant.native.api_key_env", n.APIKeyEnv, "")
	l.add("assistant.native.api_key_source", n.KeySourceName(), KeySourceNone)
	l.add("assistant.native.max_tokens", strconv.Itoa(n.MaxTokensOrDefault()), strconv.Itoa(DefaultAssistantMaxTokens))
	l.add("assistant.native.context_tokens", strconv.Itoa(n.ContextTokens), "0")
	ws := "true"
	if n.WebSearch != nil && !*n.WebSearch {
		ws = "false"
	}
	l.add("assistant.native.web_search", ws, "true")
	l.add("assistant.native.limits.daily_tokens", strconv.Itoa(n.Limits.DailyTokens), "0")
	l.add("assistant.native.limits.max_turns", strconv.Itoa(n.Limits.MaxTurns), "0")
}

// addGlobalBridge adds the tools.bridge rows.
func (l *pathEntryList) addGlobalBridge(cfg *Config) {
	gInherit := "true"
	if cfg.Tools.Bridge != nil && cfg.Tools.Bridge.Inherit != nil {
		gInherit = strconv.FormatBool(*cfg.Tools.Bridge.Inherit)
	}
	l.add("tools.bridge.inherit", gInherit, "true")
	l.add("tools.bridge.only", joinCSV(cfg.Tools.BridgeOnly()), "")
	l.add("tools.bridge.never", joinCSV(cfg.Tools.BridgeNever()), "")
}

// addTool adds the tools.<tool> pin and bridge resource rows.
func (l *pathEntryList) addTool(cfg *Config, tool string) {
	prefix := "tools." + tool
	p := cfg.Tools.Pin(tool)
	auth := "shared"
	ver, ch, path := "", "", ""
	if p != nil {
		if p.Auth != "" {
			auth = p.Auth
		}
		ver, ch, path = p.Version, p.Channel, p.Path
	}
	l.add(prefix+".auth", auth, "shared")
	l.add(prefix+".version", ver, "")
	l.add(prefix+".channel", ch, "")
	l.add(prefix+".path", path, "")
	rt := ""
	if p != nil {
		rt = p.Runtime
	}
	l.add(prefix+".runtime", rt, "")

	model, effort := "", ""
	if p != nil {
		model, effort = p.Model, p.Effort
	}
	if HarnessSupportsModel(tool) {
		l.add(prefix+".model", model, "")
	}
	if len(HarnessEffortLevels(tool)) > 0 || HarnessEffortFreeText(tool) {
		l.add(prefix+".effort", effort, "")
	}
	if p != nil {
		for _, k := range sortedKeys(p.Env) {
			l.add(prefix+".env."+k, p.Env[k], "")
		}
	}

	tInherit := ""
	if p != nil && p.Bridge != nil && p.Bridge.Inherit != nil {
		tInherit = strconv.FormatBool(*p.Bridge.Inherit)
	}
	l.add(prefix+".bridge.inherit", tInherit, "") // no single default string; empty = use global

	for _, key := range KnownBridgeResources()[tool] {
		rpath := prefix + ".bridge.resources." + key
		eff := "off"
		if ResolveResourceInherit(tool, key, cfg) {
			eff = "inherit"
		}
		def := "off"
		if ResourceDefaultInherit(tool, key) {
			def = "inherit"
		}
		l.add(rpath, eff, def)
	}
}

// setDefaultsKey handles defaults.* keys; handled=false when path is not one.
func (c *Config) setDefaultsKey(path, value string) (handled bool, err error) {
	switch path {
	case "defaults.command":
		if value == "" {
			return true, fmt.Errorf("defaults.command: empty")
		}
		c.Defaults.Command = value
		return true, nil
	case "defaults.workspace":
		c.Defaults.Workspace = clearOrValue(value)
		return true, nil
	case "defaults.session.timeout":
		return true, setDurationOrEmpty(&c.Defaults.Session.Timeout, value, path)
	case "defaults.session.max_duration":
		return true, setDurationOrEmpty(&c.Defaults.Session.MaxDuration, value, path)
	case "defaults.session.keep_alive":
		return true, setDurationOrEmpty(&c.Defaults.Session.KeepAlive, value, path)
	case "defaults.session.nudge":
		return true, setNudge(&c.Defaults.Session.Nudge, value, path)
	case "defaults.session.if_running":
		return true, setIfRunning(&c.Defaults.Session.IfRunning, value, path)
	case "defaults.memory.max_bytes":
		if strings.TrimSpace(value) == "" {
			c.Defaults.Memory.MaxBytes = DefaultMemoryMaxBytes
			return true, nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 0 {
			return true, fmt.Errorf("%s: want non-negative integer", path)
		}
		c.Defaults.Memory.MaxBytes = n
		return true, nil
	case "defaults.state.max_rows", "defaults.state.max_bytes":
		n := int64(0)
		if v := strings.TrimSpace(value); v != "" {
			var err error
			if n, err = strconv.ParseInt(v, 10, 64); err != nil || n < 0 {
				return true, fmt.Errorf("%s: want non-negative integer", path)
			}
		}
		if path == "defaults.state.max_rows" {
			c.Defaults.State.MaxRows = int(n)
		} else {
			c.Defaults.State.MaxBytes = n
		}
		return true, nil
	}
	return false, nil
}

// setAPIKey handles api.* keys; handled=false when path is not one.
func (c *Config) setAPIKey(path, value string) (handled bool, err error) {
	switch path {
	case "api.host":
		v := clearOrValue(value)
		if v != "" && !validAPIHost(v) {
			return true, fmt.Errorf("api.host: invalid host %q", value)
		}
		c.API.Host = v
		return true, nil
	case "api.port":
		if value == "" || value == "-" {
			c.API.Port = nil
			return true, nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 0 || n > 65535 {
			return true, fmt.Errorf("api.port: want integer 0-65535")
		}
		c.API.Port = &n
		return true, nil
	case "api.tls.cert":
		c.API.TLS.Cert = clearOrValue(value)
		return true, nil
	case "api.tls.key":
		c.API.TLS.Key = clearOrValue(value)
		return true, nil
	case "api.tls.insecure":
		if value == "" || value == "-" {
			c.API.TLS.Insecure = false
			return true, nil
		}
		b, err := parseBoolish(value)
		if err != nil {
			return true, fmt.Errorf("api.tls.insecure: %w", err)
		}
		c.API.TLS.Insecure = b
		return true, nil
	}
	return false, nil
}

// setNotifyKey handles notify.* keys; handled=false when path is not one.
func (c *Config) setNotifyKey(path, value string) (handled bool, err error) {
	switch path {
	case "notify.route":
		if clearOrValue(value) == "" {
			c.Notify.Route = nil
			return true, nil
		}
		var raw any
		if err := json.Unmarshal([]byte(value), &raw); err != nil {
			return true, fmt.Errorf("notify.route: want a JSON list of {via, to} steps: %w", err)
		}
		steps, err := notify.ParseSteps(raw)
		if err != nil {
			return true, err
		}
		if steps == nil {
			steps = []notify.Step{}
		}
		c.Notify.Route = steps
	case "notify.waiting_after":
		return true, setNudge(&c.Notify.WaitingAfter, value, path)
	case "notify.approval_after":
		v := strings.TrimSpace(value)
		if v != "" && v != "0" && !strings.EqualFold(v, "off") {
			if d, err := time.ParseDuration(v); err != nil || d < 0 {
				return true, fmt.Errorf("notify.approval_after: want a duration, 0, or off")
			}
		}
		c.Notify.ApprovalAfter = clearOrValue(v)
	case "notify.usage_warn_at":
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 0 || n > 100 {
			return true, fmt.Errorf("notify.usage_warn_at: want integer 0-100")
		}
		c.Notify.UsageWarnAt = n
		return true, nil
	default:
		return false, nil
	}
	return true, nil
}

// setToolsKey handles global tools.* keys; handled=false when path is not one.
func (c *Config) setToolsKey(path, value string) (handled bool, err error) {
	switch path {
	case "tools.fallback":
		return true, setFallback(&c.Tools.Fallback, value)
	case "tools.bridge.inherit":
		b, err := parseBoolish(value)
		if err != nil {
			return true, fmt.Errorf("tools.bridge.inherit: %w", err)
		}
		if c.Tools.Bridge == nil {
			c.Tools.Bridge = &BridgeConfig{}
		}
		c.Tools.Bridge.Inherit = &b
		return true, nil
	case "tools.bridge.only":
		return true, setStringList(&c.Tools.Bridge, true, value)
	case "tools.bridge.never":
		return true, setStringList(&c.Tools.Bridge, false, value)
	}
	return false, nil
}

// setHarnessKey handles harness.* keys; handled=false when path is not one.
func (c *Config) setHarnessKey(path, value string) (handled bool, err error) {
	if path != "harness.runtime" {
		return false, nil
	}
	v := strings.ToLower(strings.TrimSpace(value))
	if v != "" && v != "cli" && v != "tui" {
		return true, fmt.Errorf("harness.runtime: want cli or tui")
	}
	c.Harness.Runtime = v
	return true, nil
}

// setFilesKey handles files.* keys; handled=false when path is not one.
func (c *Config) setFilesKey(path, value string) (handled bool, err error) {
	if path != "files.max_bytes" {
		return false, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || n < 0 {
		return true, fmt.Errorf("files.max_bytes: want non-negative integer")
	}
	c.Files.MaxBytes = n
	return true, nil
}

// setWorkspacesKey handles workspaces.* keys; handled=false when path is not one.
func (c *Config) setWorkspacesKey(path, value string) (handled bool, err error) {
	if path != "workspaces.root" {
		return false, nil
	}
	c.Workspaces.Root = value
	return true, nil
}

// setEmailKey handles email.* keys; handled=false when path is not one.
func (c *Config) setEmailKey(path, value string) (handled bool, err error) {
	switch path {
	case "email.worker":
		c.Email.Worker = clearOrValue(value)
	case "email.token":
		c.Email.Token = clearOrValue(value)
	case "email.domain":
		c.Email.Domain = clearOrValue(value)
	case "email.admins":
		ops, err := parseStringSlice(value, true)
		if err != nil {
			return true, fmt.Errorf("email.admins: %w", err)
		}
		c.Email.Admins = ops
	case "email.poll_interval":
		return true, setDurationOrEmpty(&c.Email.PollInterval, value, path)
	case "email.require_dmarc":
		if value == "" || value == "-" {
			c.Email.RequireDMARC = nil
			return true, nil
		}
		b, err := parseBoolish(value)
		if err != nil {
			return true, fmt.Errorf("email.require_dmarc: %w", err)
		}
		c.Email.RequireDMARC = &b
	case "email.from_name":
		c.Email.FromName = clearOrValue(value)
	case "email.content_days":
		return true, c.Email.setContentDays(value)
	default:
		return false, nil
	}
	return true, nil
}

// setCloudKey handles cloud.* keys; handled=false when path is not one.
func (c *Config) setCloudKey(path, value string) (handled bool, err error) {
	switch path {
	case "cloud.api_url":
		c.Cloud.APIURL = clearOrValue(value)
	case "cloud.enabled":
		if value == "" || value == "-" {
			c.Cloud.Enabled = nil
			return true, nil
		}
		b, err := parseBoolish(value)
		if err != nil {
			return true, fmt.Errorf("cloud.enabled: %w", err)
		}
		c.Cloud.Enabled = &b
	case "cloud.remote_mcp":
		if value == "" || value == "-" {
			c.Cloud.RemoteMCP = nil
			return true, nil
		}
		b, err := parseBoolish(value)
		if err != nil {
			return true, fmt.Errorf("cloud.remote_mcp: %w", err)
		}
		c.Cloud.RemoteMCP = &b
	case "cloud.mobile":
		if value == "" || value == "-" {
			c.Cloud.Mobile = nil
			return true, nil
		}
		b, err := parseBoolish(value)
		if err != nil {
			return true, fmt.Errorf("cloud.mobile: %w", err)
		}
		c.Cloud.Mobile = &b
	default:
		return false, nil
	}
	return true, nil
}

// UnsetPath clears a dotted key. assistant.api_key also deletes the secret.
func (c *Config) UnsetPath(path string) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	path = strings.TrimSpace(path)
	if path == "assistant.api_key" {
		c.Assistant.pendingKey = ""
		c.Assistant.APIKeyPresent = false
		c.Assistant.Native.APIKeyPresent = false
		c.Assistant.clearSecret = true
		return nil
	}
	return c.SetPath(path, "")
}

func (c *Config) setAssistantKey(path, value string) (handled bool, err error) {
	n := &c.Assistant.Native
	switch path {
	case "assistant.harness":
		v := strings.TrimSpace(value)
		if v != "" && !knownHarness(v) {
			return true, fmt.Errorf("assistant.harness: %q is not a known harness", value)
		}
		c.Assistant.Harness = v
	case "assistant.model":
		c.Assistant.Model = clearOrValue(value)
	case "assistant.effort":
		v := strings.TrimSpace(value)
		switch v {
		case "", "low", "medium", "high":
		default:
			return true, fmt.Errorf("assistant.effort: %q not in [low, medium, high]", value)
		}
		c.Assistant.Effort = v
	case "assistant.envset":
		c.Assistant.Envset = clearOrValue(value)
	case "assistant.mode":
		v := strings.TrimSpace(value)
		switch v {
		case "", "ask", "auto_edit", "auto":
		default:
			return true, fmt.Errorf("assistant.mode: %q not in [ask, auto_edit, auto]", value)
		}
		c.Assistant.Mode = v
	case "assistant.idle_close":
		v := strings.TrimSpace(value)
		if v != "" {
			if _, err := time.ParseDuration(v); err != nil {
				return true, fmt.Errorf("assistant.idle_close: want a duration, for example 15m")
			}
		}
		c.Assistant.IdleClose = v
	case "assistant.permission_timeout":
		v := strings.TrimSpace(value)
		if v != "" {
			if _, err := time.ParseDuration(v); err != nil {
				return true, fmt.Errorf("assistant.permission_timeout: want a duration, for example 10m")
			}
		}
		c.Assistant.PermissionTimeout = v
	case "assistant.max_live":
		v := strings.TrimSpace(value)
		if v == "" {
			c.Assistant.MaxLive = 0
			return true, nil
		}
		nLive, err := strconv.Atoi(v)
		if err != nil || nLive < 0 {
			return true, fmt.Errorf("assistant.max_live: want integer >= 0")
		}
		c.Assistant.MaxLive = nLive
	case "assistant.api_key":
		if value == "" || value == "-" {
			return true, c.UnsetPath(path)
		}
		c.Assistant.pendingKey = value
		c.Assistant.clearSecret = false
	case "assistant.native.model":
		n.Model = clearOrValue(value)
	case "assistant.native.api_format":
		v := strings.ToLower(strings.TrimSpace(value))
		if v != "" && v != "openai" && v != "anthropic" {
			return true, fmt.Errorf("assistant.native.api_format: %q not in [openai, anthropic]", value)
		}
		n.APIFormat = v
	case "assistant.native.base_url":
		n.BaseURL = clearOrValue(value)
	case "assistant.native.api_key_env":
		n.APIKeyEnv = clearOrValue(value)
		// A later env pin replaces a stored secret so precedence can fall back.
		c.Assistant.clearSecret = true
		n.APIKeyPresent = false
		c.Assistant.pendingKey = ""
	case "assistant.native.api_key_source":
		return true, fmt.Errorf("assistant.native.api_key_source: read-only")
	case "assistant.native.max_tokens":
		num, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || num < 0 {
			return true, fmt.Errorf("assistant.native.max_tokens: want integer > 0")
		}
		n.MaxTokens = num
	case "assistant.native.context_tokens":
		num, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || num < 0 {
			return true, fmt.Errorf("assistant.native.context_tokens: want integer >= 0")
		}
		n.ContextTokens = num
	case "assistant.native.web_search":
		b, err := parseBoolish(value)
		if err != nil {
			return true, fmt.Errorf("assistant.native.web_search: %w", err)
		}
		n.WebSearch = &b
	case "assistant.native.limits.daily_tokens":
		num, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || num < 0 {
			return true, fmt.Errorf("assistant.native.limits.daily_tokens: want integer >= 0")
		}
		n.Limits.DailyTokens = num
	case "assistant.native.limits.max_turns":
		num, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || num < 0 {
			return true, fmt.Errorf("assistant.native.limits.max_turns: want integer >= 0")
		}
		n.Limits.MaxTurns = num
	default:
		return false, nil
	}
	return true, nil
}

// setToolKey handles tools.<tool>.<rest> keys on the tool pin.
func (c *Config) setToolKey(tool, rest, value string) error {
	pin := c.Tools.Pin(tool)
	if pin == nil {
		pin = &ToolPin{}
	}
	switch rest {
	case "auth":
		if value != "shared" && value != "separate" {
			return fmt.Errorf("tools.%s.auth: %q not in [shared, separate]", tool, value)
		}
		pin.Auth = value
	case "version":
		pin.Version = value
	case "channel":
		pin.Channel = value
	case "path":
		pin.Path = value
	case "runtime":
		v := strings.ToLower(strings.TrimSpace(value))
		if v != "" && v != "cli" && v != "tui" {
			return fmt.Errorf("tools.%s.runtime: want cli or tui", tool)
		}
		pin.Runtime = v
	case "model":
		if value != "" && !HarnessSupportsModel(tool) {
			return fmt.Errorf("tools.%s.model: %s takes no model flag", tool, tool)
		}
		pin.Model = value
	case "effort":
		if err := validateEffort(tool, value); err != nil {
			return err
		}
		pin.Effort = value
	case "bridge.inherit":
		if err := pin.setBridgeInherit(tool, value); err != nil {
			return err
		}
	default:
		if strings.HasPrefix(rest, "env.") {
			return pin.setEnv(strings.TrimPrefix(rest, "env."), value)
		}
		if !strings.HasPrefix(rest, "bridge.resources.") {
			return fmt.Errorf("unknown key tools.%s.%s", tool, rest)
		}
		key := strings.TrimPrefix(rest, "bridge.resources.")
		if err := pin.setBridgeResource(tool, key, value); err != nil {
			return err
		}
	}
	c.Tools.SetPin(tool, pin)
	return nil
}

// sortedKeys returns map keys in stable order (config rows read top to bottom).
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// setEnv sets or clears one harness-process environment variable.
func (p *ToolPin) setEnv(key, value string) error {
	if !ValidEnvKey(key) {
		return fmt.Errorf("invalid env key %q (want [A-Za-z_][A-Za-z0-9_]*)", key)
	}
	if value == "" || value == "-" {
		if p.Env != nil {
			delete(p.Env, key)
			if len(p.Env) == 0 {
				p.Env = nil
			}
		}
		return nil
	}
	if p.Env == nil {
		p.Env = map[string]string{}
	}
	p.Env[key] = value
	return nil
}

// validateEffort checks value against the tool's effort list (or free text).
func validateEffort(tool, value string) error {
	if value == "" || value == "-" {
		return nil
	}
	if HarnessEffortFreeText(tool) {
		if strings.ContainsAny(value, " \t") {
			return fmt.Errorf("tools.%s.effort: %q has whitespace", tool, value)
		}
		return nil
	}
	levels := HarnessEffortLevels(tool)
	if len(levels) == 0 {
		return fmt.Errorf("tools.%s.effort: %s has no effort flag", tool, tool)
	}
	for _, l := range levels {
		if l == value {
			return nil
		}
	}
	return fmt.Errorf("tools.%s.effort: %q not in [%s]", tool, value, strings.Join(levels, ", "))
}

// setBridgeInherit sets or clears the per-tool inherit override.
func (p *ToolPin) setBridgeInherit(tool, value string) error {
	if value == "" || value == "-" {
		if p.Bridge != nil {
			p.Bridge.Inherit = nil
		}
		return nil
	}
	if p.Bridge == nil {
		p.Bridge = &BridgeConfig{}
	}
	b, err := parseBoolish(value)
	if err != nil {
		return fmt.Errorf("tools.%s.bridge.inherit: %w", tool, err)
	}
	p.Bridge.Inherit = &b
	return nil
}

// setBridgeResource stores a normalized mode for a known resource key.
func (p *ToolPin) setBridgeResource(tool, key, value string) error {
	if !knownResource(tool, key) {
		return fmt.Errorf("tools.%s.bridge.resources.%s: unknown resource (known: %s)",
			tool, key, strings.Join(KnownBridgeResources()[tool], ", "))
	}
	norm, ok := NormalizeBridgeMode(value)
	if !ok {
		return fmt.Errorf("mode %q not in [true, false, inherit, link, off, none]", value)
	}
	store := "inherit"
	if norm == "false" {
		store = "false"
	}
	if p.Bridge == nil {
		p.Bridge = &BridgeConfig{}
	}
	if p.Bridge.Resources == nil {
		p.Bridge.Resources = ResourceModeMap{}
	}
	p.Bridge.Resources[key] = store
	return nil
}

// pathKeyDescriptions returns concise docs for static config keys (config show).
func pathKeyDescriptions() map[string]string {
	return map[string]string{
		"defaults.command":              "Default harness CLI when a job omits command",
		"defaults.session.timeout":      "Max continuous agent idle before timed_out (s/m/h)",
		"defaults.session.max_duration": "Wall-clock hard cap from task start (s/m/h)",
		"defaults.session.keep_alive":   "Hold session after COMPLETE for follow-ups",
		"defaults.session.nudge":        "Idle delay before typing continue (default off; set e.g. 5m)",
		"defaults.session.if_running":   "Overlap policy: queue|skip|replace",
		"defaults.memory.max_bytes":     "Cap on MEMORY.md bytes in the launch prompt; 0 = no cap (default 16384)",
		"defaults.state.max_rows":       "Rows in trebi state for each job or agent (default 100000)",
		"defaults.state.max_bytes":      "Bytes in trebi state for each job or agent (default 52428800)",
		"api.host":                      "HTTP bind host (default 127.0.0.1; 0.0.0.0 opens LAN)",
		"api.port":                      "HTTP bind port (default 7433; 0 = ephemeral, written to api.json)",
		"api.tls.cert":                  "PEM cert path; required with api.tls.key for non-loopback HTTPS",
		"api.tls.key":                   "PEM key path; required with api.tls.cert",
		"api.tls.insecure":              "Allow plaintext on a non-loopback bind (Tailscale/LAN); default false",
		"terminal":                      "macOS app for trebi run attach (Terminal|iTerm|Ghostty)",
		"tools.fallback":                "When pin missing: require (fail) or path (use PATH)",
		"harness.runtime":               "Vendor run runtime: cli (subprocess wire, default) or tui (incident kill switch)",
		"notify.route":                  `Global alert route as JSON, e.g. [{"via":"whatsapp","to":"+1555"},{"via":"email","to":"contact"}]`,
		"notify.waiting_after":          "Escalate waiting tasks through the notify route (default 5m; off/0 disables)",
		"notify.approval_after":         "Delay before an approval request pages (default 5m; 0 at once; off never)",
		"defaults.workspace":            "System sender workspace (id or name) for alerts with no workspace",
		"notify.usage_warn_at":          "Harness usage percent that pages a warning (default 80)",
		"files.max_bytes":               "Workspace file PUT/GET size cap in bytes (default 10485760)",
		"requests.expire_after":         "A request with no answer expires after this (default 168h)",
		"requests.retain":               "Keep the report of a closed request this long (default 720h)",
		"ui.public_url":                 "Base URL of the links in alert mail (default: the daemon listener URL)",
		"workspaces.root":               "Directory for pathless workspace create (default ~/trebi)",
		"tools.bridge.inherit":          "Global default: inherit user harness resources",
		"tools.bridge.only":             "If set, only these resource keys may inherit",
		"tools.bridge.never":            "Resource keys force-disabled globally",
		"assistant.model":               "Native assistant model; empty keeps the tmux harness",
		"assistant.api_format":          "Provider wire format: openai or anthropic (default openai)",
		"assistant.base_url":            "API origin without a required /v1 suffix",
		"assistant.api_key":             "API key (write-only; stored in secrets, never returned)",
		"assistant.api_key_env":         "Env var holding the API key (preferred over api_key)",
		"assistant.api_key_source":      "Live key source: secret, env:NAME, well-known:NAME, or none",
		"assistant.max_tokens":          "Max output tokens per turn (default 8192)",
		"assistant.context_tokens":      "Compaction window; 0 disables (default 0)",
		"assistant.system_extra":        "Extra system text prepended to the native assistant",
		"assistant.web_search":          "Anthropic server web_search tool (ignored for openai)",
		"assistant.limits.daily_tokens": "UTC-day token cap; 0 unlimited",
		"assistant.limits.max_turns":    "Max tool turns per run; 0 uses the built-in 20",
		"email.worker":                  "trebi-mail Worker URL; present enables the email channel",
		"email.token":                   "secret:<name> holding the shared Worker token",
		"email.domain":                  "Bare hostname the workspace addresses live on",
		"email.admins":                  "Comma-separated alert contact and default inbound allowlist",
		"email.poll_interval":           "Ingress poll period (default 30s; 5s to 10m)",
		"email.require_dmarc":           "Reject inbound mail without a DMARC pass (default true)",
		"email.from_name":               "Display name on outbound mail (default Trebi)",
		"email.content_days":            "Days to keep email message text (default 45; 0 = do not keep)",
		"cloud.api_url":                 "Hosted account endpoint (default https://staging-api.trebi.ai)",
		"cloud.enabled":                 "Dial the hosted workers; false keeps a linked device offline (default true)",
		"cloud.remote_mcp":              "Allow Claude, ChatGPT, and other remote MCP clients to use this machine through Trebi Cloud (default true)",
		"cloud.mobile":                  "Allow paired phones and tablets to use this machine through Trebi Cloud (default true)",
		"connectors.registries":         `MCP registries the connector catalog reads: name=url pairs, or JSON [{"name","url","kind"}] with kind mcp-registry, smithery, or docker`,
	}
}

// bridgeResourceDescriptions documents tools.<t>.bridge.resources.<key>.
func bridgeResourceDescriptions() map[string]string {
	return map[string]string{
		"skills":               "User skills directory → harness",
		"mcp":                  "User MCP server config → harness",
		"plugins":              "User plugins tree → harness",
		"plugin_enablement":    "enabledPlugins / plugin on-off prefs",
		"user_memory":          "Global CLAUDE.md (user memory) → harness",
		"settings_prefs":       "User settings minus hooks → harness",
		"settings_local":       "Local settings overlay",
		"commands":             "User slash-commands → harness",
		"agents":               "User agent definitions → harness",
		"rules":                "User rules → harness",
		"keybindings":          "User keybindings (default off)",
		"status_line":          "Status line config (default off)",
		"claude_skills_compat": "Grok: also scan ~/.claude/skills",
		"config_prefs":         "User config prefs (non-hook) → harness",
		"marketplace":          "Grok marketplace plugins (default off)",
		"prompts":              "Codex prompts/skills-like assets",
		"config":               "OpenCode full user config (default off)",
		"settings":             "Copilot settings → harness",
		"cli_config_prefs":     "Cursor CLI config prefs (non-hook) → harness",
		"themes":               "Pi themes → harness",
		"models":               "Pi model definitions → harness",
	}
}

func applyGlobalBridgeFilters(key string, base bool, cfg *Config) bool {
	if cfg == nil || cfg.Tools.Bridge == nil {
		return base
	}
	b := cfg.Tools.Bridge
	if containsStr(b.Never, key) {
		return false
	}
	if len(b.Only) > 0 && !containsStr(b.Only, key) {
		return false
	}
	return base
}

func containsStr(ss []string, k string) bool {
	for _, s := range ss {
		if s == k {
			return true
		}
	}
	return false
}

func joinCSV(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return strings.Join(ss, ",")
}

// parseStringSlice parses value as a JSON string array, optional CSV, or empty/-.
// When allowCSV is false and value is not JSON, returns a single-element slice.
func parseStringSlice(value string, allowCSV bool) ([]string, error) {
	if value == "" || value == "-" {
		return nil, nil
	}
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "[") {
		var out []string
		if err := json.Unmarshal([]byte(value), &out); err != nil {
			return nil, fmt.Errorf("want JSON string array: %w", err)
		}
		return trimNonEmpty(out), nil
	}
	if allowCSV && strings.Contains(value, ",") {
		var out []string
		for _, p := range strings.Split(value, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out, nil
	}
	return []string{value}, nil
}

func trimNonEmpty(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// clearOrValue maps empty or "-" to "" for optional string config keys.
func clearOrValue(value string) string {
	if value == "" || value == "-" {
		return ""
	}
	return value
}

func walkYAMLPaths(n *yaml.Node, prefix string, out map[string]bool) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			out[path] = true
			walkYAMLPaths(n.Content[i+1], path, out)
		}
	}
}

func setDurationOrEmpty(dst *string, value, path string) error {
	if value == "" || value == "-" {
		*dst = ""
		return nil
	}
	ce := &ConfigError{Path: "config.yaml"}
	ce.checkDuration(path, value, true)
	if len(ce.Errors) > 0 {
		return fmt.Errorf("%s", ce.Errors[0].Msg)
	}
	*dst = value
	return nil
}

func setNudge(dst *string, value, path string) error {
	if value == "" || value == "-" {
		*dst = ""
		return nil
	}
	ce := &ConfigError{Path: "config.yaml"}
	ce.checkNudge(path, value)
	if len(ce.Errors) > 0 {
		return fmt.Errorf("%s", ce.Errors[0].Msg)
	}
	*dst = value
	return nil
}

func setIfRunning(dst *string, value, path string) error {
	if value == "" || value == "-" {
		*dst = ""
		return nil
	}
	v := strings.ToLower(value)
	if !validIfRunningMode(v) || v == "" {
		return fmt.Errorf("%s: %q not in [queue, skip, replace]", path, value)
	}
	*dst = v
	return nil
}

func setTerminal(dst *string, value string) error {
	if !validTerminal(value) {
		return fmt.Errorf("terminal: %q not in [Terminal, iTerm, Ghostty]", value)
	}
	*dst = value
	return nil
}

func setFallback(dst *string, value string) error {
	if !validFallbackMode(value) {
		return fmt.Errorf("tools.fallback: %q not in [require, path]", value)
	}
	*dst = value
	return nil
}

func setStringList(bridge **BridgeConfig, only bool, value string) error {
	if *bridge == nil {
		*bridge = &BridgeConfig{}
	}
	list, err := parseStringSlice(value, true /* csv */)
	if err != nil {
		field := "tools.bridge.never"
		if only {
			field = "tools.bridge.only"
		}
		return fmt.Errorf("%s: %w", field, err)
	}
	// validate resource names against any known key
	for _, k := range list {
		if !knownResourceAny(k) {
			return fmt.Errorf("unknown resource %q", k)
		}
	}
	if only {
		(*bridge).Only = list
	} else {
		(*bridge).Never = list
	}
	return nil
}

func parseBoolish(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%q not a boolean", value)
	}
}

func setNested(m map[string]any, parts []string, leaf any) {
	if len(parts) == 0 {
		return
	}
	if len(parts) == 1 {
		m[parts[0]] = leaf
		return
	}
	child, _ := m[parts[0]].(map[string]any)
	if child == nil {
		child = map[string]any{}
		m[parts[0]] = child
	}
	setNested(child, parts[1:], leaf)
}

// isKeyGlob reports whether p uses path.Match metacharacters.
func isKeyGlob(p string) bool {
	return strings.ContainsAny(p, "*?[")
}

// matchConfigKey matches a filter pattern against a dotted config key.
// Non-glob patterns match the key or any key under that path segment.
func matchConfigKey(pattern, key string) (bool, error) {
	if !isKeyGlob(pattern) {
		return key == pattern || strings.HasPrefix(key, pattern+"."), nil
	}
	return path.Match(pattern, key)
}
