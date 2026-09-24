package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	config "github.com/trebi-ai/trebi/internal/config"
)

// EnsureOpts controls EnsureBridges behavior.
type EnsureOpts struct {
	// Force replaces non-symlink harness paths that block inherit (rm then link).
	// Without Force, a blocked path is returned as an error.
	Force bool
}

// ErrBridgeBlocked is returned when inherit cannot replace a real harness path.
var ErrBridgeBlocked = errors.New("bridge blocked: harness path is not a symlink")

// BridgeKind classifies how a resource is applied.
type BridgeKind string

const (
	KindSymlinkDir      BridgeKind = "symlink_dir"
	KindSymlinkFile     BridgeKind = "symlink_file"
	KindMCPClaude       BridgeKind = "mcp_claude"
	KindSettingsPrefs   BridgeKind = "settings_prefs"
	KindConfigTOMLPrefs BridgeKind = "config_toml_prefs"
	KindOpenCodePlugins BridgeKind = "opencode_plugins"
	KindNoop            BridgeKind = "noop"
)

// ResourceDef is one catalog entry for a tool.
type ResourceDef struct {
	Key  string
	Kind BridgeKind
	// UserRel is path under user home (e.g. ".claude/skills").
	UserRel string
	// HarnessRel is path under harness dir (e.g. "skills").
	HarnessRel string
}

// paths returns the user source and harness destination for the resource.
func (d ResourceDef) paths(userHome, harnessDir string) (src, dst string) {
	if d.UserRel != "" && userHome != "" {
		src = filepath.Join(userHome, d.UserRel)
	}
	if d.HarnessRel != "" {
		dst = filepath.Join(harnessDir, d.HarnessRel)
	}
	return src, dst
}

// apply materializes one resource for tool. KindSettingsPrefs and KindNoop
// are handled elsewhere and do nothing here.
func (d ResourceDef) apply(tool string, inherit bool, userHome, harnessDir string, force bool) error {
	src, dst := d.paths(userHome, harnessDir)
	switch d.Kind {
	case KindSymlinkDir, KindSymlinkFile:
		return applySymlink(d.Kind, inherit, src, dst, force)
	case KindMCPClaude:
		return applyMCPClaude(inherit, userHome, harnessDir, force)
	case KindConfigTOMLPrefs:
		return applyConfigTOMLPrefs(inherit, src, dst, tool)
	case KindOpenCodePlugins:
		return applyOpenCodePlugins(inherit, userHome, harnessDir, force)
	case KindSettingsPrefs, KindNoop:
		// settings_prefs merged once per tool; noop recorded in status only.
		return nil
	}
	return nil
}

// BridgeStatusRow is desired policy vs disk for one resource.
type BridgeStatusRow struct {
	Tool     string `json:"tool"`
	Resource string `json:"resource"`
	Policy   string `json:"policy"` // inherit | off
	Status   string `json:"status"` // ok | missing-source | blocked | off | error | noop
	Detail   string `json:"detail,omitempty"`
	Source   string `json:"source,omitempty"`
	Dest     string `json:"dest,omitempty"`
}

// Roots lets tests inject user home / harness base without touching real homes.
type Roots struct {
	UserHome    string
	HarnessBase string // parent of <tool>; default config.BaseDir()/harness
}

// withDefaults fills empty roots with the process defaults.
func (r Roots) withDefaults() Roots {
	if r.UserHome == "" {
		r.UserHome = UserHome()
	}
	if r.HarnessBase == "" {
		r.HarnessBase = filepath.Join(config.BaseDir(), "harness")
	}
	return r
}

// ResolveInherit returns whether resource key should inherit for tool.
// Policy lives in config.ResolveResourceInherit (single source of truth).
func ResolveInherit(tool, key string, cfg *config.Config) bool {
	return config.ResolveResourceInherit(tool, key, cfg)
}

