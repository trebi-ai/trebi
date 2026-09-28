package connectors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

var (
	// ErrNotInstalled is a catalog entry whose version folder is missing.
	ErrNotInstalled = errors.New("connector version is not installed")
	// ErrDisabled is a connector that the workspace turned off.
	ErrDisabled = errors.New("connector is disabled in this workspace")
)

// Layer names of a resolve error.
const (
	LayerCatalog   = "catalog"
	LayerGlobal    = "global"
	LayerWorkspace = "workspace"
)

// Resolved is one connector resolved for one workspace (Part C.3).
type Resolved struct {
	Name      string
	Instance  string
	Workspace string // set only for a workspace instance or override
	Version   string
	Track     manifest.Track
	From      string
	Manifest  manifest.Manifest
	Inputs    map[string]string
	Dir       string // the snapshot or manifest folder; "" for inline
}

// BinDir is the folder of the installed binary, or "".
func (r Resolved) BinDir() string {
	if r.Dir == "" {
		return ""
	}
	return filepath.Join(r.Dir, "bin")
}

// StateDir is the adapter state folder of the instance.
func (r Resolved) StateDir() string { return home.ConnectorStateDir(r.Instance) }

// WorkDir is the working folder of the adapter: the manifest folder of a
// file entry, else the state folder.
func (r Resolved) WorkDir() string {
	if strings.HasPrefix(r.From, fromFile) && r.Dir != "" {
		return r.Dir
	}
	return r.StateDir()
}

// SkillDir is the skill folder of the manifest, or "" when it has none.
func (r Resolved) SkillDir() string {
	if r.Dir == "" {
		return ""
	}
	dir := filepath.Join(r.Dir, r.Manifest.SkillPath())
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		return ""
	}
	return dir
}

// Env returns the input defaults and values with secret references expanded,
// and the names of missing secrets.
func (r Resolved) Env() (map[string]string, []string) {
	env := map[string]string{}
	if r.Manifest.Setup != nil {
		for _, in := range r.Manifest.Setup.Inputs {
			if in.Default != "" {
				env[in.Name] = in.Default
			}
		}
	}
	maps.Copy(env, r.Inputs)
	return secrets.ExpandRefs(env)
}

// PathEnv prepends the bin folder to path.
func (r Resolved) PathEnv(path string) string {
	if bin := r.BinDir(); bin != "" {
		if path == "" {
			return bin
		}
		return bin + string(os.PathListSeparator) + path
	}
	return path
}

// placeholderRE is a ${NAME} input reference in an MCP field.
var placeholderRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// MCPServer builds the MCP server of the actions block. ${NAME} in args, env,
// url, and headers expands from the inputs, and every input is also in the
// server env. It returns nil with no actions.mcp block.
func (r Resolved) MCPServer() (*agentwire.MCPServer, []string, error) {
	if r.Manifest.Actions == nil || r.Manifest.Actions.MCP == nil {
		return nil, nil, nil
	}
	m := r.Manifest.Actions.MCP
	if m.Command == "" && m.URL == "" {
		return nil, nil, fmt.Errorf("connector %q: the MCP registry reference %s is not installed; install the connector again", r.Name, m.Name)
	}
	vals, missing := r.Env()
	expand := func(s string) string {
		return placeholderRE.ReplaceAllStringFunc(s, func(ph string) string {
			return vals[ph[2:len(ph)-1]]
		})
	}
	expandMap := func(in map[string]string) map[string]string {
		out := make(map[string]string, len(in))
		for k, v := range in {
			out[k] = expand(v)
		}
		return out
	}
	srv := &agentwire.MCPServer{Name: r.Name, Command: m.Command, URL: expand(m.URL)}
	if m.Command != "" {
		env := maps.Clone(vals)
		exp, miss := secrets.ExpandRefs(expandMap(m.Env))
		missing = append(missing, miss...)
		maps.Copy(env, exp)
		if bin := r.BinDir(); bin != "" {
			env["PATH"] = r.PathEnv(os.Getenv("PATH"))
		}
		srv.Env = env
		for _, a := range m.Args {
			srv.Args = append(srv.Args, expand(a))
		}
	}
	if len(m.Headers) > 0 {
		h, miss := secrets.ExpandRefs(expandMap(m.Headers))
		missing = append(missing, miss...)
		srv.Headers = h
	}
	return srv, missing, nil
}

