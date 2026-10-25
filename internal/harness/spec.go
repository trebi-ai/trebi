package harness

import (
	"errors"
	"fmt"
)

// Spec is the fixed data of one known harness.
type Spec struct {
	Name        string // "grok"
	Label       string // "Grok"
	Vendor      string // "xAI"
	UserDir     string // ".grok", used to find a terminal login
	Installable bool   // the bin store can download it
	Hidden      bool   // Advanced mode only
	Files       []ConfigFile
}

// ConfigFile is one vendor config file that the user can let Trebi own.
type ConfigFile struct {
	Key      string // bridge resource key: "settings_prefs", "user_memory"
	Label    string // "Settings", "Instructions for every run"
	UserRel  string // under the user home: ".claude/settings.json"
	TrebiRel string // under the harness folder: "settings.json"
	Format   string // json | toml | markdown
	Inherit  string // how "Yours" works: linked, copied, or read (the CLI reads it)
}

// Specs lists the known harnesses in display order. The bin store downloads
// nothing today, so no harness is installable.
func Specs() []Spec {
	return []Spec{
		{Name: "claude", Label: "Claude Code", Vendor: "Anthropic", UserDir: ".claude", Files: []ConfigFile{
			{Key: "settings_prefs", Label: "Settings", UserRel: ".claude/settings.json", TrebiRel: "settings.json", Format: "json", Inherit: "copied"},
			{Key: "user_memory", Label: "Instructions for every run", UserRel: ".claude/CLAUDE.md", TrebiRel: "CLAUDE.md", Format: "markdown", Inherit: "linked"},
			{Key: "settings_local", Label: "Local settings", UserRel: ".claude/settings.local.json", TrebiRel: "settings.local.json", Format: "json", Inherit: "linked"},
			{Key: "mcp", Label: "MCP servers", UserRel: ".claude/mcp.json", TrebiRel: "mcp.json", Format: "json", Inherit: "linked"},
		}},
		{Name: "codex", Label: "Codex", Vendor: "OpenAI", UserDir: ".codex", Files: []ConfigFile{
			{Key: "config_prefs", Label: "Settings", UserRel: ".codex/config.toml", TrebiRel: "config.toml", Format: "toml", Inherit: "copied"},
			{Key: "user_memory", Label: "Instructions for every run", UserRel: ".codex/AGENTS.md", TrebiRel: "AGENTS.md", Format: "markdown", Inherit: "linked"},
		}},
		{Name: "grok", Label: "Grok", Vendor: "xAI", UserDir: ".grok", Files: []ConfigFile{
			{Key: "config_prefs", Label: "Settings", UserRel: ".grok/config.toml", TrebiRel: "config.toml", Format: "toml", Inherit: "copied"},
			{Key: "user_memory", Label: "Instructions for every run", UserRel: ".grok/AGENTS.md", TrebiRel: "AGENTS.md", Format: "markdown", Inherit: "linked"},
		}},
		{Name: "opencode", Label: "OpenCode", Vendor: "OpenCode", UserDir: ".local/share/opencode", Files: opencodeFiles()},
		{Name: "copilot", Label: "GitHub Copilot", Vendor: "GitHub", UserDir: ".copilot"},
		{Name: "cursor", Label: "Cursor", Vendor: "Cursor", UserDir: ".cursor"},
		{Name: "pi", Label: "Pi", Vendor: "Pi", UserDir: ".pi"},
		{Name: "omp", Label: "Oh My Pi", Vendor: "Oh My Pi", UserDir: ".omp"},
		// opencode2 is the OpenCode 2 preview; gemini and fake have no config
		// field. fake is the eval stand-in (cmd/fake).
		{Name: "opencode2", Label: "OpenCode 2", Vendor: "OpenCode", UserDir: ".local/share/opencode", Hidden: true},
		{Name: "gemini", Label: "Gemini", Vendor: "Google", UserDir: ".gemini", Hidden: true},
		{Name: "fake", Label: "Fake", Vendor: "Trebi", Hidden: true},
	}
}

// opencodeFiles are read by OpenCode itself in both modes: Trebi's copy adds
// to the user file, and its keys win.
func opencodeFiles() []ConfigFile {
	return []ConfigFile{
		{Key: "config", Label: "Settings", UserRel: ".config/opencode/opencode.json", TrebiRel: OpenCodeOwnConfig, Format: "json", Inherit: "read"},
		{Key: "user_memory", Label: "Instructions for every run", UserRel: ".config/opencode/AGENTS.md", TrebiRel: "AGENTS.md", Format: "markdown", Inherit: "read"},
	}
}

// OpenCodeOwnConfig is Trebi's copy of the OpenCode user config. Trebi's own
// opencode.json (the run plugin) stays separate.
const OpenCodeOwnConfig = "opencode.own.json"

// SpecFor returns the spec of name.
func SpecFor(name string) (Spec, bool) {
	for _, s := range Specs() {
		if s.Name == name {
			return s, true
		}
	}
	return Spec{}, false
}

// OffMessage is the error text for a job or run on a harness that is off.
func OffMessage(name string) string {
	label := name
	if s, ok := SpecFor(name); ok {
		label = s.Label
	}
	return label + " is off. Turn it on in Settings → Harnesses."
}

// ErrNoPrintMode means a harness has no proven one-shot print mode.
var ErrNoPrintMode = errors.New("harness has no print mode for decisions")

// PrintArgs is the argv of a one-shot call with no tools, no MCP, and no
// skills. The prompt goes on stdin. lastMessage is the file where codex
// writes the reply. No entry skips a permission prompt; with no tools, none
// is needed.
func PrintArgs(name, model, lastMessage string) ([]string, error) {
	var args []string
	switch name {
	case "claude":
		args = []string{"-p", "--output-format", "json", "--tools", "", "--strict-mcp-config", "--disable-slash-commands"}
	case "codex":
		args = []string{"exec", "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "--color", "never",
			"--output-last-message", lastMessage}
	default:
		return nil, fmt.Errorf("%w: %s", ErrNoPrintMode, name)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if name == "codex" {
		args = append(args, "-")
	}
	return args, nil
}
