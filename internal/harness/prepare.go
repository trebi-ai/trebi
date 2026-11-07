// Package harness keeps the auth state and the settings of the harnesses.
// The agent-visor owns their installs, logins, and homes.
package harness

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"

	config "github.com/trebi-ai/trebi/internal/config"
)

// AuthModeShared uses the user's own login.
const AuthModeShared = "shared"

// AuthModeSeparate keeps a login of its own in the harness home.
const AuthModeSeparate = "separate"

// EnsureDir creates dir with 0755 if missing.
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0755)
}

// WriteFileIfChanged writes data at path with mode; skips when identical.
func WriteFileIfChanged(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// UserHome returns the user home directory or empty.
func UserHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// AuthModeFromConfig returns shared|separate for tool from loaded config.
func AuthModeFromConfig(cfg *config.Config, tool string) string {
	if cfg == nil {
		return AuthModeShared
	}
	return cfg.Tools.AuthMode(tool)
}

// LoadSecretEnv reads KEY=value lines from ~/.trebi/secrets/<tool>.env.
// Values are never logged by this function.
func LoadSecretEnv(tool string) (map[string]string, error) {
	path := config.SecretEnvPath(tool)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if k != "" {
			out[k] = v
		}
	}
	return out, nil
}

// WriteSecretEnv writes KEY=value pairs with mode 0600 under secrets/.
func WriteSecretEnv(tool string, env map[string]string) error {
	if err := os.MkdirAll(config.SecretsDir(), 0700); err != nil {
		return err
	}
	var b strings.Builder
	for k, v := range env {
		if k == "" {
			continue
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	path := config.SecretEnvPath(tool)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RemoveSecretEnv deletes the secrets env file if present.
func RemoveSecretEnv(tool string) error {
	err := os.Remove(config.SecretEnvPath(tool))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Info is a harness status row.
type Info struct {
	Name          string `json:"name"`
	Label         string `json:"label"`
	Vendor        string `json:"vendor"`
	Base          string `json:"base,omitempty"` // set on a variant only
	Home          string `json:"home"`
	BinPath       string `json:"bin_path,omitempty"`
	Enabled       bool   `json:"enabled"`
	Installed     bool   `json:"installed"`      // binary in the bin store or on PATH
	TerminalLogin bool   `json:"terminal_login"` // a vendor login in the user home
	Installable   bool   `json:"installable"`
	Hidden        bool   `json:"hidden,omitempty"`
}

// Locator finds a harness binary and a terminal login. agentcli implements it.
type Locator interface {
	Locate(name string) (binPath string)
	TerminalLogin(name string) bool
}

// Names returns the built-in harness names.
func Names() []string {
	specs := Specs()
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// AllNames returns the built-in harness names, then the variants in name
// order.
func AllNames(cfg *config.Config) []string {
	return append(Names(), variantNames(cfg)...)
}

// List returns one row for each built-in harness, then one for each variant.
func List(cfg *config.Config, loc Locator) []Info {
	specs := Specs()
	out := make([]Info, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.info(cfg, loc, s.Name))
	}
	for _, name := range variantNames(cfg) {
		s, _ := SpecFor(cfg.HarnessBase(name))
		info := s.info(cfg, loc, name)
		info.Name, info.Base = name, s.Name
		info.Label = cmp.Or(cfg.HarnessLabel(name), name)
		info.Installable, info.Hidden = false, false
		out = append(out, info)
	}
	return out
}

// info is the row of name, which runs the program of s.
func (s Spec) info(cfg *config.Config, loc Locator, name string) Info {
	info := Info{
		Name: name, Label: s.Label, Vendor: s.Vendor,
		Home:        config.HarnessDir(name),
		Enabled:     cfg.HarnessEnabled(name),
		Installable: s.Installable,
		Hidden:      s.Hidden,
	}
	if loc != nil {
		info.BinPath = loc.Locate(s.Name)
		info.Installed = info.BinPath != ""
		info.TerminalLogin = loc.TerminalLogin(s.Name)
	}
	return info
}

// Known reports whether name is a built-in harness or a variant in
// config.yaml. Callers that build a path from request input must check this
// first: HarnessDir joins the name, so an unchecked "../.." escapes
// TREBI_HOME. A variant name passes ValidVariantName, so it is safe too.
func Known(name string) bool {
	if slices.Contains(Names(), name) {
		return true
	}
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	return IsVariant(cfg, name)
}

// IsVariant reports whether name is a variant in cfg.
func IsVariant(cfg *config.Config, name string) bool {
	return slices.Contains(variantNames(cfg), name)
}

// Label is the display name of a harness.
func Label(name string) string {
	if s, ok := SpecFor(name); ok {
		return s.Label
	}
	if cfg, err := config.Load(); err == nil && IsVariant(cfg, name) {
		return cmp.Or(cfg.HarnessLabel(name), name)
	}
	return name
}

// variantNames lists the variants in config.yaml whose base is known.
func variantNames(cfg *config.Config) []string {
	var out []string
	for name, base := range cfg.VariantBases() {
		if _, ok := SpecFor(base); ok && config.ValidVariantName(name) == nil {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
