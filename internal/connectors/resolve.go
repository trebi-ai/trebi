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
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
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

// Resolved is one connection resolved for one workspace.
type Resolved struct {
	ID       string
	Type     string
	Slug     string
	Title    string // the display title: the connection title, else the manifest title
	Version  string
	Track    manifest.Track
	From     string
	Manifest manifest.Manifest
	Inputs   map[string]string
	Dir      string            // the package or manifest folder; "" for inline
	Webhook  *protocol.Webhook // the hosted hook; nil when the connection has none
}

// BinDir is the folder of the installed binary, or "".
func (r Resolved) BinDir() string {
	if r.Dir == "" {
		return ""
	}
	return filepath.Join(r.Dir, "bin")
}

// Folder is the connection folder.
func (r Resolved) Folder() Folder { return NewFolder(r.ID) }

// StateDir is TREBI_STATE_DIR of the connection.
func (r Resolved) StateDir() string { return r.Folder().StateDir() }

// CacheDir is TREBI_CACHE_DIR of the connection.
func (r Resolved) CacheDir() string { return r.Folder().CacheDir() }

// WorkDir is the working folder of the adapter: the manifest folder of a
// file connection, else the state folder.
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

// Env returns the input defaults and values with user secret references
// expanded, the owned secrets from the folder, and the names of missing
// secrets.
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
	env, missing := secrets.ExpandRefs(env)
	if r.ID != "" {
		f := r.Folder()
		for _, name := range f.Secrets() {
			if _, set := r.Inputs[name]; set {
				continue
			}
			if v, err := f.Secret(name); err == nil {
				env[name] = v
			}
		}
	}
	return env, missing
}

// MissingInputs lists the labels and names of the required inputs that have
// no value (D13).
func (r Resolved) MissingInputs() []manifest.Input {
	if r.Manifest.Setup == nil {
		return nil
	}
	env, _ := r.Env()
	var out []manifest.Input
	for _, in := range r.Manifest.Setup.Inputs {
		if in.Required && strings.TrimSpace(env[in.Name]) == "" {
			out = append(out, in)
		}
	}
	return out
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
		return nil, nil, fmt.Errorf("connector %q: the MCP registry reference %s is not installed; install the connector again", r.Slug, m.Name)
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
	srv := &agentwire.MCPServer{Name: r.Slug, Command: m.Command, URL: expand(m.URL)}
	if m.Command != "" {
		env := maps.Clone(vals)
		exp, miss := secrets.ExpandRefs(expandMap(m.Env))
		missing = append(missing, miss...)
		maps.Copy(env, exp)
		// The harness starts the server with the run env, so the daemon
		// cannot clean it; the program applies Trebi mode (D14).
		env["TREBI_STATE_DIR"] = r.StateDir()
		env["TREBI_CACHE_DIR"] = r.CacheDir()
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

// fingerprint changes when anything that the adapter process sees changes,
// an owned secret included.
func (r Resolved) fingerprint() string {
	env, _ := r.Env()
	b, _ := json.Marshal(struct {
		M    manifest.Manifest
		Env  map[string]string
		Dir  string
		Hook *protocol.Webhook
	}{r.Manifest, env, r.Dir, r.Webhook})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Resolve applies the catalog, connection, and workspace layers of one
// connection. ref is a slug or an id. An empty workspaceID resolves the
// connection alone. The workspace layer can only turn the connection off or
// patch its actions and skill.
func (s *Service) Resolve(ref, workspaceID string) (Resolved, error) {
	c, err := s.Lookup(ref)
	if err != nil {
		return Resolved{}, err
	}
	var ov *workspaces.ConnectorOverride
	if workspaceID != "" {
		s.mu.Lock()
		if o, found := s.overrides[workspaceID][c.Slug]; found {
			ov = &o
		}
		s.mu.Unlock()
	}
	return resolveConnection(c, workspaceID, ov)
}

func resolveConnection(c Connection, workspaceID string, ov *workspaces.ConnectorOverride) (Resolved, error) {
	if ov != nil && ov.Disabled {
		return Resolved{}, fmt.Errorf("%w: %q in %s", ErrDisabled, c.Slug, workspaceID)
	}
	base, dir, err := loadBase(c)
	if err != nil {
		return Resolved{}, err
	}
	m, err := applyPatch(base, c.Patch, c.Slug, LayerGlobal)
	if err != nil {
		return Resolved{}, err
	}
	if ov != nil {
		if err := ov.Validate(); err != nil {
			return Resolved{}, &ValidationError{Name: c.Slug, Layer: LayerWorkspace, Fields: []FieldError{{Field: "patch", Message: err.Error()}}}
		}
		if m, err = applyPatch(m, ov.Patch, c.Slug, LayerWorkspace); err != nil {
			return Resolved{}, err
		}
	}
	m.Name = c.Type
	if err := m.Validate(); err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			ve.Name = c.Slug
			ve.Layer = LayerGlobal
			if ov != nil {
				ve.Layer = LayerWorkspace
			}
		}
		return Resolved{}, err
	}
	m = m.WithDefaults()
	r := Resolved{
		ID: c.ID, Type: c.Type, Slug: c.Slug, Title: c.DisplayTitle(m.Title),
		Version: m.Version, Track: c.track(), From: c.From,
		Manifest: m, Inputs: maps.Clone(c.Inputs), Dir: dir,
	}
	if c.From == FromCatalog {
		r.Version = c.Version
	}
	return r, nil
}

// loadBase reads the manifest of the connection source.
func loadBase(c Connection) (manifest.Manifest, string, error) {
	switch c.From {
	case FromInline:
		if c.Manifest == nil {
			return manifest.Manifest{}, "", &ValidationError{Name: c.Slug, Layer: LayerGlobal,
				Fields: []FieldError{{Field: "manifest", Message: "required for from: inline"}}}
		}
		m := c.Manifest.Clone()
		m.Name = c.Type
		return m, "", nil
	case FromCatalog:
		dir := home.PackageDir(c.Type, c.Version)
		m, err := readManifest(dir)
		if errors.Is(err, os.ErrNotExist) {
			return manifest.Manifest{}, "", fmt.Errorf("%w: %s %s", ErrNotInstalled, c.Type, c.Version)
		}
		if err != nil {
			return manifest.Manifest{}, "", withLayer(err, c.Slug, LayerCatalog)
		}
		return m, dir, nil
	default:
		path := c.FilePath()
		m, err := readManifest(filepath.Dir(path))
		if err != nil {
			return manifest.Manifest{}, "", withLayer(err, c.Slug, LayerGlobal)
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

// Instances resolves every connection with an events block: one process
// each. A connection that does not resolve is left out; List shows its error.
func (s *Service) Instances() []Resolved {
	var out []Resolved
	for _, c := range s.snapshot() {
		r, err := resolveConnection(c, "", nil)
		if err != nil || r.Manifest.Events == nil {
			continue
		}
		if spec := r.Manifest.SubscriptionSpec(); s.hooks != nil && spec != nil && spec.Webhook != nil && len(c.Subscriptions) > 0 {
			r.Webhook = s.hooks.Hook(r.ID)
		}
		out = append(out, r)
	}
	return out
}

// disabledIn reports whether a workspace turned a connection off.
func (s *Service) disabledIn(workspaceID, slug string) bool {
	if workspaceID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overrides[workspaceID][slug].Disabled
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
