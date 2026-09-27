package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Registry is one MCP registry endpoint. A user may add any registry that
// implements the MCP Registry API, including a private one. The catalog
// queries the list in order.
type Registry struct {
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url" json:"url"`
}

// ConnectorsConfig holds the connector catalog settings. The only key is the
// ordered registry list; an empty list means the built-in default.
type ConnectorsConfig struct {
	Registries []Registry `yaml:"registries,omitempty" json:"registries,omitempty"`
}

// DefaultRegistries is the built-in list: the official registry only.
func DefaultRegistries() []Registry {
	return []Registry{{Name: "official", URL: "https://registry.modelcontextprotocol.io"}}
}

// checkConnectors validates the registry list: every entry needs a name and
// a URL, and the names are unique so a lookup by name is unambiguous.
func (e *ConfigError) checkConnectors(c Config) {
	seen := map[string]bool{}
	for i, r := range c.Connectors.Registries {
		field := fmt.Sprintf("connectors.registries[%d]", i)
		name := strings.TrimSpace(r.Name)
		switch {
		case name == "":
			e.add(field+".name", "name required")
		case seen[name]:
			e.add(field+".name", fmt.Sprintf("duplicate registry name %q", name))
		default:
			seen[name] = true
		}
		if strings.TrimSpace(r.URL) == "" {
			e.add(field+".url", "url required")
		}
	}
}

// addConnectors lists the registry rows for `config show`.
func (l *pathEntryList) addConnectors(cfg *Config) {
	l.add("connectors.registries", renderRegistries(cfg.Connectors.Registries), renderRegistries(DefaultRegistries()))
}

// setConnectorsKey implements SetPath for connectors.registries. The value is
// a JSON array, or a comma-separated "name=url" list. An empty value restores
// the built-in default.
func (c *Config) setConnectorsKey(path, value string) (bool, error) {
	if path != "connectors.registries" {
		return false, nil
	}
	value = strings.TrimSpace(value)
	if value == "" || value == "-" {
		c.Connectors.Registries = DefaultRegistries()
		return true, nil
	}
	regs, err := parseRegistries(value)
	if err != nil {
		return true, err
	}
	c.Connectors.Registries = regs
	return true, nil
}

// parseRegistries reads a JSON array of {name,url}, or a CSV "name=url" list.
func parseRegistries(value string) ([]Registry, error) {
	if strings.HasPrefix(strings.TrimSpace(value), "[") {
		var regs []Registry
		if err := json.Unmarshal([]byte(value), &regs); err != nil {
			return nil, fmt.Errorf("connectors.registries: %w", err)
		}
		return regs, nil
	}
	var out []Registry
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, u, ok := strings.Cut(part, "=")
		if !ok {
			name, u = part, part
		}
		out = append(out, Registry{Name: strings.TrimSpace(name), URL: strings.TrimSpace(u)})
	}
	return out, nil
}

// renderRegistries renders the list as one "name=url" CSV row.
func renderRegistries(regs []Registry) string {
	parts := make([]string, 0, len(regs))
	for _, r := range regs {
		parts = append(parts, r.Name+"="+r.URL)
	}
	return strings.Join(parts, ",")
}