// EnsureBridges applies the catalog for tool (no force). Blocked real paths return an error;
// missing user sources are soft (skipped). Callers that must not fail the launch (prepare)
// may ignore the error after logging.
func EnsureBridges(tool string, cfg *config.Config) error {
	return EnsureBridgesOpts(tool, cfg, Roots{}.withDefaults(), EnsureOpts{})
}

// EnsureBridgesOpts is EnsureBridges with injectable roots and options.
func EnsureBridgesOpts(tool string, cfg *config.Config, roots Roots, opts EnsureOpts) error {
	return ensureBridges(tool, cfg, roots, opts)
}

// EnsureBridgesRoots is EnsureBridges with injectable roots (tests).
func EnsureBridgesRoots(tool string, cfg *config.Config, roots Roots) error {
	return ensureBridges(tool, cfg, roots, EnsureOpts{})
}

// BridgeStatusRoots is BridgeStatus with injectable roots.
func BridgeStatusRoots(tool string, cfg *config.Config, roots Roots) []BridgeStatusRow {
	var tools []string
	if tool != "" {
		tools = []string{tool}
	} else {
		tools = []string{"claude", "grok", "codex", "opencode", "opencode2", "cursor", "pi"}
	}
	roots = roots.withDefaults()
	var rows []BridgeStatusRow
	for _, t := range tools {
		if t == "copilot" {
			continue
		}
		harnessDir := filepath.Join(roots.HarnessBase, t)
		for _, def := range Catalog(t) {
			inherit := ResolveInherit(t, def.Key, cfg)
			pol := "off"
			if inherit {
				pol = "inherit"
			}
			src, dst := def.paths(roots.UserHome, harnessDir)
			row := BridgeStatusRow{
				Tool: t, Resource: def.Key, Policy: pol,
				Source: src, Dest: dst,
			}
			if !inherit {
				row.Status = "off"
				rows = append(rows, row)
				continue
			}
			if def.Kind == KindNoop {
				row.Status = "noop"
				rows = append(rows, row)
				continue
			}
			row.Status, row.Detail = def.probeStatus(src, dst, harnessDir, roots.UserHome)
			rows = append(rows, row)
		}
	}
	return rows
}

// Catalog returns resource definitions for a tool.
func Catalog(tool string) []ResourceDef {
	switch tool {
	case "claude":
		return claudeCatalog()
	case "grok":
		return grokCatalog()
	case "codex":
		return codexCatalog()
	case "opencode", "opencode2":
		return opencodeCatalog()
	case "copilot":
		return copilotCatalog()
	case "cursor":
		return cursorCatalog()
	case "pi":
		return piCatalog()
	default:
		return nil
	}
}

func ensureBridges(tool string, cfg *config.Config, roots Roots, opts EnsureOpts) error {
	if tool == "copilot" {
		// Dormant until R6 isolation.
		return nil
	}
	defs := Catalog(tool)
	if len(defs) == 0 {
		return nil
	}
	roots = roots.withDefaults()
	harnessDir := filepath.Join(roots.HarnessBase, tool)
	if err := EnsureDir(harnessDir); err != nil {
		return err
	}

	// settings-related modes collected for Claude settings merge
	var settingsInherit, pluginEn, statusLine bool
	if tool == "claude" {
		settingsInherit, pluginEn, statusLine = claudeSettingsModes(cfg, defs)
	}

	var errs []error
	for _, def := range defs {
		inherit := ResolveInherit(tool, def.Key, cfg)
		if err := def.apply(tool, inherit, roots.UserHome, harnessDir, opts.Force); err != nil {
			// No owning struct; stamp source inline (documented Phase 4.3 exception).
			slog.Warn("bridge", "source", "harness", "tool", tool, "resource", def.Key, "error", err)
			hint := ""
			if errors.Is(err, ErrBridgeBlocked) && !opts.Force {
				hint = "; re-run with --force to replace harness path"
			}
			errs = append(errs, fmt.Errorf("%s: %w%s", def.Key, err, hint))
		}
	}

	if tool == "claude" && settingsInherit {
		if err := applyClaudeSettingsPrefs(roots.UserHome, harnessDir, pluginEn, statusLine); err != nil {
			slog.Warn("bridge settings_prefs", "source", "harness", "error", err)
			errs = append(errs, fmt.Errorf("settings_prefs: %w", err))
		}
	}

	return errors.Join(errs...)
}

