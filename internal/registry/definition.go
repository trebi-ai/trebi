package registry

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/trebi-ai/trebi/internal/connectors"
)

// trebiMetaKey is the Trebi namespace inside a server.json _meta block.
const trebiMetaKey = "ai.trebi/connector"

// InstallOpts is one install request. Package and Remote are indices into
// ServerJSON.Packages and .Remotes; -1 means "not chosen".
type InstallOpts struct {
	Name    string
	Package int
	Remote  int
	Env     map[string]string
}

// SecretWrite is one secret the install must store before it creates the
// connector.
type SecretWrite struct {
	Name  string
	Value string
}

// Skill is the optional contributed skill block of a Trebi _meta entry. The
// caller fetches the URL; this package never does.
type Skill struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Definition derives the connector definition and the secret writes for one
// install. It performs no I/O.
//
// Precedence: an explicit Package index wins over an explicit Remote index;
// with neither, the first package wins over the first usable remote.
func (e Entry) Definition(o InstallOpts) (connectors.Definition, []SecretWrite, error) {
	name := o.connectorName(e.Server.Name)
	def := connectors.Definition{
		Name:   name,
		Source: &connectors.Source{Registry: e.Registry, Name: e.Server.Name, Version: e.Server.Version},
	}
	pkg, remote, err := e.selectSource(o)
	if err != nil {
		return def, nil, err
	}
	var writes []SecretWrite
	switch {
	case pkg != nil:
		mcp, w, err := e.packageAction(*pkg, name, o.Env)
		if err != nil {
			return def, nil, err
		}
		def.Actions = &connectors.Actions{MCP: mcp}
		writes = w
	case remote != nil:
		def.Actions = &connectors.Actions{MCP: e.remoteAction(*remote)}
	default:
		return def, nil, fmt.Errorf("%w: no installable package or remote", ErrUnsupportedPackage)
	}
	if tb := e.trebiBlock(); tb != nil {
		if raw, ok := tb["events"]; ok {
			var ev connectors.Events
			if err := decodeInto(raw, &ev); err != nil {
				return def, nil, fmt.Errorf("_meta events: %w", err)
			}
			def.Events = &ev
		}
		if raw, ok := tb["channel"]; ok {
			var ch connectors.Channel
			if err := decodeInto(raw, &ch); err != nil {
				return def, nil, fmt.Errorf("_meta channel: %w", err)
			}
			def.Channel = &ch
		}
		if raw, ok := tb["actions"]; ok {
			var meta struct {
				CLI *struct {
					Commands []string `json:"commands"`
					Skill    string   `json:"skill"`
				} `json:"cli"`
			}
			if err := decodeInto(raw, &meta); err != nil {
				return def, nil, fmt.Errorf("_meta actions: %w", err)
			}
			if meta.CLI != nil && len(meta.CLI.Commands) > 0 {
				if def.Actions == nil {
					def.Actions = &connectors.Actions{}
				}
				def.Actions.CLI = &connectors.CLIAction{
					Commands: meta.CLI.Commands,
					Skill:    meta.CLI.Skill,
				}
			}
		}
		if sk, ok := e.Skill(); ok && def.Actions != nil && def.Actions.CLI != nil {
			// The contributed skill takes the name the entry declares, so
			// the install writes it under the same name (Part H.4).
			def.Actions.CLI.Skill = sk.Name
		}
	}
	return def, writes, nil
}

// Skill returns the contributed skill block of the entry, when it declares
// one. It is pure; the caller fetches the URL.
func (e Entry) Skill() (*Skill, bool) {
	tb := e.trebiBlock()
	if tb == nil {
		return nil, false
	}
	raw, ok := tb["skill"]
	if !ok {
		return nil, false
	}
	var sk Skill
	if err := decodeInto(raw, &sk); err != nil {
		return nil, false
	}
	if sk.Name == "" && sk.URL == "" {
		return nil, false
	}
	return &sk, true
}

// trebiBlock finds the ai.trebi/connector block in the server _meta first,
// then in the response-level _meta.
func (e Entry) trebiBlock() map[string]any {
	for _, m := range []map[string]any{e.Server.Meta, e.Meta} {
		if m == nil {
			continue
		}
		if b, ok := m[trebiMetaKey].(map[string]any); ok {
			return b
		}
	}
	return nil
}

// selectSource picks the package or remote to install.
func (e Entry) selectSource(o InstallOpts) (*Package, *Remote, error) {
	if o.Package >= 0 {
		if o.Package >= len(e.Server.Packages) {
			return nil, nil, fmt.Errorf("%w: package index %d out of range", ErrUnsupportedPackage, o.Package)
		}
		p := e.Server.Packages[o.Package]
		return &p, nil, nil
	}
	if o.Remote >= 0 {
		if o.Remote >= len(e.Server.Remotes) {
			return nil, nil, fmt.Errorf("%w: remote index %d out of range", ErrUnsupportedRemote, o.Remote)
		}
		r := e.Server.Remotes[o.Remote]
		if !remoteCapable(r) {
			return nil, nil, fmt.Errorf("%w: %q", ErrUnsupportedRemote, r.Type)
		}
		return nil, &r, nil
	}
	if len(e.Server.Packages) > 0 {
		p := e.Server.Packages[0]
		return &p, nil, nil
	}
	for _, r := range e.Server.Remotes {
		if remoteCapable(r) {
			rr := r
			return nil, &rr, nil
		}
	}
	return nil, nil, fmt.Errorf("%w: no installable package or remote", ErrUnsupportedPackage)
}

