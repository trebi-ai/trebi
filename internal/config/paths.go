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
		case "bridge.inherit":
			return fmt.Sprintf("%s-only inherit default (empty = use global)", tool)
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
		"opencode/config":
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

// ListPathEntries returns all known config paths with effective value and source.
// declared is optional set of dotted paths present in the YAML file.
func ListPathEntries(cfg *Config, declared map[string]bool) []PathEntry {
	if cfg == nil {
		cfg = defaultConfig()
		applyLoadDefaults(cfg)
	}
	if declared == nil {
		declared = map[string]bool{}
	}
	l := &pathEntryList{declared: declared}
	l.addCore(cfg)
	l.addGlobalBridge(cfg)
	for _, tool := range FirstClassTools() {
		l.addTool(cfg, tool)
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
			applyLoadDefaults(cfg)
			return cfg, declared, nil
		}
		return nil, nil, err
	}
	declared, err = DeclaredPaths(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", filepathBase(ConfigPath()), err)
	}
	if err := decodeConfig(data, cfg); err != nil {
		return nil, nil, err
	}
	applyLoadDefaults(cfg)
	if err := Validate(cfg); err != nil {
		return nil, nil, err
	}
	return cfg, declared, nil
}

// SetPath sets a known dotted config key on cfg. Does not Save.
func SetPath(cfg *Config, path, value string) error {
	if cfg == nil {
		return fmt.Errorf("nil config")
	}
	path = strings.TrimSpace(path)
	value = strings.TrimSpace(value)
	if path == "" {
		return fmt.Errorf("empty key")
	}
	if path == "terminal" {
		return setTerminal(&cfg.Terminal, value)
	}
	if handled, err := cfg.setDefaultsKey(path, value); handled {
		return err
	}
	if handled, err := cfg.setNotifyKey(path, value); handled {
		return err
	}
	if handled, err := cfg.setToolsKey(path, value); handled {
		return err
	}

	// tools.<name>.…
	parts := strings.Split(path, ".")
	if len(parts) >= 3 && parts[0] == "tools" {
		tool := parts[1]
		if !isFirstClass(tool) {
			return fmt.Errorf("unknown tool %q", tool)
		}
		rest := strings.Join(parts[2:], ".")
		return cfg.setToolKey(tool, rest, value)
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
	applyLoadDefaults(cfg)
	ents := ListPathEntries(cfg, nil)
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
	l.add("defaults.session.timeout", cfg.Defaults.Session.Timeout, "")
	l.add("defaults.session.max_duration", cfg.Defaults.Session.MaxDuration, "")
	l.add("defaults.session.keep_alive", cfg.Defaults.Session.KeepAlive, "")
	l.add("defaults.session.nudge", cfg.Defaults.Session.Nudge, "")
	l.add("defaults.session.if_running", cfg.Defaults.Session.IfRunning, "")
	l.add("terminal", cfg.Terminal, "Terminal")
	l.add("tools.fallback", cfg.Tools.Fallback, "path")
	l.add("notify.email", cfg.Notify.Email, "")
	l.add("notify.whatsapp", cfg.Notify.WhatsApp, "")
	l.add("notify.discord", cfg.Notify.Discord, "")
	// Effective value: unset → 5m (WaitingAfterDuration); keep source=default when not in YAML.
	waShow := strings.TrimSpace(cfg.Notify.WaitingAfter)
	if waShow == "" {
		waShow = "5m"
	}
	l.add("notify.waiting_after", waShow, "5m")
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

	tInherit := ""
	if p != nil && p.Bridge != nil && p.Bridge.Inherit != nil {
		tInherit = strconv.FormatBool(*p.Bridge.Inherit)
	}
	l.add(prefix+".bridge.inherit", dashIfEmpty(tInherit), "") // no single default string; empty = use global

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
	}
	return false, nil
}

// setNotifyKey handles notify.* keys; handled=false when path is not one.
func (c *Config) setNotifyKey(path, value string) (handled bool, err error) {
	switch path {
	case "notify.email":
		c.Notify.Email = clearOrValue(value)
	case "notify.whatsapp":
		c.Notify.WhatsApp = clearOrValue(value)
	case "notify.discord":
		c.Notify.Discord = clearOrValue(value)
	case "notify.waiting_after":
		return true, setNudge(&c.Notify.WaitingAfter, value, path)
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
	case "bridge.inherit":
		if err := pin.setBridgeInherit(tool, value); err != nil {
			return err
		}
	default:
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
		"defaults.session.if_running":   "Overlap policy: skip|replace|coalesce|allow",
		"terminal":                      "macOS app for jobi attach (Terminal|iTerm|Ghostty)",
		"tools.fallback":                "When pin missing: require (fail) or path (use PATH)",
		"notify.email":                  "Shell cmd for email alerts (tried first)",
		"notify.whatsapp":               "Shell cmd for WhatsApp alerts (tried second)",
		"notify.discord":                "Shell cmd for Discord alerts (tried third)",
		"notify.waiting_after":          "Escalate waiting tasks via notify chain (default 5m; off/0 disables)",
		"tools.bridge.inherit":          "Global default: inherit user harness resources",
		"tools.bridge.only":             "If set, only these resource keys may inherit",
		"tools.bridge.never":            "Resource keys force-disabled globally",
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

func dashIfEmpty(s string) string {
	if s == "" {
		return ""
	}
	return s
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

func filepathBase(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return p
	}
	return p[i+1:]
}

func isFirstClass(name string) bool {
	for _, t := range FirstClassTools() {
		if t == name {
			return true
		}
	}
	return false
}

func setDurationOrEmpty(dst *string, value, path string) error {
	if value == "" || value == "-" {
		*dst = ""
		return nil
	}
	ce := &ConfigError{Path: "config.yaml"}
	validateDurationField(ce, path, value, true)
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
	validateNudgeField(ce, path, value)
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
		return fmt.Errorf("%s: %q not in [skip, replace, coalesce, allow]", path, value)
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
