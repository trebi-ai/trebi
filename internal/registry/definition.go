package registry

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/secrets"
)

// trebiMetaKey is the Trebi namespace inside a server.json _meta block.
const trebiMetaKey = "ai.trebi/connector"

// publisherMetaKey is the only server _meta key the official registry keeps.
// It drops every other key when a server is published.
const publisherMetaKey = "io.modelcontextprotocol.registry/publisher-provided"

// secretPrefix marks a value that names a stored secret.
const secretPrefix = "secret:"

// InstallOpts is one install request. Package and Remote are indices into
// ServerJSON.Packages and .Remotes; -1 means "not chosen". Env holds the
// caller values, keyed by env name, header name, argument name or value
// hint, or template variable name.
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
	r := newInputResolver(name, o.Env)
	var mcp *connectors.MCPAction
	switch {
	case pkg != nil:
		mcp, err = e.packageAction(*pkg, r)
	case remote != nil:
		mcp, err = e.remoteAction(*remote, r)
	default:
		err = fmt.Errorf("%w: no installable package or remote", ErrUnsupportedPackage)
	}
	if err != nil {
		return def, nil, err
	}
	def.Actions = &connectors.Actions{MCP: mcp}
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
				def.Actions.CLI = &connectors.CLIAction{
					Commands: meta.CLI.Commands,
					Skill:    meta.CLI.Skill,
				}
			}
		}
		if sk, ok := e.Skill(); ok && def.Actions.CLI != nil {
			// The contributed skill takes the name the entry declares, so
			// the install writes it under the same name (Part H.4).
			def.Actions.CLI.Skill = sk.Name
		}
	}
	return def, r.writes, nil
}

// Skill returns the contributed skill block of the entry. It is ok only when
// the block sets both name and url. It is pure; the caller fetches the URL.
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
	if strings.TrimSpace(sk.Name) == "" || strings.TrimSpace(sk.URL) == "" {
		return nil, false
	}
	return &sk, true
}