// fingerprint changes when anything that the adapter process sees changes.
func (r Resolved) fingerprint() string {
	b, _ := json.Marshal(struct {
		M      manifest.Manifest
		Inputs map[string]string
		Dir    string
	}{r.Manifest, r.Inputs, r.Dir})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// splitInstance splits "name@ws" into the name and the workspace.
func splitInstance(instance string) (name, workspace string) {
	name, workspace, _ = strings.Cut(instance, "@")
	return name, workspace
}

// Resolve applies the catalog, global, and workspace layers of one connector
// (Part C.3). An empty workspaceID resolves the global instance.
func (s *Service) Resolve(name, workspaceID string) (Resolved, error) {
	s.mu.Lock()
	e, ok := s.entries[name]
	var ov *workspaces.ConnectorOverride
	if workspaceID != "" {
		if o, found := s.overrides[workspaceID][name]; found {
			ov = &o
		}
	}
	s.mu.Unlock()
	if !ok {
		return Resolved{}, fmt.Errorf("connector %q not found: %w", name, ErrNotFound)
	}
	return resolveEntry(e.clone(), workspaceID, ov)
}

func resolveEntry(e Entry, workspaceID string, ov *workspaces.ConnectorOverride) (Resolved, error) {
	if ov != nil && ov.Disabled {
		return Resolved{}, fmt.Errorf("%w: %q in %s", ErrDisabled, e.Name, workspaceID)
	}
	version := e.Version
	if ov != nil && ov.Version != "" {
		if e.From != FromCatalog && ov.Version != version {
			return Resolved{}, &ValidationError{Name: e.Name, Layer: LayerWorkspace,
				Fields: []FieldError{{Field: "version", Message: "a version override needs from: catalog"}}}
		}
		version = ov.Version
	}
	base, dir, err := loadBase(e, version)
	if err != nil {
		return Resolved{}, err
	}
	m, err := applyPatch(base, e.Patch, e.Name, LayerGlobal)
	if err != nil {
		return Resolved{}, err
	}
	inputs := maps.Clone(e.Inputs)
	track := e.track()
	if ov != nil {
		if m, err = applyPatch(m, ov.Patch, e.Name, LayerWorkspace); err != nil {
			return Resolved{}, err
		}
		if inputs == nil {
			inputs = map[string]string{}
		}
		maps.Copy(inputs, ov.Inputs)
		if ov.Track != "" {
			track = manifest.Track(ov.Track)
		}
	}
	m.Name = e.Name
	if err := m.Validate(); err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			ve.Layer = LayerGlobal
			if ov != nil {
				ve.Layer = LayerWorkspace
			}
		}
		return Resolved{}, err
	}
	m = m.WithDefaults()
	r := Resolved{
		Name: e.Name, Instance: e.Name, Version: m.Version, Track: track, From: e.From,
		Manifest: m, Inputs: inputs, Dir: dir,
	}
	if e.From == FromCatalog {
		r.Version = version
	}
	if ov != nil {
		r.Workspace = workspaceID
		if ownInstance(ov, m, e.Version) {
			r.Instance = e.Name + "@" + workspaceID
		}
	}
	return r, nil
}

// ownInstance reports whether a workspace override makes its own process: it
// touches a connector with events beyond its actions and skill.
func ownInstance(ov *workspaces.ConnectorOverride, m manifest.Manifest, globalVersion string) bool {
	if m.Events == nil {
		return false
	}
	if len(ov.Inputs) > 0 || (ov.Version != "" && ov.Version != globalVersion) {
		return true
	}
	for k := range ov.Patch {
		if k != "actions" && k != "skill" {
			return true
		}
	}
	return false
}