// claudeSettingsModes resolves the settings-merge policies for claude.
func claudeSettingsModes(cfg *config.Config, defs []ResourceDef) (settingsInherit, pluginEn, statusLine bool) {
	for _, d := range defs {
		switch d.Key {
		case "settings_prefs":
			settingsInherit = ResolveInherit("claude", d.Key, cfg)
		case "plugin_enablement":
			pluginEn = ResolveInherit("claude", d.Key, cfg)
		case "status_line":
			statusLine = ResolveInherit("claude", d.Key, cfg)
		}
	}
	return settingsInherit, pluginEn, statusLine
}

func (d ResourceDef) probeStatus(src, dst, harnessDir, userHome string) (status, detail string) {
	switch d.Kind {
	case KindSymlinkDir, KindSymlinkFile:
		return probeSymlink(src, dst)
	case KindMCPClaude:
		mcp := filepath.Join(userHome, ".claude", "mcp.json")
		if pathExists(mcp) {
			return probeSymlink(mcp, filepath.Join(harnessDir, "mcp.json"))
		}
		if pathExists(filepath.Join(userHome, ".claude.json")) {
			return "ok", "mcpServers merge source present"
		}
		return "missing-source", "no mcp.json or .claude.json"
	case KindSettingsPrefs:
		u := filepath.Join(userHome, ".claude", "settings.json")
		if !pathExists(u) {
			return "missing-source", ""
		}
		return "ok", ""
	case KindConfigTOMLPrefs:
		if src != "" && !pathExists(src) {
			return "missing-source", ""
		}
		return "ok", ""
	case KindOpenCodePlugins:
		if src != "" && !pathExists(src) {
			return "missing-source", ""
		}
		return "ok", ""
	default:
		return "noop", ""
	}
}

// probeSymlink reports whether dst is a symlink to src.
func probeSymlink(src, dst string) (status, detail string) {
	if src == "" || !pathExists(src) {
		return "missing-source", "user source missing"
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		return "missing", "dest not linked"
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return "blocked", "harness path is not a symlink"
	}
	target, _ := os.Readlink(dst)
	if target == src {
		return "ok", ""
	}
	return "error", "symlink points elsewhere: " + target
}

func applySymlink(kind BridgeKind, inherit bool, src, dst string, force bool) error {
	if dst == "" {
		return nil
	}
	if !inherit {
		return removeTrebiSymlink(dst)
	}
	if src == "" || !pathExists(src) {
		return nil // missing source — soft
	}
	if fi, err := os.Lstat(dst); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			if !force {
				return fmt.Errorf("%w: %s", ErrBridgeBlocked, dst)
			}
			// --force: remove real file/dir so we can own the path as a symlink.
			if err := os.RemoveAll(dst); err != nil {
				return fmt.Errorf("force remove %s: %w", dst, err)
			}
		} else {
			target, _ := os.Readlink(dst)
			if target == src {
				return nil
			}
			if err := os.Remove(dst); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if err := os.Symlink(src, dst); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", dst, src, err)
	}
	return nil
}

func removeTrebiSymlink(dst string) error {
	fi, err := os.Lstat(dst)
	if err != nil {
		return nil
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		// real file/dir — leave alone
		return nil
	}
	return os.Remove(dst)
}

func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func applyMCPClaude(inherit bool, userHome, harnessDir string, force bool) error {
	userMCP := filepath.Join(userHome, ".claude", "mcp.json")
	harnessMCP := filepath.Join(harnessDir, "mcp.json")
	if !inherit {
		_ = removeTrebiSymlink(harnessMCP) // leftover symlink is harmless if inherit is off
		// leave harness .claude.json mcpServers as-is (v1)
		return nil
	}
	var linkErr error
	if pathExists(userMCP) {
		if err := applySymlink(KindSymlinkFile, true, userMCP, harnessMCP, force); err != nil {
			linkErr = err
		}
	}
	// Merge mcpServers from ~/.claude.json into harness .claude.json
	userClaudeJSON := filepath.Join(userHome, ".claude.json")
	if err := mergeMCPServers(userClaudeJSON, filepath.Join(harnessDir, ".claude.json")); err != nil {
		return errors.Join(linkErr, err)
	}
	return linkErr
}