// trebiBlock finds the ai.trebi/connector block. The official registry keeps
// only the publisher-provided key of the server _meta, so the block can be
// there or at the top of the server _meta (a private registry). The
// response-level _meta is the last place.
func (e Entry) trebiBlock() map[string]any {
	var places []map[string]any
	if m := e.Server.Meta; m != nil {
		places = append(places, m)
		if pub, ok := m[publisherMetaKey].(map[string]any); ok {
			places = append(places, pub)
		}
	}
	if e.Meta != nil {
		places = append(places, e.Meta)
	}
	for _, m := range places {
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

// packageAction maps one stdio package onto an MCP stdio action. A set
// runtimeHint is the command, and runtimeArguments go before the package.
func (e Entry) packageAction(p Package, r *inputResolver) (*connectors.MCPAction, error) {
	id := strings.TrimSpace(p.Identifier)
	if id == "" {
		return nil, fmt.Errorf("%w: package has no identifier", ErrUnsupportedPackage)
	}
	if p.Transport != nil && p.Transport.Type != "" && p.Transport.Type != "stdio" {
		return nil, fmt.Errorf("%w: transport %q", ErrUnsupportedPackage, p.Transport.Type)
	}
	version := p.Version
	if version == "" {
		version = e.Server.Version
	}
	var command, ref string
	switch p.RegistryType {
	case "npm":
		command, ref = "npx", id
		if version != "" {
			ref = id + "@" + version
		}
	case "pypi":
		command, ref = "uvx", id
		if version != "" {
			ref = id + "==" + version
		}
	case "oci":
		command, ref = "docker", ociRef(id, version)
	default:
		return nil, fmt.Errorf("%w: registryType %q", ErrUnsupportedPackage, p.RegistryType)
	}
	if hint := strings.TrimSpace(p.RuntimeHint); hint != "" {
		if !commandRE.MatchString(hint) {
			return nil, fmt.Errorf("%w: runtimeHint %q must match %s", ErrInvalidEntry, hint, commandRE)
		}
		command = hint
	}
	runtimeArgs, err := r.arguments(p.RuntimeArguments)
	if err != nil {
		return nil, err
	}
	pkgArgs, err := r.arguments(p.PackageArguments)
	if err != nil {
		return nil, err
	}
	env, err := r.keyValues(p.EnvironmentVariables, envNameRE)
	if err != nil {
		return nil, err
	}
	r.extraEnv(env)
	if err := r.done(); err != nil {
		return nil, err
	}
	var args []string
	switch {
	case command == "npx":
		args = append([]string{"-y"}, runtimeArgs...)
		args = append(args, ref)
	case command == "uvx" && p.RegistryType == "pypi":
		// uv runs a pinned tool with --from: docs.astral.sh/uv/guides/tools.
		args = runtimeArgs
		if ref != id {
			args = append(args, "--from", ref)
		}
		args = append(args, id)
	case command == "docker":
		args = append([]string{"run", "-i", "--rm"}, runtimeArgs...)
		for _, k := range slices.Sorted(maps.Keys(env)) {
			args = append(args, "-e", k)
		}
		args = append(args, ref)
	default:
		args = append(runtimeArgs, ref)
	}
	m := &connectors.MCPAction{Command: command, Args: append(args, pkgArgs...)}
	if len(env) > 0 {
		m.Env = env
	}
	return m, nil
}

// ociRef is the image reference. The current schema puts the tag or digest
// in the identifier; an older entry puts the tag in version.
func ociRef(id, version string) string {
	last := id[strings.LastIndex(id, "/")+1:]
	if strings.Contains(last, ":") || strings.Contains(id, "@") || version == "" {
		return id
	}
	return id + ":" + version
}

// remoteAction maps one remote transport onto an MCP HTTP action. URL
// variables and header inputs are filled from the caller values.
func (e Entry) remoteAction(rm Remote, r *inputResolver) (*connectors.MCPAction, error) {
	u, secret, err := r.value("", Input{Value: rm.URL, Variables: rm.Variables})
	if err != nil {
		return nil, err
	}
	if secret {
		return nil, fmt.Errorf("%w: remote url takes a secret input", ErrInvalidEntry)
	}
	headers, err := r.keyValues(rm.Headers, headerNameRE)
	if err != nil {
		return nil, err
	}
	if err := r.done(); err != nil {
		return nil, err
	}
	if err := CheckURL(u); err != nil {
		return nil, fmt.Errorf("%w: remote %s", err, u)
	}
	m := &connectors.MCPAction{URL: u}
	if len(headers) > 0 {
		m.Headers = headers
	}
	return m, nil
}

// InputSpec is one value that an install of one source takes. A client form
// shows one field per spec and sends the value in env under Name.
type InputSpec struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"` // env | header | argument | variable
	Description string   `json:"description,omitempty"`
	Format      string   `json:"format,omitempty"`
	Default     string   `json:"default,omitempty"`
	Choices     []string `json:"choices,omitempty"`
	Required    bool     `json:"required"`
	Secret      bool     `json:"secret"`
}

// Inputs lists the inputs of each package and each remote, in index order.
// An env variable is always an input. A header or argument is an input when
// the registry sets no value for it. Each template variable is an input.
func (e Entry) Inputs() (packages, remotes [][]InputSpec) {
	for _, p := range e.Server.Packages {
		l := inputList{specs: []InputSpec{}}
		for _, a := range append(slices.Clone(p.RuntimeArguments), p.PackageArguments...) {
			key := a.ValueHint
			if a.Type == "named" {
				key = a.Name
			}
			in := a.Input
			if a.Type == "positional" && in.Value == "" && key != "" {
				in.IsRequired = true
			}
			l.add(key, "argument", in, in.Value == "")
		}
		for _, kv := range p.EnvironmentVariables {
			l.add(kv.Name, "env", kv.Input, true)
		}
		packages = append(packages, l.specs)
	}
	for _, rm := range e.Server.Remotes {
		l := inputList{specs: []InputSpec{}}
		l.add("", "url", Input{Value: rm.URL, Variables: rm.Variables}, false)
		for _, kv := range rm.Headers {
			l.add(kv.Name, "header", kv.Input, kv.Value == "")
		}
		remotes = append(remotes, l.specs)
	}
	return packages, remotes
}

// inputList collects the input specs of one source, one spec per name.
type inputList struct {
	specs []InputSpec
	seen  map[string]bool
}

// add lists the input itself when self is set, then each variable that its
// value template uses.
func (l *inputList) add(name, kind string, in Input, self bool) {
	if self {
		l.put(name, kind, in)
	}
	for _, ph := range placeholderRE.FindAllString(in.Value+" "+in.Default, -1) {
		v := ph[1 : len(ph)-1]
		if vin, ok := in.Variables[v]; ok {
			l.put(v, "variable", vin)
		}
	}
}

func (l *inputList) put(name, kind string, in Input) {
	name = strings.TrimSpace(name)
	if name == "" || l.seen[name] {
		return
	}
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	l.seen[name] = true
	l.specs = append(l.specs, InputSpec{
		Name: name, Kind: kind, Description: in.Description, Format: in.Format,
		Default: in.Default, Choices: in.Choices, Required: in.IsRequired, Secret: in.IsSecret,
	})
}

// inputResolver fills registry inputs from the caller values. It collects the
// secret writes, the required inputs with no value, and the caller keys that
// an input used.
type inputResolver struct {
	connector string
	given     map[string]string
	used      map[string]bool
	writes    []SecretWrite
	missing   []string
}

func newInputResolver(connector string, given map[string]string) *inputResolver {
	return &inputResolver{connector: connector, given: given, used: map[string]bool{}}
}

// value resolves one input. A caller value under key wins; else the input
// value or its default, with each {name} placeholder filled from Variables.
// secret reports that a secret input went into the result.
func (r *inputResolver) value(key string, in Input) (string, bool, error) {
	if key != "" {
		if g, ok := r.given[key]; ok {
			r.used[key] = true
			if g != "" {
				return g, in.IsSecret, nil
			}
		}
	}
	v := in.Value
	if v == "" {
		v = in.Default
	}
	secret := in.IsSecret
	tmpl := v
	var ferr error
	v = placeholderRE.ReplaceAllStringFunc(tmpl, func(ph string) string {
		name := ph[1 : len(ph)-1]
		vin, ok := in.Variables[name]
		if !ok {
			return ph // the schema keeps an unknown placeholder as it is
		}
		sub, sec, err := r.value(name, Input{Value: vin.Value, Default: vin.Default, IsRequired: vin.IsRequired, IsSecret: vin.IsSecret})
		if err != nil && ferr == nil {
			ferr = err
		}
		if strings.HasPrefix(sub, secretPrefix) && tmpl != ph && ferr == nil {
			ferr = fmt.Errorf("%w: %s: a secret reference cannot go inside a template; give the full value under the outer name", ErrInvalidInput, name)
		}
		secret = secret || sec
		return sub
	})
	if ferr != nil {
		return "", false, ferr
	}
	if v == "" && in.IsRequired && key != "" {
		r.missing = append(r.missing, key)
	}
	return v, secret, nil
}

// keyValues resolves env variables or headers into a map. nameRE checks each
// declared name. A secret value is written to the store and replaced by a
// secret: reference. A given secret: reference is kept as it is.
func (r *inputResolver) keyValues(kvs []KeyValue, nameRE *regexp.Regexp) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range kvs {
		name := strings.TrimSpace(kv.Name)
		if name == "" {
			continue
		}
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("%w: name %q must match %s", ErrInvalidEntry, name, nameRE)
		}
		v, secret, err := r.value(name, kv.Input)
		if err != nil {
			return nil, err
		}
		if v == "" {
			continue
		}
		if secret && !strings.HasPrefix(v, secretPrefix) {
			secretName := r.connector + "_" + name
			if err := secrets.CheckName(secretName); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidEntry, err)
			}
			r.writes = append(r.writes, SecretWrite{Name: secretName, Value: v})
			v = secretPrefix + secretName
		}
		out[name] = v
	}
	sort.Slice(r.writes, func(i, j int) bool { return r.writes[i].Name < r.writes[j].Name })
	return out, nil
}