// loadBase reads the manifest of the entry source at one version.
func loadBase(e Entry, version string) (manifest.Manifest, string, error) {
	switch e.From {
	case FromInline:
		if e.Manifest == nil {
			return manifest.Manifest{}, "", &ValidationError{Name: e.Name, Layer: LayerGlobal,
				Fields: []FieldError{{Field: "manifest", Message: "required for from: inline"}}}
		}
		m := e.Manifest.Clone()
		m.Name = e.Name
		return m, "", nil
	case FromCatalog:
		dir := home.ConnectorVersionDir(e.Name, version)
		m, err := readManifest(dir)
		if errors.Is(err, os.ErrNotExist) {
			return manifest.Manifest{}, "", fmt.Errorf("%w: %s %s", ErrNotInstalled, e.Name, version)
		}
		if err != nil {
			return manifest.Manifest{}, "", withLayer(err, e.Name, LayerCatalog)
		}
		return m, dir, nil
	default:
		path := e.FilePath()
		m, err := readManifest(filepath.Dir(path))
		if err != nil {
			return manifest.Manifest{}, "", withLayer(err, e.Name, LayerGlobal)
		}
		return m, filepath.Dir(path), nil
	}
}

// readManifest parses dir/trebi-connector.yaml without the rule check; the
// resolved result is checked after the patches.
func readManifest(dir string) (manifest.Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err != nil {
		return manifest.Manifest{}, err
	}
	return manifest.Parse(data)
}

func withLayer(err error, name, layer string) error {
	var ve *ValidationError
	if errors.As(err, &ve) {
		ve.Layer = layer
		return ve
	}
	return &ValidationError{Name: name, Layer: layer, Fields: []FieldError{{Message: err.Error()}}}
}

// applyPatch applies one layer patch.
func applyPatch(m manifest.Manifest, patch map[string]any, name, layer string) (manifest.Manifest, error) {
	if len(patch) == 0 {
		return m, nil
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return m, err
	}
	out, err := manifest.Patch(m, raw)
	if err != nil {
		return m, &ValidationError{Name: name, Layer: layer, Fields: []FieldError{{Field: "patch", Message: err.Error()}}}
	}
	return out, nil
}

// Instances resolves every instance the daemon must run: the global instance
// of each connector with events, plus each workspace instance. A connector
// that does not resolve is left out; List shows its error.
func (s *Service) Instances() []Resolved {
	s.mu.Lock()
	names := sortedNames(s.entries)
	wsIDs := sortedNames(s.overrides)
	s.mu.Unlock()
	var out []Resolved
	for _, n := range names {
		r, err := s.Resolve(n, "")
		if err != nil || r.Manifest.Events == nil {
			continue
		}
		out = append(out, r)
	}
	for _, ws := range wsIDs {
		s.mu.Lock()
		ovNames := sortedNames(s.overrides[ws])
		s.mu.Unlock()
		for _, n := range ovNames {
			r, err := s.Resolve(n, ws)
			if err != nil || r.Instance == n {
				continue
			}
			out = append(out, r)
		}
	}
	return out
}

// InstanceFor returns the instance that serves a connector in a workspace:
// its own instance, or the global one. It is "" when the workspace turned the
// connector off.
func (s *Service) InstanceFor(name, workspaceID string) string {
	if workspaceID == "" {
		return name
	}
	s.mu.Lock()
	ov, ok := s.overrides[workspaceID][name]
	_, managed := s.entries[name]
	s.mu.Unlock()
	if !ok || !managed {
		return name
	}
	if ov.Disabled {
		return ""
	}
	r, err := s.Resolve(name, workspaceID)
	if err != nil {
		return name
	}
	return r.Instance
}

// resolveInstance resolves an instance key: "name" or "name@ws".
func (s *Service) resolveInstance(instance string) (Resolved, error) {
	name, ws := splitInstance(instance)
	r, err := s.Resolve(name, ws)
	if err != nil {
		return r, err
	}
	if r.Instance != instance {
		return r, fmt.Errorf("connector instance %q not found: %w", instance, ErrNotFound)
	}
	return r, nil
}

// loadOverrides reads the workspace layers of every live workspace.
func (s *Service) loadOverrides(ctx context.Context) map[string]map[string]workspaces.ConnectorOverride {
	if s.ws == nil {
		return map[string]map[string]workspaces.ConnectorOverride{}
	}
	ov, err := s.ws.AllConnectorOverrides(ctx)
	if err != nil {
		s.log.Warn("connector overrides", "error", err)
		return map[string]map[string]workspaces.ConnectorOverride{}
	}
	return ov
}