// packageAction maps one package onto an MCP stdio action. The command and
// args follow Part H.2; declared env variables become env entries, and a
// secret variable becomes a SecretWrite plus a secret: reference.
func (e Entry) packageAction(p Package, connectorName string, given map[string]string) (*connectors.MCPAction, []SecretWrite, error) {
	if strings.TrimSpace(p.Identifier) == "" {
		return nil, nil, fmt.Errorf("%w: package has no identifier", ErrUnsupportedPackage)
	}
	version := p.Version
	if version == "" {
		version = e.Server.Version
	}
	args := argumentTokens(p.PackageArguments)
	m := &connectors.MCPAction{}
	switch p.RegistryType {
	case "npm":
		m.Command = "npx"
		m.Args = append([]string{"-y", p.Identifier + "@" + version}, args...)
	case "pypi":
		m.Command = "uvx"
		m.Args = append([]string{p.Identifier + "==" + version}, args...)
	case "oci":
		m.Command = "docker"
		m.Args = []string{"run", "-i", "--rm"}
		for _, v := range p.EnvironmentVariables {
			if name := strings.TrimSpace(v.Name); name != "" {
				m.Args = append(m.Args, "-e", name)
			}
		}
		m.Args = append(m.Args, p.Identifier+":"+version)
		m.Args = append(m.Args, args...)
	default:
		return nil, nil, fmt.Errorf("%w: registryType %q", ErrUnsupportedPackage, p.RegistryType)
	}
	env, writes, missing, err := e.resolveEnv(p.EnvironmentVariables, connectorName, given)
	if err != nil {
		return nil, nil, err
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, nil, fmt.Errorf("%w: %s", ErrMissingEnv, strings.Join(missing, ", "))
	}
	m.Env = env
	return m, writes, nil
}

// remoteAction maps one remote transport onto an MCP HTTP action.
func (e Entry) remoteAction(r Remote) *connectors.MCPAction {
	m := &connectors.MCPAction{URL: r.URL}
	headers := map[string]string{}
	for _, h := range r.Headers {
		name := strings.TrimSpace(h.Name)
		if name == "" || h.Value == "" {
			continue
		}
		headers[name] = h.Value
	}
	if len(headers) > 0 {
		m.Headers = headers
	}
	return m
}

// resolveEnv builds the MCP env map, the secret writes, and the list of
// required variables with no value. A caller value wins over the registry
// default. Undeclared caller values are written literally. A declared name
// that is not an env name is ErrInvalidEntry: it becomes part of a secret
// file name.
func (e Entry) resolveEnv(declared []KeyValue, connectorName string, given map[string]string) (map[string]string, []SecretWrite, []string, error) {
	env := map[string]string{}
	var writes []SecretWrite
	var missing []string
	seen := map[string]bool{}
	for _, v := range declared {
		name := strings.TrimSpace(v.Name)
		if name == "" {
			continue
		}
		if !envNameRE.MatchString(name) {
			return nil, nil, nil, fmt.Errorf("%w: env name %q must match %s", ErrInvalidEntry, name, envNameRE)
		}
		seen[name] = true
		value := ""
		if given != nil {
			value = given[name]
		}
		if value == "" {
			value = v.Value
		}
		switch {
		case v.IsSecret && value != "":
			secretName := connectorName + "_" + name
			writes = append(writes, SecretWrite{Name: secretName, Value: value})
			env[name] = "secret:" + secretName
		case v.IsRequired && value == "":
			missing = append(missing, name)
		case value != "":
			env[name] = value
		}
	}
	for k, v := range given {
		if seen[k] || v == "" {
			continue
		}
		env[k] = v
	}
	sort.Slice(writes, func(i, j int) bool { return writes[i].Name < writes[j].Name })
	if len(env) == 0 {
		env = nil
	}
	return env, writes, missing, nil
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// argumentTokens renders package arguments for a command line. A positional
// argument is its value; a named argument is name=value or a bare name.
func argumentTokens(args []Argument) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		switch a.Type {
		case "positional":
			if a.Value != "" {
				out = append(out, a.Value)
			}
		case "named":
			name := strings.TrimSpace(a.Name)
			if name == "" {
				continue
			}
			if a.Value != "" {
				out = append(out, name+"="+a.Value)
			} else {
				out = append(out, name)
			}
		}
	}
	return out
}

// remoteCapable reports whether a remote transport this client can install.
func remoteCapable(r Remote) bool {
	return r.Type == "streamable-http" || r.Type == "sse"
}

// connectorName derives the connector name from the install name, or from the
// server name when none is given. The result matches the connector name rule.
func (o InstallOpts) connectorName(serverName string) string {
	if n := strings.TrimSpace(o.Name); n != "" {
		return n
	}
	base := serverName
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.ToLower(strings.TrimSpace(base))
	base = invalidNameChars.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	if base == "" {
		base = "connector"
	}
	return base
}

var invalidNameChars = regexp.MustCompile(`[^a-z0-9]+`)

// decodeInto re-encodes a decoded JSON value into a typed struct.
func decodeInto(raw any, out any) error {
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