func mergeMCPServers(userClaudeJSON, harnessClaudeJSON string) error {
	userServers := map[string]any{}
	if b, err := os.ReadFile(userClaudeJSON); err == nil {
		var u map[string]any
		if json.Unmarshal(b, &u) == nil {
			if m, ok := u["mcpServers"].(map[string]any); ok {
				userServers = m
			}
		}
	}
	if len(userServers) == 0 {
		return nil
	}
	m := map[string]any{}
	if b, err := os.ReadFile(harnessClaudeJSON); err == nil {
		_ = json.Unmarshal(b, &m) // corrupt harness json: start from empty map
		if m == nil {
			m = map[string]any{}
		}
	}
	existing, _ := m["mcpServers"].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	for k, v := range userServers {
		if _, ok := existing[k]; !ok {
			existing[k] = v
		}
	}
	// User keys win on conflict with empty — if key exists keep harness; if only in user add.
	// Spec: "user keys win on conflict with empty harness; harness-only keys kept"
	// Re-read: user keys win on conflict. So overwrite with user.
	for k, v := range userServers {
		existing[k] = v
	}
	m["mcpServers"] = existing
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return WriteFileIfChanged(harnessClaudeJSON, b, 0600)
}

func applyClaudeSettingsPrefs(userHome, harnessDir string, pluginEnablement, statusLine bool) error {
	userPath := filepath.Join(userHome, ".claude", "settings.json")
	harnessPath := filepath.Join(harnessDir, "settings.json")
	// Never touch harnessDir/trebi-settings.json.

	ub, err := os.ReadFile(userPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var user map[string]any
	if err := json.Unmarshal(ub, &user); err != nil {
		return err
	}
	h := map[string]any{}
	if hb, err := os.ReadFile(harnessPath); err == nil {
		_ = json.Unmarshal(hb, &h) // corrupt settings: start from empty map
		if h == nil {
			h = map[string]any{}
		}
	}
	// Copy selected prefs excluding hooks always.
	for k, v := range user {
		if k == "hooks" {
			continue
		}
		if k == "enabledPlugins" && !pluginEnablement {
			continue
		}
		if k == "statusLine" && !statusLine {
			continue
		}
		h[k] = v
	}
	delete(h, "hooks")
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return WriteFileIfChanged(harnessPath, b, 0644)
}

// applyConfigTOMLPrefs merges a small allowlist of user TOML prefs into the
// harness config without clobbering trebi-owned sections (Grok hooks flags, etc.).
func applyConfigTOMLPrefs(inherit bool, userPath, harnessPath, tool string) error {
	if harnessPath == "" {
		return nil
	}
	hb, err := os.ReadFile(harnessPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	out := string(hb)
	prefs := map[string]string{}
	if inherit && userPath != "" {
		ub, err := os.ReadFile(userPath)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			prefs = userTOMLPrefs(ub, tool)
		}
	}
	if tool != "grok" && len(prefs) == 0 {
		return nil
	}
	if out == "" {
		out = "# trebi harness config\n"
	}
	if tool == "grok" {
		out = ensureGrokConfigTOML(out)
	}
	if len(prefs) > 0 {
		out = appendMissingTOMLPrefs(out, prefs)
	}
	return WriteFileIfChanged(harnessPath, []byte(out), 0644)
}

// grokTrebiMCPBlock is the isolated-GROK_HOME MCP server. Stdio (not HTTP)
// so a daemon port change does not drop the tools mid-session.
const grokTrebiMCPBlock = `[mcp_servers.trebi]
command = "trebi"
args = ["mcp", "serve"]
`

// ensureGrokConfigTOML keeps trebi-owned grok flags valid. Grok rewrites
// [harness] on its own; appending a second table makes the file unparsable.
func ensureGrokConfigTOML(out string) string {
	out = mergeDuplicateTOMLSection(out, "harness")
	if !strings.Contains(out, "hooks") {
		out += "\n[compat.claude]\nhooks = false\n"
	}
	out = upsertTOMLKey(out, "harness", "disable_codebase_upload", "true")
	return ensureGrokTrebiMCP(out)
}

// ensureGrokTrebiMCP adds the trebi MCP stdio bridge when missing. An existing
// [mcp_servers.trebi] table (HTTP url, custom command) is left alone.
func ensureGrokTrebiMCP(out string) string {
	if strings.Contains(out, "[mcp_servers.trebi]") {
		return out
	}
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + "\n" + grokTrebiMCPBlock
}

// mergeDuplicateTOMLSection folds repeated [section] tables into the first.
func mergeDuplicateTOMLSection(in, section string) string {
	header := "[" + section + "]"
	type block struct {
		header string
		lines  []string
	}
	var blocks []block
	cur := block{}
	flush := func() {
		blocks = append(blocks, cur)
		cur = block{}
	}
	for _, line := range strings.Split(in, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {
			flush()
			cur.header = trim
			continue
		}
		cur.lines = append(cur.lines, line)
	}
	flush()
	n := 0
	for _, b := range blocks {
		if b.header == header {
			n++
		}
	}
	if n < 2 {
		return in
	}
	first := -1
	var merged []block
	for _, b := range blocks {
		if b.header == header {
			if first < 0 {
				merged = append(merged, b)
				first = len(merged) - 1
				continue
			}
			merged[first].lines = append(merged[first].lines, b.lines...)
			continue
		}
		merged = append(merged, b)
	}
	var b strings.Builder
	for i, blk := range merged {
		if i > 0 {
			b.WriteByte('\n')
		}
		if blk.header != "" {
			b.WriteString(blk.header)
			b.WriteByte('\n')
		}
		b.WriteString(strings.Join(blk.lines, "\n"))
	}
	out := b.String()
	if strings.HasSuffix(in, "\n") && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

// upsertTOMLKey sets key in [section], creating the table if needed. It never
// appends a second copy of the table.
func upsertTOMLKey(out, section, key, value string) string {
	if strings.Contains(out, key+" =") || strings.Contains(out, key+"=") {
		return out
	}
	header := "[" + section + "]"
	line := key + " = " + value
	idx := indexTOMLHeader(out, header)
	if idx < 0 {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		return out + "\n" + header + "\n" + line + "\n"
	}
	rest := idx + len(header)
	if rest < len(out) && out[rest] == '\n' {
		rest++
	}
	return out[:rest] + line + "\n" + out[rest:]
}

func indexTOMLHeader(out, header string) int {
	searchFrom := 0
	for {
		i := strings.Index(out[searchFrom:], header)
		if i < 0 {
			return -1
		}
		i += searchFrom
		atLineStart := i == 0 || out[i-1] == '\n'
		after := i + len(header)
		atLineEnd := after == len(out) || out[after] == '\n' || out[after] == '\r'
		if atLineStart && atLineEnd {
			return i
		}
		searchFrom = i + 1
	}
}

// userTOMLPrefs scans user TOML and collects the allowlisted prefs.
func userTOMLPrefs(data []byte, tool string) map[string]string {
	prefs := map[string]string{}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {
			section = strings.Trim(trim, "[]")
			continue
		}
		k, v, ok := strings.Cut(trim, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "permission_mode", "auto_compact_threshold_percent", "model":
			prefs[k] = v
		}
		if section == "models" && k == "default" {
			prefs["models.default"] = v
		}
		// Codex often uses model = "..." at top level (already covered).
		if tool == "codex" && (k == "model" || k == "notify") {
			// never import notify — trebi injects its own
			if k == "notify" {
				continue
			}
			prefs[k] = v
		}
	}
	return prefs
}

// appendMissingTOMLPrefs appends prefs that the harness config does not set yet.
func appendMissingTOMLPrefs(out string, prefs map[string]string) string {
	for k, v := range prefs {
		if k == "models.default" {
			if !strings.Contains(out, "[models]") {
				out += "\n[models]\ndefault = " + v + "\n"
			} else if !strings.Contains(out, "default =") {
				out += "default = " + v + "\n"
			}
			continue
		}
		marker := k + " ="
		if !strings.Contains(out, marker) && !strings.Contains(out, k+"=") {
			out += "\n" + k + " = " + v + "\n"
		}
	}
	return out
}

func applyOpenCodePlugins(inherit bool, userHome, harnessDir string, force bool) error {
	userPlugins := filepath.Join(userHome, ".config", "opencode", "plugins")
	harnessPlugins := filepath.Join(harnessDir, "plugins")
	trebiPlugin := "trebi-lifecycle.js"
	if !inherit {
		// Remove only symlinks in harness plugins that point into user tree.
		entries, err := os.ReadDir(harnessPlugins)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if e.Name() == trebiPlugin {
				continue
			}
			p := filepath.Join(harnessPlugins, e.Name())
			fi, err := os.Lstat(p)
			if err != nil || fi.Mode()&os.ModeSymlink == 0 {
				continue
			}
			target, _ := os.Readlink(p)
			if strings.HasPrefix(target, userPlugins) {
				_ = os.Remove(p) // stale plugin symlink; skip and continue the sweep
			}
		}
		return nil
	}
	if !pathExists(userPlugins) {
		return nil
	}
	if err := EnsureDir(harnessPlugins); err != nil {
		return err
	}
	entries, err := os.ReadDir(userPlugins)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.Name() == trebiPlugin {
			continue
		}
		src := filepath.Join(userPlugins, e.Name())
		dst := filepath.Join(harnessPlugins, e.Name())
		if err := applySymlink(KindSymlinkFile, true, src, dst, force); err != nil {
			// directories may need symlink_dir semantics; applySymlink handles both.
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
		}
	}
	return errors.Join(errs...)
}