// arguments renders package or runtime arguments for a command line. A
// positional argument is its value; a named argument is --name=value, or
// "-n value" for a short flag. An optional named argument with no value is a
// bare flag only when it has no value format other than boolean.
func (r *inputResolver) arguments(args []Argument) ([]string, error) {
	out := make([]string, 0, len(args))
	for _, a := range args {
		key := strings.TrimSpace(a.ValueHint)
		name := strings.TrimSpace(a.Name)
		if a.Type == "named" {
			key = name
		}
		if a.Type == "positional" && a.Value == "" && key != "" {
			a.IsRequired = true // a value hint alone asks the user for the value
		}
		v, secret, err := r.value(key, a.Input)
		if err != nil {
			return nil, err
		}
		if secret || strings.HasPrefix(v, secretPrefix) {
			return nil, fmt.Errorf("%w: argument %q takes a secret; a command line cannot hold one", ErrInvalidEntry, key)
		}
		switch a.Type {
		case "positional":
			if v != "" {
				out = append(out, v)
			}
		case "named":
			switch {
			case name == "":
			case v != "" && strings.HasPrefix(name, "--"):
				out = append(out, name+"="+v)
			case v != "":
				out = append(out, name, v)
			case !a.IsRequired && (a.Format == "" || a.Format == "boolean"):
				out = append(out, name)
			}
		}
	}
	return out, nil
}

// extraEnv adds each caller value that no input used and that is an env name
// to env. Any other unused key stays unused, and done reports it.
func (r *inputResolver) extraEnv(env map[string]string) {
	for k, v := range r.given {
		if r.used[k] || v == "" || !envNameRE.MatchString(k) {
			continue
		}
		r.used[k] = true
		env[k] = v
	}
}

// done reports the required inputs with no value, then the caller keys that
// no input used.
func (r *inputResolver) done() error {
	if len(r.missing) > 0 {
		missing := slices.Compact(slices.Sorted(slices.Values(r.missing)))
		return fmt.Errorf("%w: %s", ErrMissingInput, strings.Join(missing, ", "))
	}
	var unused []string
	for k := range r.given {
		if !r.used[k] {
			unused = append(unused, k)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		return fmt.Errorf("%w: %s", ErrUnusedInput, strings.Join(unused, ", "))
	}
	return nil
}

var (
	envNameRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	headerNameRE  = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")
	commandRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	placeholderRE = regexp.MustCompile(`\{[^{}\s]+\}`)
)

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
