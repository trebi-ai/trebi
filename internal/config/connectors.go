package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Registry is one MCP registry endpoint. A user may add any registry that
// implements the MCP Registry API, including a private one. The catalog
// queries the list in order.
type Registry struct {
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url" json:"url"`
	Kind string `yaml:"kind,omitempty" json:"kind,omitempty"`
}

// Registry kinds. An empty kind is RegistryMCP.
const (
	RegistryMCP      = "mcp-registry"
	RegistrySmithery = "smithery"
	RegistryDocker   = "docker"
)

// KindOrDefault returns the kind, or RegistryMCP when it is empty.
func (r Registry) KindOrDefault() string {
	if r.Kind == "" {
		return RegistryMCP
	}
	return r.Kind
}

// ConnectorsConfig holds the connector catalog settings. The only key is the
// ordered registry list; an empty list means the built-in default. Kind picks
// the API: the MCP Registry API, Smithery, or the Docker MCP Catalog.
type ConnectorsConfig struct {
	Registries []Registry `yaml:"registries,omitempty" json:"registries,omitempty"`
}

// DefaultRegistries is the built-in list: the official registry only.
func DefaultRegistries() []Registry {
	return []Registry{{Name: "official", URL: "https://registry.modelcontextprotocol.io"}}
}

// checkConnectors validates the registry list: every entry needs a name and
// an http or https URL, and the names are unique so a lookup by name is
// unambiguous.
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
		switch r.KindOrDefault() {
		case RegistryMCP, RegistrySmithery, RegistryDocker:
		default:
			e.add(field+".kind", fmt.Sprintf("%q not in [%s, %s, %s]", r.Kind, RegistryMCP, RegistrySmithery, RegistryDocker))
		}
		raw := strings.TrimSpace(r.URL)
		if raw == "" {
			e.add(field+".url", "url required")
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			e.add(field+".url", fmt.Sprintf("url %q must be http or https with a host", raw))
		}
	}
}

// addConnectors lists the registry rows for `config show`.
func (l *pathEntryList) addConnectors(cfg *Config) {
	l.add("connectors.registries", renderRegistries(cfg.Connectors.Registries), renderRegistries(DefaultRegistries()))
}

// setConnectorsKey implements SetPath for connectors.registries. The value is
// a JSON array, or a "name=url" list split on ";" (or on "," when the value
// has no ";"). An empty value restores
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

// parseRegistries reads a JSON array of {name,url}, or a "name=url" list. A
// ";" separator lets a URL hold a comma.
func parseRegistries(value string) ([]Registry, error) {
	if strings.HasPrefix(strings.TrimSpace(value), "[") {
		var regs []Registry
		if err := json.Unmarshal([]byte(value), &regs); err != nil {
			return nil, fmt.Errorf("connectors.registries: %w", err)
		}
		return regs, nil
	}
	sep := ","
	if strings.Contains(value, ";") {
		sep = ";"
	}
	var out []Registry
	for _, part := range strings.Split(value, sep) {
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

// renderRegistries renders the list as one "name=url" row split on ";", so
// parseRegistries reads it back. A list with a kind renders as JSON.
func renderRegistries(regs []Registry) string {
	for _, r := range regs {
		if r.Kind != "" {
			b, _ := json.Marshal(regs) // []Registry always marshals
			return string(b)
		}
	}
	parts := make([]string, 0, len(regs))
	for _, r := range regs {
		parts = append(parts, r.Name+"="+r.URL)
	}
	return strings.Join(parts, ";")
}