func claudeCatalog() []ResourceDef {
	return []ResourceDef{
		{Key: "skills", Kind: KindSymlinkDir, UserRel: ".claude/skills", HarnessRel: "skills"},
		{Key: "mcp", Kind: KindMCPClaude},
		{Key: "plugins", Kind: KindSymlinkDir, UserRel: ".claude/plugins", HarnessRel: "plugins"},
		{Key: "plugin_enablement", Kind: KindNoop}, // folded into settings_prefs
		{Key: "user_memory", Kind: KindSymlinkFile, UserRel: ".claude/CLAUDE.md", HarnessRel: "CLAUDE.md"},
		{Key: "settings_prefs", Kind: KindSettingsPrefs, UserRel: ".claude/settings.json", HarnessRel: "settings.json"},
		{Key: "settings_local", Kind: KindSymlinkFile, UserRel: ".claude/settings.local.json", HarnessRel: "settings.local.json"},
		{Key: "commands", Kind: KindSymlinkDir, UserRel: ".claude/commands", HarnessRel: "commands"},
		{Key: "agents", Kind: KindSymlinkDir, UserRel: ".claude/agents", HarnessRel: "agents"},
		{Key: "rules", Kind: KindSymlinkDir, UserRel: ".claude/rules", HarnessRel: "rules"},
		{Key: "keybindings", Kind: KindSymlinkFile, UserRel: ".claude/keybindings.json", HarnessRel: "keybindings.json"},
		{Key: "status_line", Kind: KindNoop},
	}
}

func grokCatalog() []ResourceDef {
	return []ResourceDef{
		{Key: "skills", Kind: KindSymlinkDir, UserRel: ".grok/skills", HarnessRel: "skills"},
		{Key: "claude_skills_compat", Kind: KindNoop},
		{Key: "config_prefs", Kind: KindConfigTOMLPrefs, UserRel: ".grok/config.toml", HarnessRel: "config.toml"},
		{Key: "marketplace", Kind: KindNoop},
		{Key: "plugins", Kind: KindSymlinkDir, UserRel: ".grok/installed-plugins", HarnessRel: "installed-plugins"},
	}
}

func codexCatalog() []ResourceDef {
	return []ResourceDef{
		{Key: "config_prefs", Kind: KindConfigTOMLPrefs, UserRel: ".codex/config.toml", HarnessRel: "config.toml"},
		{Key: "skills", Kind: KindSymlinkDir, UserRel: ".codex/skills", HarnessRel: "skills"},
		{Key: "prompts", Kind: KindSymlinkDir, UserRel: ".codex/prompts", HarnessRel: "prompts"},
		{Key: "mcp", Kind: KindSymlinkFile, UserRel: ".codex/mcp.json", HarnessRel: "mcp.json"},
	}
}

func opencodeCatalog() []ResourceDef {
	return []ResourceDef{
		{Key: "plugins", Kind: KindOpenCodePlugins, UserRel: ".config/opencode/plugins", HarnessRel: "plugins"},
		{Key: "config", Kind: KindNoop}, // never full replace
		{Key: "skills", Kind: KindSymlinkDir, UserRel: ".config/opencode/skills", HarnessRel: "skills"},
	}
}

func copilotCatalog() []ResourceDef {
	// Dormant until R6.
	return []ResourceDef{
		{Key: "settings", Kind: KindSymlinkFile, UserRel: ".copilot/settings.json", HarnessRel: "settings.json"},
		{Key: "skills", Kind: KindSymlinkDir, UserRel: ".copilot/skills", HarnessRel: "skills"},
	}
}

func cursorCatalog() []ResourceDef {
	// Under CURSOR_CONFIG_DIR isolation; inherit capability resources from ~/.cursor.
	// Sessions/chats stay harness-local (not listed). cli-config is trebi-seeded — prefs
	// inheritance is opt-in via cli_config_prefs (noop in v1 to avoid clobbering hooks).
	// Lifecycle hooks are merged into ~/.cursor/hooks.json by ensureCursorUserHooks
	// (Cursor Agent CLI does not load hooks from CURSOR_CONFIG_DIR).
	return []ResourceDef{
		{Key: "mcp", Kind: KindSymlinkFile, UserRel: ".cursor/mcp.json", HarnessRel: "mcp.json"},
		{Key: "rules", Kind: KindSymlinkDir, UserRel: ".cursor/rules", HarnessRel: "rules"},
		{Key: "skills", Kind: KindSymlinkDir, UserRel: ".cursor/skills-cursor", HarnessRel: "skills-cursor"},
		{Key: "cli_config_prefs", Kind: KindNoop},
	}
}

func piCatalog() []ResourceDef {
	// Under PI_CODING_AGENT_DIR isolation (default ~/.pi/agent).
	// Lifecycle extension is trebi-materialized under extensions/ — not bridged.
	// settings.json is trebi-seeded; prefs inherit is opt-in noop to avoid clobber.
	// mcp is owned by tool_pi.ensurePiMcpConfig (merge user servers + host imports;
	// pure symlink would block hostConfigDiscovery / imports).
	return []ResourceDef{
		{Key: "skills", Kind: KindSymlinkDir, UserRel: ".pi/agent/skills", HarnessRel: "skills"},
		{Key: "prompts", Kind: KindSymlinkDir, UserRel: ".pi/agent/prompts", HarnessRel: "prompts"},
		{Key: "themes", Kind: KindSymlinkDir, UserRel: ".pi/agent/themes", HarnessRel: "themes"},
		{Key: "mcp", Kind: KindNoop, UserRel: ".pi/agent/mcp.json", HarnessRel: "mcp.json"},
		{Key: "models", Kind: KindSymlinkFile, UserRel: ".pi/agent/models.json", HarnessRel: "models.json"},
		{Key: "settings_prefs", Kind: KindNoop},
	}
}
